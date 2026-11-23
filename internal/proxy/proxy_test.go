package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func opts() Options {
	return Options{Timeout: 2 * time.Second, FollowMax: 2 * time.Second, MaxBytes: 1 << 20}
}

func TestFallbackAndHeaders(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	var gotAuth, gotPath, gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotQuery = r.Header.Get("Authorization"), r.URL.Path, r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "x=y")
		io.WriteString(w, `{"status":"success"}`)
	}))
	defer up.Close()

	p, err := New("metrics", []string{down.URL, up.URL}, opts(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/metrics/query?query=up", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	p.Forward(w, r, Route{Upstream: "/api/v1/query"}, r.URL.Query())

	if w.Code != 200 || !strings.Contains(w.Body.String(), "success") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if gotAuth != "" {
		t.Fatal("caller's Authorization header was sent upstream")
	}
	if gotPath != "/api/v1/query" || gotQuery != "up" {
		t.Fatalf("upstream got %s ?query=%s", gotPath, gotQuery)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("upstream headers leaked through")
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	calls := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":"bad query"}`, http.StatusBadRequest)
	}))
	defer bad.Close()
	p, _ := New("metrics", []string{bad.URL, bad.URL}, opts(), quiet)
	w := httptest.NewRecorder()
	p.Forward(w, httptest.NewRequest("GET", "/", nil), Route{Upstream: "/api/v1/query"}, url.Values{})
	if w.Code != 400 || calls != 1 {
		t.Fatalf("code %d after %d calls", w.Code, calls)
	}
}

func TestAllDown(t *testing.T) {
	p, _ := New("logs", []string{"http://127.0.0.1:1"}, opts(), quiet)
	w := httptest.NewRecorder()
	p.Forward(w, httptest.NewRequest("GET", "/", nil), Route{Upstream: "/select/logsql/query"}, url.Values{})
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestTooLargeAborts(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer big.Close()
	o := opts()
	o.MaxBytes = 100
	p, _ := New("logs", []string{big.URL}, o, quiet)
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler, got %v", r)
		}
	}()
	p.Forward(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), Route{Upstream: "/x"}, url.Values{})
	t.Fatal("did not abort")
}
