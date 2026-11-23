package status

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/config"
)

func fakeProc(t *testing.T) string {
	dir := t.TempDir()
	files := map[string]string{
		"uptime":  "3600.52 7000.00\n",
		"loadavg": "0.42 0.30 0.20 1/300 4242\n",
		"meminfo": "MemTotal:       16000000 kB\nMemFree:         1000000 kB\nMemAvailable:    4000000 kB\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const unitsJSON = `[{"unit":"microvm@vm1.service","load":"loaded","active":"active","sub":"running"},
{"unit":"microvm@vm2.service","load":"loaded","active":"failed","sub":"failed"}]`

const cephJSON = `{"health":{"status":"HEALTH_WARN","checks":{"OSD_DOWN":{"severity":"HEALTH_WARN","summary":{"message":"1 osds down"}}}},
"osdmap":{"num_osds":3,"num_up_osds":2,"num_in_osds":3},
"pgmap":{"num_pgs":10,"pgs_by_state":[{"state_name":"active+clean","count":7},{"state_name":"active+undersized","count":3}]}}`

func TestStatus(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer store.Close()

	var peerAuth string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(api.Node{Host: "hv02", Version: "0.1.0", VIPs: []string{"api"},
			VMs: []api.VMUnit{{Name: "vm3", Active: "inactive", Sub: "dead"}}})
	}))
	defer peer.Close()

	cfg := &config.Config{Site: "lab", Host: "hv01"}
	cfg.Hosts = []config.Host{
		{Name: "hv01", API: "http://unused", Metrics: store.URL, Logs: store.URL},
		{Name: "hv02", API: peer.URL, Metrics: store.URL, Logs: store.URL},
		{Name: "hv03", API: "http://127.0.0.1:1", Metrics: store.URL, Logs: store.URL},
	}
	cfg.VIPs = []config.VIP{{Name: "s3", Address: "10.0.30.10"}, {Name: "api", Address: "10.0.20.20"}, {Name: "mon", Address: "10.0.20.10"}}
	cfg.VMs = []config.VM{
		{Name: "vm1", Host: "hv01", Enabled: true}, {Name: "vm2", Host: "hv01", Enabled: true},
		{Name: "vm3", Host: "hv02", Enabled: true}, {Name: "vm4", Host: "hv03", Enabled: true},
		{Name: "vm5", Host: "hv02", Enabled: true}, {Name: "vm6", Host: "hv01", Enabled: false},
	}
	cfg.Ceph.Command, cfg.Ceph.ID, cfg.Ceph.Keyring = "ceph", "clusterd", "/k"
	cfg.Limits.PeerTimeout.Duration = 1e9

	c := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.procRoot = fakeProc(t)
	c.interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("10.0.30.10"), Mask: net.CIDRMask(32, 32)}}, nil
	}
	c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "systemctl":
			return []byte(unitsJSON), nil
		case "ceph":
			return []byte(cephJSON), nil
		}
		return nil, errors.New("unexpected command " + name)
	}

	st := c.Status(context.Background(), "tok")
	if peerAuth != "Bearer tok" {
		t.Fatalf("peer got Authorization %q", peerAuth)
	}
	h := map[string]api.HostStatus{}
	for _, x := range st.Hosts {
		h[x.Name] = x
	}
	if !h["hv01"].Reachable || h["hv01"].UptimeSeconds != 3600 || h["hv01"].MemUsedPercent != 75 || h["hv01"].Load1 != 0.42 {
		t.Fatalf("hv01 = %+v", h["hv01"])
	}
	if !h["hv02"].Reachable || h["hv03"].Reachable || h["hv03"].Error == "" {
		t.Fatalf("hv02 = %+v, hv03 = %+v", h["hv02"], h["hv03"])
	}
	want := map[string]string{"vm1": "running", "vm2": "failed", "vm3": "stopped", "vm4": "unknown", "vm5": "missing", "vm6": "disabled"}
	for _, vm := range st.VMs {
		if want[vm.Name] != vm.State {
			t.Errorf("%s: state %s, want %s", vm.Name, vm.State, want[vm.Name])
		}
	}
	owners := map[string][]string{}
	for _, v := range st.VIPs {
		owners[v.Name] = v.Owners
	}
	if len(owners["s3"]) != 1 || owners["s3"][0] != "hv01" || owners["api"][0] != "hv02" || len(owners["mon"]) != 0 {
		t.Fatalf("vips = %+v", st.VIPs)
	}
	if st.Ceph.Health != "HEALTH_WARN" || st.Ceph.OSDsUp != 2 || st.Ceph.PGsActiveClean != 7 || len(st.Ceph.Checks) != 1 {
		t.Fatalf("ceph = %+v", st.Ceph)
	}
	if st.K3s.Reachable || st.K3s.Error == "" {
		t.Fatalf("k3s without kubeconfig = %+v", st.K3s)
	}
}

func TestCephCacheCoalesces(t *testing.T) {
	cfg := &config.Config{Site: "lab", Host: "hv01", Hosts: []config.Host{{Name: "hv01", API: "http://x", Metrics: "http://x", Logs: "http://x"}}}
	cfg.Ceph.Command, cfg.Ceph.ID, cfg.Ceph.Keyring = "ceph", "clusterd", "/k"
	c := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var mu sync.Mutex
	calls := 0
	c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // the real CLI takes ~0.5 s
		return []byte(cephJSON), nil
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if got := c.cephCached(context.Background()); got.Health != "HEALTH_WARN" || got.AsOf.IsZero() {
				t.Errorf("cached = %+v", got)
			}
		})
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("10 concurrent callers ran ceph %d times, want 1", calls)
	}
	start := time.Now()
	c.cephCached(context.Background())
	if calls != 1 || time.Since(start) > 10*time.Millisecond {
		t.Fatalf("a fresh cache was not used (calls %d)", calls)
	}
	// Stale: older than a minute, it is refreshed before answering.
	c.cephMu.Lock()
	c.cephVal.AsOf = time.Now().Add(-2 * time.Minute)
	c.cephMu.Unlock()
	c.cephCached(context.Background())
	if calls != 2 {
		t.Fatalf("stale cache not refreshed (calls %d)", calls)
	}
}
