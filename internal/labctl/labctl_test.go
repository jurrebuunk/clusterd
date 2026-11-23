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
	"strings"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/actions"
	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/kubefake"
	"github.com/jurrebuunk/clusterd/internal/server"
	"github.com/jurrebuunk/clusterd/internal/status"
)

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
	cfg := &config.Config{Site: "lab", Host: "hv01", Hosts: []config.Host{{Name: "hv01", API: "http://x", Metrics: store.URL, Logs: store.URL}}}
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
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			fake.Mu.Lock()
			fake.NodeMap["w1"].BootID = "boot-2"
			fake.Mu.Unlock()
			return nil, nil
		}}
	s, err := server.New(cfg, auth.NewStore(tokens), status.New(cfg, nil, log), eng, log)
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

	if code, _, _ := run(t, "", "whoami", "--site", "lab"); code != ExitUsage {
		t.Fatalf("unknown site: exit %d", code)
	}
	if code, _, _ := run(t, "lab_wrong\n", "login", "--site", "lab", "--api", url); code != ExitAuth {
		t.Fatalf("login with a bad token: exit %d", code)
	}
	code, out, errs := run(t, tok+"\n", "login", "--site", "lab", "--api", url)
	if code != 0 || !strings.Contains(out, "as tester (manage)") {
		t.Fatalf("login: %d %q %q", code, out, errs)
	}
	tokPath := filepath.Join(cfgHome, "labctl", "tokens", "lab")
	if fi, err := os.Stat(tokPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	// hgd1 (built-in) and lab exist; switch to lab.
	if code, _, _ := run(t, "", "use", "lab"); code != 0 {
		t.Fatal("use lab failed")
	}
	if code, out, _ := run(t, "", "whoami"); code != 0 || !strings.Contains(out, "tester") {
		t.Fatalf("whoami: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "--json", "status"); code != 0 || !strings.Contains(out, `"site": "lab"`) {
		t.Fatalf("status --json: %d %q", code, out)
	}
	for _, sec := range []string{"ceph", "vm", "nodes"} {
		if code, out, errs := run(t, "", "status", sec); code != 0 || strings.HasPrefix(out, "HOST") {
			t.Fatalf("status %s: %d %q %q", sec, code, out, errs)
		}
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

	// Environment overrides.
	t.Setenv("LABCTL_TOKEN", "lab_wrong")
	if code, _, _ := run(t, "", "whoami"); code != ExitAuth {
		t.Fatalf("LABCTL_TOKEN: exit %d", code)
	}
	t.Setenv("LABCTL_TOKEN", "")
	t.Setenv("LABCTL_API", "http://127.0.0.1:1")
	if code, _, errs := run(t, "", "whoami"); code != ExitUnreachable || !strings.Contains(errs, "WARP") {
		t.Fatalf("unreachable: exit %d %q", code, errs)
	}
	t.Setenv("LABCTL_API", "")

	// A site without a token.
	if code, _, _ := run(t, "", "--site", "hgd1", "whoami"); code != ExitAuth {
		t.Fatalf("no token: exit %d", code)
	}
	if code, _, _ := run(t, "", "bogus"); code != ExitUsage {
		t.Fatalf("unknown command: exit %d", code)
	}
}

func TestTokenNew(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out := filepath.Join(t.TempDir(), "tok")
	code, stdout, _ := run(t, "", "token", "new", "--name", "ui", "--scope", "operator", "--out", out)
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
	if code, _, _ := run(t, "", "token", "new", "--name", "ui", "--scope", "admin"); code != ExitUsage {
		t.Fatal("bad scope accepted")
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
			Buckets:  []api.S3Bucket{{Name: "backups", Owner: "app", Objects: 40, SizeBytes: 2e9, QuotaBytes: 4e9}, {Name: "media", Owner: "jurre", Objects: 10, SizeBytes: 1e9}},
			Pool:     api.S3Pool{StoredBytes: 3e9, MaxAvailBytes: 297e9, UsedPercent: 1}},
	}
	st.K3s.Nodes[0].Schedulable = false
	return st
}

func TestStatusOutput(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var out, warn bytes.Buffer
	printStatus(&out, &warn, statusFixture(now), now)
	want := `HOST  STATUS       CLUSTERD  UPTIME  LOAD  MEM
hv01  Up,LogsDown  1.0.0     3d      0.50  32%
hv02  Unreachable  -         -       -     -

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

BUCKET   OWNER  OBJECTS  SIZE  QUOTA  FULL
backups  app    40       2.0G  4.0G   50%
media    jurre  10       1.0G  -      -
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
		if code != 0 || !strings.Contains(out, "vm") || strings.Contains(out, "COMMANDS") {
			t.Errorf("%s: %d %q", sh, code, out[:min(len(out), 80)])
		}
	}
	if code, out, _ := run(t, "", "__complete", "sites"); code != 0 || strings.TrimSpace(out) != "hgd1" {
		t.Errorf("sites: %q", out)
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
