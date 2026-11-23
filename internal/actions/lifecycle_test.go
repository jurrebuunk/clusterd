package actions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kubefake"
)

// world is a fake host: VM units, Ceph flags and PG state.
type world struct {
	mu       sync.Mutex
	f        *kubefake.Kube
	units    map[string]string // vm -> ActiveState
	noout    map[string]bool   // host -> flag
	okToStop bool
	pgsClean bool
	ran      []string
	boot     int
}

func newWorld(t *testing.T, host string) (*world, *Engine) {
	w := &world{f: kubefake.New(), units: map[string]string{}, noout: map[string]bool{}, okToStop: true, pgsClean: true}
	cfg := &config.Config{Host: host}
	for _, h := range []string{"hv1", "hv2", "hv3"} {
		cfg.Hosts = append(cfg.Hosts, config.Host{Name: h})
	}
	place := map[string]string{"m1": "hv1", "w1": "hv1", "m2": "hv2", "m3": "hv3"}
	for _, vm := range []string{"m1", "m2", "m3", "w1"} {
		cfg.VMs = append(cfg.VMs, config.VM{Name: vm, Host: place[vm], Enabled: true})
		w.units[vm] = "active"
	}
	cfg.Ceph.Command, cfg.Ceph.ID, cfg.Ceph.Keyring = "ceph", "clusterd", "/k"
	e := &Engine{
		Cfg: cfg, Kube: w.f, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Timing: Timing{Poll: 2 * time.Millisecond, DrainTimeout: time.Second, RestartTimeout: time.Second,
			BootTimeout: time.Second, LockTTL: 5 * time.Second, RecoverTimeout: time.Second, Total: 5 * time.Second},
		Run: w.run,
	}
	return w, e
}

func (w *world) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cmd := strings.Join(args, " ")
	w.ran = append(w.ran, name+" "+cmd)
	vmOf := func() string {
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "microvm@"); ok {
				return strings.TrimSuffix(v, ".service")
			}
		}
		return ""
	}
	switch {
	case name == "systemctl" && strings.Contains(cmd, "show"):
		return []byte(w.units[vmOf()] + "\n"), nil
	case name == "systemctl" && strings.Contains(cmd, " stop "):
		vm := vmOf()
		w.units[vm] = "inactive"
		w.f.Mu.Lock()
		w.f.NodeMap[vm].Ready = false
		w.f.Mu.Unlock()
		return nil, nil
	case name == "systemctl" && strings.Contains(cmd, " start "):
		vm := vmOf()
		w.units[vm] = "active"
		w.boot++
		w.f.Mu.Lock()
		w.f.NodeMap[vm].Ready = true
		w.f.NodeMap[vm].BootID = "boot-new"
		w.f.Mu.Unlock()
		return nil, nil
	case name == "systemctl" && strings.Contains(cmd, "reboot"):
		return nil, nil
	case strings.Contains(cmd, "osd dump"):
		var parts []string
		for h, on := range w.noout {
			if on {
				parts = append(parts, `"`+h+`":["noout"]`)
			}
		}
		return []byte(`{"crush_node_flags":{` + strings.Join(parts, ",") + `}}`), nil
	case strings.Contains(cmd, "osd metadata"):
		return []byte(`[{"id":0,"hostname":"hv1"},{"id":1,"hostname":"hv2"},{"id":2,"hostname":"hv3"}]`), nil
	case strings.Contains(cmd, "ok-to-stop"):
		if w.okToStop {
			return []byte(`{"ok_to_stop":true}`), nil
		}
		return []byte(`{"ok_to_stop":false}`), nil
	case strings.Contains(cmd, "unset-group noout"):
		w.noout[args[len(args)-1]] = false
		return nil, nil
	case strings.Contains(cmd, "set-group noout"):
		w.noout[args[len(args)-1]] = true
		return nil, nil
	case strings.Contains(cmd, " status"):
		if w.pgsClean {
			return []byte(`{"pgmap":{"num_pgs":10,"pgs_by_state":[{"state_name":"active+clean","count":10}]}}`), nil
		}
		return []byte(`{"pgmap":{"num_pgs":10,"pgs_by_state":[{"state_name":"active+clean","count":7}]}}`), nil
	}
	return nil, errors.New("unexpected: " + name + " " + cmd)
}

func (w *world) ranAny(sub string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.ContainsFunc(w.ran, func(s string) bool { return strings.Contains(s, sub) })
}

func TestVMStopStart(t *testing.T) {
	w, e := newWorld(t, "hv1")
	r, err := e.StartVMStop(context.Background(), "w1", who)
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(r)
	if err := r.Wait(); err != nil {
		t.Fatalf("stop: %v\n%s", err, messages(evs))
	}
	if w.units["w1"] != "inactive" || w.f.NodeMap["w1"].Schedulable {
		t.Fatalf("after stop: unit %s, schedulable %v", w.units["w1"], w.f.NodeMap["w1"].Schedulable)
	}
	if !strings.Contains(messages(evs), "evicting pod apps/web-1") {
		t.Errorf("no drain:\n%s", messages(evs))
	}

	// Start: refused while running, works once stopped.
	w.units["m1"] = "active"
	if _, err := e.StartVMStart(context.Background(), "m1", who); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("start of a running VM: %v", err)
	}
	r, err = e.StartVMStart(context.Background(), "w1", who)
	if err != nil {
		t.Fatal(err)
	}
	evs = collect(r)
	if err := r.Wait(); err != nil {
		t.Fatalf("start: %v\n%s", err, messages(evs))
	}
	if w.units["w1"] != "active" || !w.f.NodeMap["w1"].Schedulable || !w.f.NodeMap["w1"].Ready {
		t.Fatalf("after start: %s %+v", w.units["w1"], *w.f.NodeMap["w1"])
	}
	if w.f.LeaseObj != nil {
		t.Fatal("lock left behind")
	}
	// Not this host's VM.
	if _, err := e.StartVMStop(context.Background(), "m2", who); err == nil || !strings.Contains(err.Error(), "runs on hv2") {
		t.Fatalf("foreign VM: %v", err)
	}
}

