package actions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kube"
	"github.com/jurrebuunk/clusterd/internal/kubefake"
)

type harness struct {
	f        *kubefake.Kube
	e        *Engine
	mu       sync.Mutex
	commands []string
	restart  func() error // what "systemctl restart" does
}

func newHarness(t *testing.T) *harness {
	h := &harness{f: kubefake.New()}
	cfg := &config.Config{Host: "hv1"}
	for _, vm := range []string{"m1", "m2", "m3", "w1"} {
		cfg.VMs = append(cfg.VMs, config.VM{Name: vm, Host: "hv1", Enabled: true})
	}
	h.restart = func() error {
		// The VM comes back with a new boot after a moment.
		go func() {
			time.Sleep(20 * time.Millisecond)
			h.f.Mu.Lock()
			h.f.NodeMap["w1"].BootID = "boot-2"
			h.f.NodeMap["w1"].Ready = true
			h.f.Mu.Unlock()
		}()
		return nil
	}
	h.e = &Engine{
		Cfg:  cfg,
		Kube: h.f,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Timing: Timing{Poll: 5 * time.Millisecond, DrainTimeout: 300 * time.Millisecond,
			RestartTimeout: time.Second, BootTimeout: 300 * time.Millisecond, LockTTL: 2 * time.Second, Total: 5 * time.Second},
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			h.mu.Lock()
			h.commands = append(h.commands, name+" "+strings.Join(args, " "))
			h.mu.Unlock()
			return nil, h.restart()
		},
	}
	return h
}

func collect(r *Run) []api.ActionEvent {
	var out []api.ActionEvent
	for ev := range r.Events() {
		out = append(out, ev)
	}
	return out
}

func messages(evs []api.ActionEvent) string {
	var m []string
	for _, e := range evs {
		m = append(m, e.Status+": "+e.Message)
	}
	return strings.Join(m, "\n")
}

var who = Caller{Token: "tester", Scope: "operator", RequestID: "r1"}

