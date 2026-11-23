package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/actions"
	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kube"
	"github.com/jurrebuunk/clusterd/internal/kubefake"
	"github.com/jurrebuunk/clusterd/internal/status"
)

type actionHarness struct {
	handler http.Handler
	srv     *Server
	fake    *kubefake.Kube
	cfg     *config.Config
	tokens  map[auth.Scope]string
	ran     []string
}

func newActionHarness(t *testing.T) *actionHarness {
	h := &actionHarness{fake: kubefake.New(), tokens: map[auth.Scope]string{}}
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(store.Close)
	var lines []string
	for _, sc := range []auth.Scope{auth.ScopeRead, auth.ScopeOperator, auth.ScopeManage} {
		tok, _ := auth.Generate()
		h.tokens[sc] = tok
		lines = append(lines, auth.HashLine("t-"+sc.String(), sc, tok))
	}
	parsed, _ := auth.Parse(strings.NewReader(strings.Join(lines, "\n")))
	cfg := &config.Config{Site: "lab", Host: "hv1", ServerName: "api.test"}
	cfg.Hosts = []config.Host{{Name: "hv1", API: "http://unused", Metrics: store.URL, Logs: store.URL}}
	for _, vm := range []string{"m1", "m2", "m3", "w1"} {
		cfg.VMs = append(cfg.VMs, config.VM{Name: vm, Host: "hv1", Enabled: true})
	}
	cfg.Limits.RequestTimeout.Duration = 5 * time.Second
	cfg.Limits.FollowMax.Duration = 5 * time.Second
	cfg.Limits.PeerTimeout.Duration = time.Second
	cfg.Limits.ProxyMaxBytes = 1 << 20
	cfg.Limits.LogsLimit = 10
	h.cfg = cfg
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := &actions.Engine{
		Cfg: cfg, Kube: h.fake, Log: log,
		Timing: actions.Timing{Poll: 2 * time.Millisecond, DrainTimeout: time.Second, RestartTimeout: time.Second,
			BootTimeout: time.Second, LockTTL: 5 * time.Second, Total: 5 * time.Second},
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			h.ran = append(h.ran, strings.Join(args, " "))
			h.fake.Mu.Lock()
			h.fake.NodeMap["w1"].BootID = "boot-2"
			h.fake.Mu.Unlock()
			return nil, nil
		},
	}
	s, err := New(cfg, auth.NewStore(parsed), status.New(cfg, nil, log), eng, log)
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.handler = s, s.Handler()
	return h
}

func TestVMRestartEndpoint(t *testing.T) {
	h := newActionHarness(t)
	op := h.tokens[auth.ScopeOperator]

	if w := do(h.handler, "POST", "/v1/actions/vms/w1/restart", h.tokens[auth.ScopeRead]); w.Code != 403 {
		t.Fatalf("read token: %d", w.Code)
	}
	if w := do(h.handler, "GET", "/v1/actions/vms/w1/restart", op); w.Code != 404 {
		t.Fatalf("GET: %d", w.Code)
	}
	if w := do(h.handler, "POST", "/v1/actions/vms/nope/restart", op); w.Code != 404 {
		t.Fatalf("unknown VM: %d", w.Code)
	}

	w := do(h.handler, "POST", "/v1/actions/vms/w1/restart?dry_run=1", op)
	var plan api.RestartPlan
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || len(plan.Evict) != 2 {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	if len(h.ran) != 0 {
		t.Fatal("dry run ran something")
	}

	h.fake.NodeMap["m2"].Ready = false
	if w := do(h.handler, "POST", "/v1/actions/vms/w1/restart", op); w.Code != 412 || !strings.Contains(w.Body.String(), "m2") {
		t.Fatalf("precondition: %d %s", w.Code, w.Body.String())
	}
	h.fake.NodeMap["m2"].Ready = true

	l := &kube.Lease{}
	l.Metadata.Name, l.Metadata.Namespace = "clusterd-disruption", "clusterd"
	l.Spec.HolderIdentity, l.Spec.LeaseDurationSeconds = "someone else", 60
	l.Spec.RenewTime = kube.MicroTime{Time: time.Now()}
	held, _ := h.fake.CreateLease(context.Background(), l)
	if w := do(h.handler, "POST", "/v1/actions/vms/w1/restart", op); w.Code != 409 || !strings.Contains(w.Body.String(), "someone else") {
		t.Fatalf("busy: %d %s", w.Code, w.Body.String())
	}
	h.fake.DeleteLease(context.Background(), held)

	w = do(h.handler, "POST", "/v1/actions/vms/w1/restart", op)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("run: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	var last api.ActionEvent
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if last.Status != api.EventDone || len(lines) < 6 {
		t.Fatalf("stream ended with %+v after %d lines:\n%s", last, len(lines), w.Body.String())
	}
	if len(h.ran) != 1 || h.ran[0] != "--no-ask-password restart microvm@w1.service" {
		t.Fatalf("ran %v", h.ran)
	}
}

func TestVMRestartForwarding(t *testing.T) {
	h := newActionHarness(t)
	var gotAuth, gotBy, gotPath string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBy, gotPath = r.Header.Get("Authorization"), r.Header.Get(forwardedHeader), r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"time":"2026-01-01T00:00:00Z","status":"done","message":"ok"}`+"\n")
	}))
	defer peer.Close()
	h.cfg.Hosts = append(h.cfg.Hosts, config.Host{Name: "hv2", API: peer.URL, Metrics: "http://x", Logs: "http://x"})
	h.cfg.VMs[3].Host = "hv2"
	op := h.tokens[auth.ScopeOperator]

	w := do(h.handler, "POST", "/v1/actions/vms/w1/restart?dry_run=1", op)
	if w.Code != 200 || gotAuth != "Bearer "+op || gotBy != "hv1" || gotPath != "/v1/actions/vms/w1/restart?dry_run=1" {
		t.Fatalf("forward: %d auth=%q by=%q path=%q", w.Code, gotAuth, gotBy, gotPath)
	}
	if w.Header().Get("X-Clusterd-Executed-By") != "hv2" {
		t.Fatal("missing X-Clusterd-Executed-By")
	}
	// A forwarded request for a VM that isn't here is never passed on again.
	r := httptest.NewRequest("POST", "/v1/actions/vms/w1/restart", nil)
	r.Header.Set("Authorization", "Bearer "+op)
	r.Header.Set(forwardedHeader, "hv3")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("double forward: %d", rec.Code)
	}
}
