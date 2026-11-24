package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/status"
)

type upstreamHit struct{ path, query string }

func setup(t *testing.T) (http.Handler, map[auth.Scope]string, *[]upstreamHit) {
	hits := &[]upstreamHit{}
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits = append(*hits, upstreamHit{r.URL.Path, r.URL.RawQuery})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(store.Close)

	tokens := map[auth.Scope]string{}
	var lines []string
	for _, sc := range []auth.Scope{auth.ScopeRead, auth.ScopeOperator, auth.ScopeManage} {
		tok, _ := auth.Generate()
		tokens[sc] = tok
		lines = append(lines, auth.HashLine("t-"+sc.String(), sc, tok))
	}
	parsed, err := auth.Parse(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Site: "lab", Host: "hv01", Hosts: []config.Host{{Name: "hv01", API: "http://x", Metrics: store.URL, Logs: store.URL}}}
	cfg.Limits.RequestTimeout.Duration = 5 * time.Second
	cfg.Limits.FollowMax.Duration = 5 * time.Second
	cfg.Limits.ProxyMaxBytes = 1 << 20
	cfg.Limits.LogsLimit = 1000
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg, auth.NewStore(parsed), status.New(cfg, nil, log), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler(), tokens, hits
}

func do(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	return doBody(h, method, path, token, "")
}

func doBody(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuth(t *testing.T) {
	h, tok, _ := setup(t)
	cases := []struct {
		path, token string
		code        int
	}{
		{"/v1/health", "", 200},
		{"/v1/whoami", "", 401},
		{"/v1/whoami", "lab_wrong", 401},
		{"/v1/whoami", tok[auth.ScopeRead], 200},
		{"/v1/vms", "", 401},
		{"/v1/operations/x", "lab_wrong", 401},
		{"/v1/nope", "", 404},
	}
	for _, c := range cases {
		w := do(h, "GET", c.path, c.token)
		if w.Code != c.code {
			t.Errorf("GET %s: %d, want %d (%s)", c.path, w.Code, c.code, w.Body.String())
		}
		if w.Code >= 400 {
			var e struct{ Error string }
			if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Error == "" {
				t.Errorf("GET %s: error body %q is not {\"error\": …}", c.path, w.Body.String())
			}
		}
		if w.Header().Get("X-Request-Id") == "" {
			t.Errorf("GET %s: no request id", c.path)
		}
	}
	w := do(h, "GET", "/v1/whoami", tok[auth.ScopeOperator])
	if !strings.Contains(w.Body.String(), `"scope":"operator"`) || !strings.Contains(w.Body.String(), `"name":"t-operator"`) {
		t.Fatalf("whoami = %s", w.Body.String())
	}
}

func TestProxyRoutes(t *testing.T) {
	h, tok, hits := setup(t)
	read := tok[auth.ScopeRead]
	cases := []struct{ path, upstream, query string }{
		{"/v1/metrics/query?query=up", "/api/v1/query", "query=up"},
		{"/v1/metrics/query_range?query=up&step=60", "/api/v1/query_range", "query=up&step=60"},
		{"/v1/metrics/label/job/values", "/api/v1/label/job/values", ""},
		{"/v1/logs/query?query=*", "/select/logsql/query", "limit=1000&query=%2A"},
		{"/v1/logs/query?query=*&limit=5", "/select/logsql/query", "limit=5&query=%2A"},
		{"/v1/logs/query?query=*&follow=1", "/select/logsql/tail", "query=%2A"},
		{"/v1/logs/hits?query=*&step=1h", "/select/logsql/hits", "query=%2A&step=1h"},
	}
	for _, c := range cases {
		*hits = nil
		w := do(h, "GET", c.path, read)
		if w.Code != 200 || len(*hits) != 1 {
			t.Errorf("%s: code %d, %d upstream hits", c.path, w.Code, len(*hits))
			continue
		}
		if got := (*hits)[0]; got.path != c.upstream || got.query != c.query {
			t.Errorf("%s: upstream %s?%s, want %s?%s", c.path, got.path, got.query, c.upstream, c.query)
		}
	}
	// Write and admin paths of the stores are simply not routed.
	for _, p := range []string{"/v1/metrics/import", "/v1/metrics/admin/tsdb/delete_series", "/v1/logs/insert"} {
		*hits = nil
		if w := do(h, "GET", p, read); w.Code != 404 || len(*hits) != 0 {
			t.Errorf("%s: code %d, %d upstream hits", p, w.Code, len(*hits))
		}
	}
	if w := do(h, "POST", "/v1/metrics/query?query=up", read); w.Code == 200 {
		t.Error("POST to a GET route succeeded")
	}
	if w := do(h, "GET", "/v1/metrics/label/a%2Fb/values", read); w.Code != 400 {
		t.Errorf("bad label name: %d", w.Code)
	}
}
