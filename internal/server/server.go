// Package server is clusterd's HTTP API.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metrics"

	"github.com/jurrebuunk/clusterd/internal/actions"
	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/auth"
	"github.com/jurrebuunk/clusterd/internal/config"
	"github.com/jurrebuunk/clusterd/internal/proxy"
	"github.com/jurrebuunk/clusterd/internal/status"
	"github.com/jurrebuunk/clusterd/internal/version"
)

// Server holds everything the handlers need.
type Server struct {
	cfg     *config.Config
	tokens  *auth.Store
	status  *status.Collector
	metrics *proxy.Proxy
	logs    *proxy.Proxy
	actions *actions.Engine // nil without Kubernetes access
	peers   *http.Client    // forwarding actions to the VM's host
	log     *slog.Logger
}

// New wires the API. The proxies are built from cfg; eng may be nil.
func New(cfg *config.Config, tokens *auth.Store, st *status.Collector, eng *actions.Engine, log *slog.Logger) (*Server, error) {
	opts := proxy.Options{
		Timeout:   cfg.Limits.RequestTimeout.Duration,
		FollowMax: cfg.Limits.FollowMax.Duration,
		MaxBytes:  cfg.Limits.ProxyMaxBytes,
	}
	var mURLs, lURLs []string
	for _, h := range cfg.LocalFirst() {
		mURLs = append(mURLs, h.Metrics)
		lURLs = append(lURLs, h.Logs)
	}
	mp, err := proxy.New("victoriametrics", mURLs, opts, log)
	if err != nil {
		return nil, err
	}
	lp, err := proxy.New("victorialogs", lURLs, opts, log)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, tokens: tokens, status: st, metrics: mp, logs: lp, actions: eng, peers: peerClient(cfg), log: log}, nil
}

type route struct {
	pattern string // Go ServeMux pattern
	name    string // label for metrics and logs
	scope   auth.Scope
	stream  bool
	h       http.HandlerFunc
}

// Route describes one endpoint, for the contract test against api/openapi.yaml.
type Route struct {
	Method, Path, Scope string
}

// Routes lists the documented endpoints (not the catch-alls).
func (s *Server) Routes() []Route {
	var out []Route
	for _, rt := range s.routes() {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			continue
		}
		out = append(out, Route{Method: method, Path: path, Scope: rt.scope.String()})
	}
	return out
}

// Handler is the full API with middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, s.wrap(rt))
	}
	return securityHeaders(mux)
}

