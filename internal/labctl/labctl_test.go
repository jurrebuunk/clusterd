package labctl

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/actions"
	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/fakestore"
	"github.com/jurrebuunk/clusterd/internal/kubefake"
	"github.com/jurrebuunk/clusterd/internal/server"
	"github.com/jurrebuunk/clusterd/internal/status"
)

var unitsMu sync.Mutex
var units = map[string]string{}
var restarts []string

// fakeSystemctl plays systemctl for VM units: restart and start give the
// node a new boot, stop makes it NotReady.
func fakeSystemctl(fake *kubefake.Kube) func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		unitsMu.Lock()
		defer unitsMu.Unlock()
		vm := ""
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "microvm@"); ok {
				vm = strings.TrimSuffix(v, ".service")
			}
		}
		state, ok := units[vm]
		if !ok {
			state = "active"
		}
		fake.Mu.Lock()
		defer fake.Mu.Unlock()
		switch {
		case slices.Contains(args, "show"):
			return []byte(state + "\n"), nil
		case slices.Contains(args, "stop"):
			units[vm] = "inactive"
			fake.NodeMap[vm].Ready = false
		case slices.Contains(args, "restart"), slices.Contains(args, "start"):
			if slices.Contains(args, "restart") {
				restarts = append(restarts, vm)
			}
			units[vm] = "active"
			fake.NodeMap[vm].Ready = true
			fake.NodeMap[vm].BootID = fake.NodeMap[vm].BootID + "+"
		}
		return nil, nil
	}
}