func TestCordon(t *testing.T) {
	w, e := newWorld(t, "hv1")
	if msg, err := e.SetCordon(context.Background(), "m2", true, who); err != nil || msg != "node/m2 cordoned" || w.f.NodeMap["m2"].Schedulable {
		t.Fatalf("cordon: %q %v", msg, err)
	}
	if _, err := e.SetCordon(context.Background(), "m2", false, who); err != nil || !w.f.NodeMap["m2"].Schedulable {
		t.Fatalf("uncordon: %v", err)
	}
}

func TestHostMaintenanceAndResume(t *testing.T) {
	w, e := newWorld(t, "hv1")
	plan, err := e.PlanHostMaintenance(context.Background(), "hv1", true)
	if err != nil {
		t.Fatal(err)
	}
	// Agents first, then servers.
	if strings.Join(plan.VMs, ",") != "w1,m1" || len(plan.OSDs) != 1 || plan.OSDs[0] != 0 || !strings.Contains(strings.Join(plan.Steps, "|"), "reboot hv1") {
		t.Fatalf("plan = %+v", plan)
	}
	r, err := e.StartHostMaintenance(context.Background(), "hv1", true, who)
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(r)
	if err := r.Wait(); err != nil {
		t.Fatalf("maintenance: %v\n%s", err, messages(evs))
	}
	if w.units["w1"] != "inactive" || w.units["m1"] != "inactive" || !w.noout["hv1"] {
		t.Fatalf("after maintenance: units %v, noout %v", w.units, w.noout)
	}
	// The reboot runs after the answer, a few seconds later.
	deadline := time.Now().Add(5 * time.Second)
	for !w.ranAny("reboot") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !w.ranAny("systemctl --no-ask-password reboot") {
		t.Fatal("no reboot")
	}
	hosts, _ := e.MaintenanceHosts(context.Background())
	if len(hosts) != 1 || hosts[0] != "hv1" {
		t.Fatalf("maintenance hosts = %v", hosts)
	}

	// While hv1 is in maintenance, no other host may go.
	_, e2 := newWorld(t, "hv2")
	e2.Run = w.run
	e2.Kube = w.f
	if _, err := e2.PlanHostMaintenance(context.Background(), "hv2", false); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second host: %v", err)
	}

	w.pgsClean = true
	r, err = e.StartHostResume(context.Background(), "hv1", who)
	if err != nil {
		t.Fatal(err)
	}
	evs = collect(r)
	if err := r.Wait(); err != nil {
		t.Fatalf("resume: %v\n%s", err, messages(evs))
	}
	if w.units["w1"] != "active" || w.units["m1"] != "active" || w.noout["hv1"] || !w.f.NodeMap["m1"].Schedulable {
		t.Fatalf("after resume: units %v noout %v", w.units, w.noout)
	}
	if !strings.Contains(messages(evs), "all 10 PGs active+clean") {
		t.Errorf("no PG wait:\n%s", messages(evs))
	}
}

func TestHostMaintenanceRefuses(t *testing.T) {
	w, e := newWorld(t, "hv1")
	w.okToStop = false
	if _, err := e.PlanHostMaintenance(context.Background(), "hv1", false); !errors.Is(err, ErrPrecondition) || !strings.Contains(err.Error(), "can't stop") {
		t.Fatalf("ok-to-stop false: %v", err)
	}
	w.okToStop = true
	w.f.NodeMap["m3"].Ready = false
	if _, err := e.PlanHostMaintenance(context.Background(), "hv1", false); !errors.Is(err, ErrPrecondition) || !strings.Contains(err.Error(), "m3") {
		t.Fatalf("other node down: %v", err)
	}
	if _, err := e.PlanHostMaintenance(context.Background(), "hv2", false); err == nil || !strings.Contains(err.Error(), "run on the host itself") {
		t.Fatalf("other host: %v", err)
	}
	if _, err := e.PlanHostMaintenance(context.Background(), "hv9", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown host: %v", err)
	}
	if w.ranAny("set-group") || w.ranAny(" stop ") {
		t.Fatal("a refused plan changed something")
	}
}

func TestResumeWaitsForCeph(t *testing.T) {
	w, e := newWorld(t, "hv1")
	w.noout["hv1"] = true
	w.pgsClean = false
	r, _ := e.StartHostResume(context.Background(), "hv1", who)
	collect(r)
	if err := r.Wait(); err == nil || !strings.Contains(err.Error(), "PGs not clean") {
		t.Fatalf("resume with unclean PGs: %v", err)
	}
}