func (s *Server) routes() []route {
	return []route{
		{"GET /v1/health", "health", auth.ScopeNone, false, s.health},
		{"GET /v1/whoami", "whoami", auth.ScopeRead, false, s.whoami},
		{"GET /v1/internal/node", "internal_node", auth.ScopeRead, false, s.node},
		{"GET /v1/status", "status", auth.ScopeRead, false, s.statusAll},
		{"GET /v1/checks", "checks", auth.ScopeRead, false, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, s.status.Checks(r.Context(), callerFrom(r.Context()).raw))
		}},

		{"GET /v1/hosts", "hosts", auth.ScopeRead, false, s.hosts},
		{"GET /v1/hosts/{name}", "host", auth.ScopeRead, false, s.hosts},
		{"POST /v1/hosts/{name}/maintenance", "host_maintenance", auth.ScopeManage, true, s.hostAction("maintenance")},
		{"POST /v1/hosts/{name}/resume", "host_resume", auth.ScopeManage, true, s.hostAction("resume")},
		{"GET /v1/vms", "vms", auth.ScopeRead, false, s.vms},
		{"GET /v1/vms/{name}", "vm", auth.ScopeRead, false, s.vms},
		{"POST /v1/vms/{name}/restart", "vm_restart", auth.ScopeOperator, true, s.vmAction("restart")},
		{"POST /v1/vms/{name}/stop", "vm_stop", auth.ScopeOperator, true, s.vmAction("stop")},
		{"POST /v1/vms/{name}/start", "vm_start", auth.ScopeOperator, true, s.vmAction("start")},
		{"GET /v1/nodes", "nodes", auth.ScopeRead, false, s.nodes},
		{"GET /v1/nodes/{name}", "node", auth.ScopeRead, false, s.nodes},
		{"POST /v1/nodes/{name}/cordon", "node_cordon", auth.ScopeOperator, false, s.nodeCordon(true)},
		{"POST /v1/nodes/{name}/uncordon", "node_uncordon", auth.ScopeOperator, false, s.nodeCordon(false)},
		{"GET /v1/pods", "pods", auth.ScopeRead, false, s.pods},
		{"GET /v1/ceph", "ceph", auth.ScopeRead, false, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, s.status.Ceph(r.Context()))
		}},
		{"GET /v1/ceph/osds", "ceph_osds", auth.ScopeRead, false, func(w http.ResponseWriter, r *http.Request) {
			d := s.status.Disks(r.Context())
			writeJSON(w, http.StatusOK, api.List[api.OSD]{Items: d.OSDs, Errors: d.Errors})
		}},
		{"GET /v1/disks", "disks", auth.ScopeRead, false, func(w http.ResponseWriter, r *http.Request) {
			d := s.status.Disks(r.Context())
			writeJSON(w, http.StatusOK, api.List[api.Disk]{Items: d.Disks, Errors: d.Errors})
		}},
		{"GET /v1/filesystems", "filesystems", auth.ScopeRead, false, func(w http.ResponseWriter, r *http.Request) {
			d := s.status.Disks(r.Context())
			writeJSON(w, http.StatusOK, api.List[api.Filesystem]{Items: d.Filesystems, Errors: d.Errors})
		}},
		{"GET /v1/vips", "vips", auth.ScopeRead, false, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, api.List[api.VIPStatus]{Items: s.status.VIPs(r.Context(), callerFrom(r.Context()).raw)})
		}},
		{"GET /v1/s3", "s3", auth.ScopeRead, false, func(w http.ResponseWriter, r *http.Request) {
			st, _ := s.status.S3(r.Context())
			writeJSON(w, http.StatusOK, st)
		}},
		{"GET /v1/s3/buckets", "s3_buckets", auth.ScopeRead, false, s.buckets},
		{"GET /v1/s3/buckets/{name}", "s3_bucket", auth.ScopeRead, false, s.buckets},
		{"GET /v1/operations", "operations", auth.ScopeRead, false, s.operations},
		{"GET /v1/operations/{id}", "operation", auth.ScopeRead, false, s.operation},

		{"GET /v1/metrics/query", "metrics_query", auth.ScopeRead, false, s.metricsRoute("/api/v1/query")},
		{"GET /v1/metrics/query_range", "metrics_query_range", auth.ScopeRead, false, s.metricsRoute("/api/v1/query_range")},
		{"GET /v1/metrics/series", "metrics_series", auth.ScopeRead, false, s.metricsRoute("/api/v1/series")},
		{"GET /v1/metrics/labels", "metrics_labels", auth.ScopeRead, false, s.metricsRoute("/api/v1/labels")},
		{"GET /v1/metrics/label/{name}/values", "metrics_label_values", auth.ScopeRead, false, s.labelValues},

		{"GET /v1/logs/query", "logs_query", auth.ScopeRead, true, s.logsQuery},
		{"GET /v1/logs/hits", "logs_hits", auth.ScopeRead, false, s.logsRoute("/select/logsql/hits")},
		{"GET /v1/logs/streams", "logs_streams", auth.ScopeRead, false, s.logsRoute("/select/logsql/streams")},

		{"/", "not_found", auth.ScopeNone, false, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "not found")
		}},
	}
}

type ctxKey int

const tokenKey ctxKey = 0

type caller struct {
	token auth.Token
	raw   string
}

func callerFrom(ctx context.Context) caller {
	c, _ := ctx.Value(tokenKey).(caller)
	return c
}