// apiServer runs a real clusterd handler with one manage token.
func apiServer(t *testing.T) (string, string) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/query":
			io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","instance":"hv01"},"value":[1,"1"]}]}}`)
		case "/select/logsql/query":
			io.WriteString(w, `{"_time":"2026-01-01T10:00:02Z","_msg":"second","host":"hv01","unit":"a.service"}`+"\n")
			io.WriteString(w, `{"_time":"2026-01-01T10:00:01Z","_msg":"first","host":"hv01","unit":"a.service"}`+"\n")
		}
	}))
	t.Cleanup(store.Close)
	tok, _ := auth.Generate()
	tokens, _ := auth.Parse(strings.NewReader(auth.HashLine("tester", auth.ScopeManage, tok)))
	rgw := fakestore.RGW()
	t.Cleanup(rgw.Close)
	cred := filepath.Join(t.TempDir(), "s3")
	os.WriteFile(cred, []byte(`{"access_key":"AK","secret_key":"SK"}`), 0o600)
	cfg := &config.Config{Site: "lab", Host: "hv01", Hosts: []config.Host{{Name: "hv01", API: "http://x", Metrics: store.URL, Logs: store.URL, RGW: rgw.URL}}}
	cfg.S3.Endpoint, cfg.S3.Credentials = "https://s3.test", cred
	cfg.Limits.RequestTimeout.Duration = 5 * time.Second
	cfg.Limits.FollowMax.Duration = 5 * time.Second
	cfg.Limits.ProxyMaxBytes = 1 << 20
	cfg.Limits.LogsLimit = 10
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, vm := range []string{"m1", "m2", "m3", "w1"} {
		cfg.VMs = append(cfg.VMs, config.VM{Name: vm, Host: "hv01", Enabled: true})
	}
	fake := kubefake.New()
	eng := &actions.Engine{Cfg: cfg, Kube: fake, Log: log,
		Timing: actions.Timing{Poll: 2 * time.Millisecond, DrainTimeout: time.Second, RestartTimeout: time.Second,
			BootTimeout: time.Second, LockTTL: 5 * time.Second, Total: 5 * time.Second},
		Run: fakeSystemctl(fake)}
	col := status.NewWith(cfg, fake, log)
	col.SetSysRoot(t.TempDir())
	s, err := server.New(cfg, auth.NewStore(tokens), col, eng, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv.URL, tok
}

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestEndToEnd(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("LABCTL_API", "")
	t.Setenv("LABCTL_TOKEN", "")
	t.Setenv("LABCTL_SITE", "")
	url, tok := apiServer(t)

	if code, _, _ := run(t, "", "auth", "status", "--site", "lab"); code != ExitUsage {
		t.Fatalf("unknown site: exit %d", code)
	}
	if code, _, _ := run(t, "lab_wrong\n", "auth", "login", "--site", "lab", "--api", url); code != ExitAuth {
		t.Fatalf("login with a bad token: exit %d", code)
	}
	code, out, errs := run(t, tok+"\n", "auth", "login", "--site", "lab", "--api", url)
	if code != 0 || !strings.Contains(out, "as tester (manage)") {
		t.Fatalf("login: %d %q %q", code, out, errs)
	}
	tokPath := filepath.Join(cfgHome, "labctl", "tokens", "lab")
	if fi, err := os.Stat(tokPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	// hgd1 (built-in) and lab exist; switch to lab.
	if code, _, _ := run(t, "", "site", "use", "lab"); code != 0 {
		t.Fatal("site use lab failed")
	}
	if code, out, _ := run(t, "", "site", "list", "-o", "name"); code != 0 || out != "hgd1\nlab\n" {
		t.Fatalf("site list: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "auth", "status"); code != 0 || !strings.Contains(out, "tester") {
		t.Fatalf("auth status: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "-o", "json", "status"); code != 0 || !strings.Contains(out, `"site": "lab"`) {
		t.Fatalf("status -o json: %d %q", code, out)
	}
	// Every resource lists; global flags go anywhere on the line.
	for _, c := range [][]string{{"host", "list"}, {"vm", "ls"}, {"node", "list"}, {"ceph", "status"}, {"osd", "list"},
		{"disk", "list"}, {"fs", "list"}, {"vip", "list"}, {"s3", "status"}, {"s3", "bucket", "list"}, {"op", "list"}} {
		if code, out, errs := run(t, "", c...); code != 0 || out == "" {
			t.Fatalf("%v: %d %q %q", c, code, out, errs)
		}
	}
	if code, out, _ := run(t, "", "vm", "list", "-o", "name"); code != 0 || out != "m1\nm2\nm3\nw1\n" {
		t.Fatalf("vm list -o name: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "vm", "show", "w1"); code != 0 || !strings.Contains(out, "State:") {
		t.Fatalf("vm show: %d %q", code, out)
	}
	if code, _, errs := run(t, "", "vm", "show", "nope"); code != ExitAPI || !strings.Contains(errs, `no VM "nope"`) {
		t.Fatalf("vm show nope: %d %q", code, errs)
	}
	if code, out, _ := run(t, "", "node", "show", "w1", "-o", "json"); code != 0 || !strings.Contains(out, `"name": "w1"`) {
		t.Fatalf("node show: %d %q", code, out)
	}
	// Help everywhere; unknown verbs suggest the closest.
	if code, out, _ := run(t, "", "vm", "--help"); code != 0 || !strings.Contains(out, "roll") {
		t.Fatalf("vm --help: %d %q", code, out)
	}
	if code, _, errs := run(t, "", "vm", "stpo", "w1"); code != ExitUsage || !strings.Contains(errs, "did you mean stop") {
		t.Fatalf("typo: %d %q", code, errs)
	}
	if code, _, _ := run(t, "", "-o", "yaml", "vm", "list"); code != ExitUsage {
		t.Fatalf("-o yaml: %d", code)
	}
	if code, out, _ := run(t, "", "status"); code != 0 || !strings.HasPrefix(out, "HOST") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "metrics", "query", "up"); code != 0 || !strings.Contains(out, `up{instance="hv01"}`) {
		t.Fatalf("metrics: %d %q", code, out)
	}
	code, out, _ = run(t, "", "logs", "--since", "1d")
	if code != 0 || strings.Index(out, "first") > strings.Index(out, "second") || strings.Index(out, "first") < 0 {
		t.Fatalf("logs (oldest first): %d %q", code, out)
	}

	// VM restart: refuses without a terminal or --yes, dry run lists steps,
	// --yes runs it and prints the progress.
	if code, _, errs := run(t, "", "vm", "restart", "w1"); code != ExitUsage || !strings.Contains(errs, "--yes") {
		t.Fatalf("vm restart without --yes: %d %q", code, errs)
	}
	if code, out, _ := run(t, "", "vm", "restart", "w1", "--dry-run"); code != 0 || !strings.Contains(out, "drain node/w1 (2 pods)") {
		t.Fatalf("dry run: %d %q", code, out)
	}
	code, out, errs = run(t, "", "vm", "restart", "w1", "--yes")
	if code != 0 || !strings.Contains(out, "node/w1 uncordoned") || !strings.HasSuffix(strings.TrimSpace(out), "vm restart w1 done") {
		t.Fatalf("vm restart: %d %q %q", code, out, errs)
	}
	if code, _, errs := run(t, "", "vm", "restart", "nope", "--yes"); code != ExitAPI || !strings.Contains(errs, "no VM") {
		t.Fatalf("unknown vm: %d %q", code, errs)
	}

	// Stop and start.
	if code, out, errs := run(t, "", "vm", "stop", "w1", "--yes"); code != 0 || !strings.Contains(out, "microvm@w1 stopped") {
		t.Fatalf("vm stop: %d %q %q", code, out, errs)
	}
	if code, _, errs := run(t, "", "vm", "start", "m1", "--yes"); code != ExitAPI || !strings.Contains(errs, "already active") {
		t.Fatalf("start a running VM: %d %q", code, errs)
	}
	if code, out, errs := run(t, "", "vm", "start", "w1", "--yes"); code != 0 || !strings.Contains(out, "node/w1 uncordoned") {
		t.Fatalf("vm start: %d %q %q", code, out, errs)
	}

	// Roll: agents first, every VM once.
	restarts = nil
	if code, out, errs := run(t, "", "vm", "roll", "--yes"); code != 0 || strings.Count(out, " done\n") != 4 {
		t.Fatalf("vm roll: %d %q %q", code, out, errs)
	}
	if strings.Join(restarts, ",") != "w1,m1,m2,m3" {
		t.Fatalf("roll order %v, want the agent first", restarts)
	}

	// Cordon and uncordon.
	if code, out, _ := run(t, "", "node", "cordon", "m2"); code != 0 || strings.TrimSpace(out) != "node/m2 cordoned" {
		t.Fatalf("cordon: %d %q", code, out)
	}
	run(t, "", "node", "uncordon", "m2")

	// S3: a user whose key only goes to a file, a bucket with a quota.
	keys := filepath.Join(t.TempDir(), "web.env")
	if code, _, errs := run(t, "", "s3", "user", "create", "web"); code != ExitUsage || !strings.Contains(errs, "--out") {
		t.Fatalf("user create without --out: %d %q", code, errs)
	}
	code, out, errs = run(t, "", "s3", "user", "create", "web", "--out", keys)
	b, _ := os.ReadFile(keys)
	fi, _ := os.Stat(keys)
	if code != 0 || strings.Contains(out, "SK") || !strings.Contains(string(b), "AWS_SECRET_ACCESS_KEY=SK") || fi.Mode().Perm() != 0o600 {
		t.Fatalf("user create: %d %q %q %q", code, out, errs, b)
	}
	if code, _, _ := run(t, "", "s3", "user", "create", "web2", "--out", keys); code != ExitUsage {
		t.Fatal("overwrote a key file")
	}
	if code, out, errs := run(t, "", "s3", "bucket", "create", "web-assets", "--owner", "web", "--quota", "5G"); code != 0 || !strings.Contains(out, "quota 5.0G") {
		t.Fatalf("bucket create: %d %q %q", code, out, errs)
	}
	if code, out, _ := run(t, "", "s3", "bucket", "quota", "web-assets", "none"); code != 0 || !strings.Contains(out, "no quota") {
		t.Fatalf("quota none: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "s3", "user", "list"); code != 0 || !strings.Contains(out, "web ") || strings.Contains(out, "clusterd") {
		t.Fatalf("user list: %d %q", code, out)
	}
	if code, _, errs := run(t, "", "s3", "user", "delete", "web", "--yes"); code != ExitAPI || !strings.Contains(errs, "owns buckets") {
		t.Fatalf("delete an owner: %d %q", code, errs)
	}
	if code, _, _ := run(t, "", "s3", "bucket", "delete", "web-assets"); code != ExitUsage {
		t.Fatal("deleted without a terminal or --yes")
	}
	run(t, "", "s3", "bucket", "delete", "web-assets", "--yes")
	if code, out, errs := run(t, "", "s3", "user", "delete", "web", "--yes"); code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("user delete: %d %q %q", code, out, errs)
	}

	// Without waiting: the operation, named.
	if code, out, errs := run(t, "", "vm", "restart", "w1", "--yes", "--no-wait"); code != 0 || !strings.HasPrefix(out, "operation/") {
		t.Fatalf("--no-wait: %d %q %q", code, out, errs)
	}
	waitUnlocked(t)

	// Health: the fake has no monitoring data, so some areas fail.
	if code, out, _ := run(t, "", "health"); code != ExitUnhealthy || !strings.HasPrefix(out, "CHECK") || !strings.Contains(out, "nodes") {
		t.Fatalf("health: %d %q", code, out)
	}

	// Environment overrides.
	t.Setenv("LABCTL_TOKEN", "lab_wrong")
	if code, _, _ := run(t, "", "auth", "status"); code != ExitAuth {
		t.Fatalf("LABCTL_TOKEN: exit %d", code)
	}
	t.Setenv("LABCTL_TOKEN", "")
	t.Setenv("LABCTL_API", "http://127.0.0.1:1")
	if code, _, errs := run(t, "", "auth", "status"); code != ExitUnreachable || !strings.Contains(errs, "WARP") {
		t.Fatalf("unreachable: exit %d %q", code, errs)
	}
	t.Setenv("LABCTL_API", "")

	// A site without a token.
	if code, _, _ := run(t, "", "--site", "hgd1", "auth", "status"); code != ExitAuth {
		t.Fatalf("no token: exit %d", code)
	}
	if code, _, _ := run(t, "", "bogus"); code != ExitUsage {
		t.Fatalf("unknown command: exit %d", code)
	}
	if code, _, _ := run(t, "", "vm", "list", "--bogus"); code != ExitUsage {
		t.Fatalf("unknown flag: exit %d", code)
	}
	if code, _, _ := run(t, "", "auth", "logout"); code != 0 {
		t.Fatal("logout")
	}
	if code, _, _ := run(t, "", "auth", "status"); code != ExitAuth {
		t.Fatal("still logged in")
	}
}

// waitUnlocked waits until an action started without --wait is done.
func waitUnlocked(t *testing.T) {
	t.Helper()
	for range 100 {
		if code, _, errs := run(t, "", "vm", "restart", "m1", "--dry-run"); code == 0 && !strings.Contains(errs, "busy") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the action never finished")
}

func TestTokenNew(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out := filepath.Join(t.TempDir(), "tok")
	code, stdout, _ := run(t, "", "auth", "token", "create", "--name", "ui", "--scope", "operator", "--out", out)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	tok, _ := readToken(out)
	if strings.Contains(stdout, tok) {
		t.Fatal("token printed although --out was given")
	}
	parsed, err := auth.Parse(strings.NewReader(stdout))
	if err != nil || len(parsed) != 1 {
		t.Fatalf("hash line %q: %v", stdout, err)
	}
	if got, ok := auth.NewStore(parsed).Lookup(tok); !ok || got.Scope != auth.ScopeOperator {
		t.Fatal("hash line does not match the token")
	}
	if code, _, _ := run(t, "", "auth", "token", "create", "--name", "ui", "--scope", "admin"); code != ExitUsage {
		t.Fatal("bad scope accepted")
	}
}

func TestUnitFilter(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"clusterd":                          `unit:in("clusterd", "clusterd.service")`,
		"ceph-osd-0.service":                `unit:="ceph-osd-0.service"`,
		"acme-order-renew-s3.lan.buunk.org": `unit:in("acme-order-renew-s3.lan.buunk.org", "acme-order-renew-s3.lan.buunk.org.service")`,
		"ceph-osd*":                         `unit:"ceph-osd"*`,
		"acme-renew-s3.lan.buunk.org.timer": `unit:="acme-renew-s3.lan.buunk.org.timer"`,
	} {
		if got := unitFilter(in); got != want {
			t.Errorf("unitFilter(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBuildLogsQuery(t *testing.T) {
	cases := []struct {
		host, unit string
		follow     bool
		user, want string
	}{
		{"", "", false, "", `_time:1h | sort by (_time desc) limit 100`},
		{"hv01", "ceph-osd-0.service", false, "error", `_time:1h host:="hv01" unit:="ceph-osd-0.service" (error) | sort by (_time desc) limit 100`},
		{"hv01", "", true, "", `host:="hv01"`},
		{"", "clusterd", true, "", `unit:in("clusterd", "clusterd.service")`},
		{"", "", true, "", `*`},
		{"", "", false, "error | stats count()", `_time:1h error | stats count()`},
	}
	for _, c := range cases {
		if got := buildLogsQuery(c.host, c.unit, time.Hour, 100, c.follow, c.user); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "2d": 48 * time.Hour, "1w": 168 * time.Hour} {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseSince("-1h"); err == nil {
		t.Error("negative duration accepted")
	}
}

func TestShortErrorAndTitle(t *testing.T) {
	if title("running") != "Running" {
		t.Error("title")
	}
	for in, want := range map[string]string{
		`unreachable: Get "https://10.10.20.11/v1/node": dial tcp 10.10.20.11:443: connect: connection refused`:                      "connection refused",
		`unreachable: Get "https://10.10.20.12/v1/node": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`: "context deadline exceeded",
		"plain": "plain",
	} {
		if got := shortError(in); got != want {
			t.Errorf("shortError(%q) = %q, want %q", in, got, want)
		}
	}
}

func statusFixture(now time.Time) api.Status {
	wear := 47.0
	exp := now.Add(80 * 24 * time.Hour)
	st := api.Status{
		Hosts: []api.HostStatus{
			{Name: "hv01", Reachable: true, Version: "1.0.0", UptimeSeconds: 3 * 86400, Load1: 0.5, MemUsedPercent: 31.6,
				NixOS:  &api.NixOS{Version: "26.05", Revision: "54edb49c0ffee", RebootRequired: true},
				Stores: []api.StoreHealth{{Name: "victoriametrics", OK: true}, {Name: "victorialogs", OK: false}}},
			{Name: "hv02", Error: `unreachable: Get "https://x/v1/node": dial tcp: connect: connection refused`},
		},
		VMs: []api.VMStatus{{Name: "w1", Host: "hv01", State: "running"}},
		K3s: api.K3sStatus{Reachable: true, Nodes: []api.K3sNode{{Name: "w1", Ready: true, Roles: []string{}, Version: "v1", Zone: "hv01"}}},
		Ceph: api.CephStatus{Health: "HEALTH_WARN", Checks: []string{"HEALTH_WARN SLOW_OPS: 2 slow ops"}, OSDsTotal: 3, OSDsUp: 3, OSDsIn: 3,
			PGsTotal: 10, PGsActiveClean: 10, BytesTotal: 3e12, BytesUsed: 36e9, AsOf: now.Add(-4 * time.Second)},
		VIPs: []api.VIPStatus{{Name: "api", Address: "10.0.0.20", Owners: []string{"hv01"}}, {Name: "mon", Address: "10.0.0.10", Owners: []string{}}},
		Disks: api.DisksStatus{
			Disks: []api.Disk{
				{Host: "hv01", Device: "nvme0", Type: "nvme", Role: "system", SizeBytes: 256e9, UsedBytes: 50e9, UsedPercent: 19.6, WearPercent: &wear, TemperatureC: 41, Healthy: true},
				{Host: "hv01", Device: "sda", Type: "hdd", Role: "osd.0", SizeBytes: 1000204886016, UsedBytes: 12e9, UsedPercent: 1.3, TemperatureC: 33, Healthy: true},
			},
			Filesystems: []api.Filesystem{{Host: "hv01", Mountpoint: "/", Device: "/dev/nvme0n1p2", SizeBytes: 200e9, UsedBytes: 50e9, UsedPercent: 26.3},
				{Host: "vm1", Mountpoint: "/var/lib", Device: "/dev/vda", SizeBytes: 60e9, UsedBytes: 55e9, UsedPercent: 92}},
		},
		S3: api.S3Status{Endpoint: "https://s3.test", Users: 2, Objects: 50, SizeBytes: 3e9, CertExpires: &exp,
			Gateways: []api.S3Gateway{{Host: "hv01", Up: true}, {Host: "hv02", Up: true}},
			Buckets:  2,
			Pool:     api.S3Pool{StoredBytes: 3e9, MaxAvailBytes: 297e9, UsedPercent: 1}},
	}
	st.K3s.Nodes[0].Schedulable = false
	return st
}

func TestStatusOutput(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var out, warn bytes.Buffer
	printStatus(&out, &warn, statusFixture(now), now)
	want := `HOST  STATUS                    CLUSTERD  COMMIT   UPTIME  LOAD  MEM
hv01  Up,RebootNeeded,LogsDown  1.0.0     54edb49  3d      0.50  32%
hv02  Unreachable               -         -        -       -     -

VM  HOST  STATUS
w1  hv01  Running

NODE  STATUS                    ROLES   VERSION  ZONE
w1    Ready,SchedulingDisabled  <none>  v1       hv01

CEPH         OSDS UP  OSDS IN  PGS CLEAN  RAW USED  RAW SIZE  FULL  AGE
HEALTH_WARN  3/3      3/3      10/10      36G       3.0T      1%    4s

VIP  ADDRESS    OWNER
api  10.0.0.20  hv01
mon  10.0.0.10  <unknown>

DISK   HOST  ROLE    TYPE  SIZE  USED  FULL  WEAR  TEMP  SMART
nvme0  hv01  system  nvme  256G  50G   20%   47%   41C   OK
sda    hv01  osd.0   hdd   1.0T  12G   1%    -     33C   OK

FILESYSTEM  HOST  DEVICE          SIZE  USED  FULL
/           hv01  /dev/nvme0n1p2  200G  50G   26%
/var/lib    vm1   /dev/vda        60G   55G   92%

S3       GATEWAYS  USERS  BUCKETS  OBJECTS  STORED  ROOM  FULL  CERT
s3.test  2/2       2      2        50       3.0G    297G  1%    80d
`
	if got := out.String(); got != want {
		t.Errorf("status output:\n%s\nwant:\n%s", got, want)
	}
	for _, w := range []string{"warning: hv02: connection refused", "warning: ceph HEALTH_WARN SLOW_OPS", "warning: vm1 /var/lib is 92% full"} {
		if !strings.Contains(warn.String(), w) {
			t.Errorf("missing %q in warnings:\n%s", w, warn.String())
		}
	}
}

func TestPct(t *testing.T) {
	for p, want := range map[float64]string{0: "0%", 0.02: "<0.1%", 0.5: "0.5%", 19.6: "20%", 100: "100%"} {
		if got := pct(p); got != want {
			t.Errorf("pct(%v) = %s, want %s", p, got, want)
		}
	}
}

func TestBytesHuman(t *testing.T) {
	for b, want := range map[int64]string{999: "999B", 1500: "1.5K", 256060514304: "256G", 1000204886016: "1.0T", 36e9: "36G"} {
		if got := bytesHuman(b); got != want {
			t.Errorf("bytesHuman(%d) = %s, want %s", b, got, want)
		}
	}
}

func TestCompletion(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, sh := range []string{"bash", "zsh", "fish"} {
		code, out, _ := run(t, "", "completion", sh)
		if code != 0 || !strings.Contains(out, "labctl") {
			t.Errorf("%s: %d %q", sh, code, out[:min(len(out), 80)])
		}
	}
	if code, out, _ := run(t, "", "__complete", "site", "use", ""); code != 0 || !strings.HasPrefix(out, "hgd1\n") {
		t.Errorf("sites: %q", out)
	}
	if code, out, _ := run(t, "", "__complete", "v"); code != 0 || !strings.Contains(out, "vm\t") || !strings.Contains(out, "vip\t") {
		t.Errorf("commands: %q", out)
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "30s", 5 * time.Minute: "5m", 3*time.Hour + 4*time.Minute: "3h4m",
		20 * time.Hour: "20h", 50 * time.Hour: "2d2h", 40 * 24 * time.Hour: "40d",
	} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %s, want %s", d, got, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"10G": 10e9, "1.5T": 1.5e12, "500M": 500e6, "10Gi": 10 << 30, "512MiB": 512 << 20, "100": 100} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "G", "-1G", "ten"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
}
