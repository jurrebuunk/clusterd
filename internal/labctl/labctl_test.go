package labctl

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/config"
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
	s, err := server.New(cfg, auth.NewStore(tokens), status.New(cfg, nil, log), log)
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
	if code != 0 || !strings.Contains(out, "as tester, scope manage") {
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
	if code, out, _ := run(t, "", "status"); code != 0 || !strings.Contains(out, "Site lab") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := run(t, "", "metrics", "query", "up"); code != 0 || !strings.Contains(out, `up{instance="hv01"}`) {
		t.Fatalf("metrics: %d %q", code, out)
	}
	code, out, _ = run(t, "", "logs", "--since", "1d")
	if code != 0 || strings.Index(out, "first") > strings.Index(out, "second") || strings.Index(out, "first") < 0 {
		t.Fatalf("logs (oldest first): %d %q", code, out)
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

func TestShortError(t *testing.T) {
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