// wrap adds request IDs, timeouts, auth, metrics and the access log.
func (s *Server) wrap(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := newRequestID()
		w.Header().Set("X-Request-Id", rid)
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		r.Body = http.MaxBytesReader(rec, r.Body, 1<<20)
		if !rt.stream {
			_ = http.NewResponseController(w).SetWriteDeadline(start.Add(s.cfg.Limits.RequestTimeout.Duration + 5*time.Second))
		}

		var who string
		defer func() {
			code := rec.code
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					// Deliberate (oversized upstream response): let net/http
					// cut the connection once this is logged.
					defer panic(v)
				} else {
					s.log.Error("panic", "request_id", rid, "route", rt.name, "panic", fmt.Sprint(v))
					if !rec.wrote {
						writeError(rec, http.StatusInternalServerError, "internal error")
					}
				}
				code = http.StatusInternalServerError
			}
			d := time.Since(start)
			metrics.GetOrCreateCounter(fmt.Sprintf(`clusterd_requests_total{route=%q,code="%d"}`, rt.name, code)).Inc()
			metrics.GetOrCreateHistogram(fmt.Sprintf(`clusterd_request_duration_seconds{route=%q}`, rt.name)).Update(d.Seconds())
			if rt.name == "health" && code == http.StatusOK {
				return // Keepalived asks every two seconds
			}
			// The path only: query strings can hold log searches.
			s.log.Info("request", "request_id", rid, "method", r.Method, "path", r.URL.Path, "route", rt.name,
				"code", code, "duration_ms", d.Milliseconds(), "token", who, "remote", r.RemoteAddr)
		}()

		if rt.scope != auth.ScopeNone {
			raw, ok := bearer(r)
			if !ok {
				metrics.GetOrCreateCounter(`clusterd_auth_failures_total{reason="missing"}`).Inc()
				rec.Header().Set("WWW-Authenticate", `Bearer realm="clusterd"`)
				writeError(rec, http.StatusUnauthorized, "missing bearer token")
				return
			}
			tok, ok := s.tokens.Lookup(raw)
			if !ok {
				metrics.GetOrCreateCounter(`clusterd_auth_failures_total{reason="invalid"}`).Inc()
				rec.Header().Set("WWW-Authenticate", `Bearer realm="clusterd", error="invalid_token"`)
				writeError(rec, http.StatusUnauthorized, "invalid token")
				return
			}
			who = tok.Name
			if !tok.Scope.Allows(rt.scope) {
				metrics.GetOrCreateCounter(`clusterd_auth_failures_total{reason="scope"}`).Inc()
				writeError(rec, http.StatusForbidden, fmt.Sprintf("token %q has scope %s, this needs %s", tok.Name, tok.Scope, rt.scope))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), tokenKey, caller{token: tok, raw: raw}))
		}
		rt.h(rec, r)
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.code, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and deadlines.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.Error{Error: msg})
}

// Handlers.

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: "ok"})
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	c := callerFrom(r.Context())
	writeJSON(w, http.StatusOK, api.Whoami{
		Site: s.cfg.Site, Host: s.cfg.Host, Name: c.token.Name, Scope: c.token.Scope.String(), Version: version.Version,
	})
}

func (s *Server) node(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status.Node(r.Context()))
}

func (s *Server) statusAll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status.Status(r.Context(), callerFrom(r.Context()).raw))
}

func (s *Server) metricsRoute(upstream string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.metrics.Forward(w, r, proxy.Route{Upstream: upstream}, r.URL.Query())
	}
}

func (s *Server) labelValues(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validLabel(name) {
		writeError(w, http.StatusBadRequest, "invalid label name")
		return
	}
	s.metrics.Forward(w, r, proxy.Route{Upstream: "/api/v1/label/" + url.PathEscape(name) + "/values"}, r.URL.Query())
}

func validLabel(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (s *Server) logsRoute(upstream string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.logs.Forward(w, r, proxy.Route{Upstream: upstream}, r.URL.Query())
	}
}

// logsQuery searches, or follows with ?follow=1 (VictoriaLogs' tail).
func (s *Server) logsQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	follow, _ := strconv.ParseBool(q.Get("follow"))
	q.Del("follow")
	if follow {
		s.logs.Forward(w, r, proxy.Route{Upstream: "/select/logsql/tail", Stream: true}, q)
		return
	}
	// Without a limit a broad query returns everything in the store.
	if q.Get("limit") == "" {
		q.Set("limit", strconv.Itoa(s.cfg.Limits.LogsLimit))
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.cfg.Limits.RequestTimeout.Duration + 5*time.Second))
	s.logs.Forward(w, r, proxy.Route{Upstream: "/select/logsql/query"}, q)
}