func TestRestartHappyPath(t *testing.T) {
	h := newHarness(t)
	h.f.PDBBlock["web-2"] = 2 // a disruption budget says no twice
	plan, _, err := h.e.PlanVMRestart(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Evict) != 2 || plan.Skipped != 1 || len(plan.Steps) != 5 || plan.Host != "hv1" {
		t.Fatalf("plan = %+v", plan)
	}
	r, err := h.e.StartVMRestart(context.Background(), "w1", who)
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(r)
	if err := r.Wait(); err != nil {
		t.Fatalf("restart failed: %v\n%s", err, messages(evs))
	}
	got := messages(evs)
	for _, want := range []string{"node/w1 cordoned", "evicting pod apps/web-1", "evicting pod apps/web-2", "node/w1 drained",
		"microvm@w1 restarted on hv1", "node/w1 Ready (new boot)", "node/w1 uncordoned", "Kubernetes API ready", "done: vm restart w1 done"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !h.f.NodeMap["w1"].Schedulable {
		t.Error("node left cordoned")
	}
	if h.f.LeaseObj != nil {
		t.Error("lock not released")
	}
	if len(h.commands) != 1 || h.commands[0] != "systemctl --no-ask-password restart microvm@w1.service" {
		t.Errorf("commands = %v", h.commands)
	}
}

func TestBusy(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	h.restart = func() error { <-block; return errors.New("stopped") }
	r, err := h.e.StartVMRestart(context.Background(), "w1", who)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.e.StartVMRestart(context.Background(), "m1", Caller{Token: "other"})
	var be *BusyError
	if !errors.As(err, &be) || !strings.Contains(be.Holder, "vm restart w1 by tester") {
		t.Fatalf("second restart: %v", err)
	}
	plan, _, _ := h.e.PlanVMRestart(context.Background(), "m1")
	if plan.LockHolder == "" {
		t.Error("dry run does not show the lock holder")
	}
	close(block)
	collect(r)
	if err := r.Wait(); err == nil || !strings.Contains(err.Error(), "left cordoned") {
		t.Fatalf("failed restart: %v", err)
	}
	if h.f.NodeMap["w1"].Schedulable {
		t.Error("a node whose VM failed to restart must stay cordoned")
	}
	if h.f.LeaseObj != nil {
		t.Error("lock not released after a failure")
	}
}

func TestExpiredLockIsTakenOver(t *testing.T) {
	h := newHarness(t)
	l := &kube.Lease{}
	l.Metadata.Name, l.Metadata.Namespace = lockName, lockNamespace
	l.Spec.HolderIdentity, l.Spec.LeaseDurationSeconds = "crashed instance", 60
	l.Spec.RenewTime = kube.MicroTime{Time: time.Now().Add(-2 * time.Minute)}
	h.f.CreateLease(context.Background(), l)
	r, err := h.e.StartVMRestart(context.Background(), "w1", who)
	if err != nil {
		t.Fatalf("expired lock not taken over: %v", err)
	}
	collect(r)
	if err := r.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestPreconditions(t *testing.T) {
	cases := map[string]struct {
		setup func(f *kubefake.Kube, e *Engine)
		vm    string
		want  error
	}{
		"unknown vm":   {func(f *kubefake.Kube, e *Engine) {}, "nope", ErrNotFound},
		"other down":   {func(f *kubefake.Kube, e *Engine) { f.NodeMap["m2"].Ready = false }, "w1", ErrPrecondition},
		"node missing": {func(f *kubefake.Kube, e *Engine) { delete(f.NodeMap, "m3") }, "w1", ErrPrecondition},
		"disabled":     {func(f *kubefake.Kube, e *Engine) { e.Cfg.VMs[3].Enabled = false }, "w1", ErrPrecondition},
		"bare pod": {func(f *kubefake.Kube, e *Engine) {
			f.PodMap["w1"] = append(f.PodMap["w1"], kube.Pod{Namespace: "x", Name: "debug", UID: "u9", Phase: "Running"})
		}, "w1", ErrPrecondition},
	}
	for name, c := range cases {
		h := newHarness(t)
		c.setup(h.f, h.e)
		_, err := h.e.StartVMRestart(context.Background(), c.vm, who)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
		if h.f.LeaseObj != nil {
			t.Errorf("%s: lock left behind", name)
		}
		if len(h.commands) != 0 {
			t.Errorf("%s: ran %v", name, h.commands)
		}
	}
	// A broken target itself may be restarted: that's what the action is for.
	h := newHarness(t)
	h.f.NodeMap["w1"].Ready = false
	if _, _, err := h.e.PlanVMRestart(context.Background(), "w1"); err != nil {
		t.Errorf("NotReady target refused: %v", err)
	}
	// Not this host's VM.
	h = newHarness(t)
	h.e.Cfg.VMs[3].Host = "hv2"
	if _, err := h.e.StartVMRestart(context.Background(), "w1", who); err == nil || !strings.Contains(err.Error(), "runs on hv2") {
		t.Errorf("foreign VM: %v", err)
	}
}

func TestDrainFailureUncordons(t *testing.T) {
	h := newHarness(t)
	h.f.EvictErr = errors.New("api broke")
	r, err := h.e.StartVMRestart(context.Background(), "w1", who)
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(r)
	if err := r.Wait(); err == nil || !strings.Contains(err.Error(), "drain") {
		t.Fatalf("err = %v", err)
	}
	if !h.f.NodeMap["w1"].Schedulable || len(h.commands) != 0 {
		t.Fatalf("after a failed drain: schedulable=%v commands=%v", h.f.NodeMap["w1"].Schedulable, h.commands)
	}
	if last := evs[len(evs)-1]; last.Status != api.EventError {
		t.Fatalf("last event %+v", last)
	}
}

func TestBootTimeoutLeavesCordoned(t *testing.T) {
	h := newHarness(t)
	h.restart = func() error { return nil } // VM never comes back
	r, _ := h.e.StartVMRestart(context.Background(), "w1", who)
	collect(r)
	if err := r.Wait(); err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Fatalf("err = %v", err)
	}
	if h.f.NodeMap["w1"].Schedulable {
		t.Fatal("node uncordoned without coming back")
	}
}

func TestEtcdQuorum(t *testing.T) {
	h := newHarness(t)
	// Two servers already gone from the registered set would trip the node
	// count check first; make one server a non-etcd node instead.
	h.f.NodeMap["m2"].Roles = []string{"etcd"}
	h.f.NodeMap["m3"].Roles = []string{}
	h.f.NodeMap["w1"].Roles = []string{"etcd"}
	// etcd members: m1, m2, w1. Restarting m1 leaves m2, w1: quorum kept.
	if _, _, err := h.e.PlanVMRestart(context.Background(), "m1"); err != nil {
		t.Fatalf("quorum kept but refused: %v", err)
	}
}
