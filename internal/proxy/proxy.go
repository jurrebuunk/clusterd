// Package proxy forwards an allow-list of read-only query endpoints to the
// monitoring stores: this host's store first, the peers' as fallback.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

// Route maps one API path to one upstream path.
type Route struct {
	Upstream string
	// Stream marks long-lived responses (log tailing): flushed as they come,
	// bounded by FollowMax instead of the request timeout.
	Stream bool
}

// Options bound what one request may cost.
type Options struct {
	Timeout   time.Duration
	FollowMax time.Duration
	MaxBytes  int64
}

// Proxy sends requests to one kind of store (VictoriaMetrics or VictoriaLogs).
type Proxy struct {
	name      string
	upstreams []*url.URL // local first
	opts      Options
	client    *http.Client
	log       *slog.Logger
}

// New builds a proxy over upstream base URLs in the order they are tried.
func New(name string, upstreams []string, opts Options, log *slog.Logger) (*Proxy, error) {
	p := &Proxy{name: name, opts: opts, log: log}
	for _, u := range upstreams {
		parsed, err := url.Parse(strings.TrimRight(u, "/"))
		if err != nil {
			return nil, fmt.Errorf("%s upstream %q: %w", name, u, err)
		}
		p.upstreams = append(p.upstreams, parsed)
	}
	if len(p.upstreams) == 0 {
		return nil, fmt.Errorf("%s: no upstreams", name)
	}
	p.client = &http.Client{
		// No client timeout: streams last long; contexts bound every request.
		Transport: &http.Transport{
			ResponseHeaderTimeout: opts.Timeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return p, nil
}

// Forward serves one request on route. query is what is sent upstream.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, route Route, query url.Values) {
	limit := p.opts.Timeout
	if route.Stream {
		limit = p.opts.FollowMax
	}
	ctx, cancel := context.WithTimeout(r.Context(), limit)
	defer cancel()

	var lastErr error
	for i, base := range p.upstreams {
		u := *base
		u.Path = base.Path + route.Upstream
		u.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			lastErr = err
			continue
		}
		// Only harmless headers go upstream; the caller's token never does.
		if a := r.Header.Get("Accept"); a != "" {
			req.Header.Set("Accept", a)
		}
		resp, err := p.client.Do(req)
		if err == nil && resp.StatusCode >= 500 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		if err != nil {
			if errors.Is(r.Context().Err(), context.Canceled) {
				return // the client went away
			}
			lastErr = err
			metrics.GetOrCreateCounter(fmt.Sprintf(`clusterd_upstream_errors_total{store=%q,upstream=%q}`, p.name, base.Host)).Inc()
			p.log.Warn("upstream failed, trying next", "store", p.name, "upstream", base.Host, "err", err)
			continue
		}
		if i > 0 {
			metrics.GetOrCreateCounter(fmt.Sprintf(`clusterd_upstream_fallbacks_total{store=%q}`, p.name)).Inc()
		}
		p.copy(w, resp, route.Stream)
		return
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("no %s store answered: %v", p.name, lastErr))
}

func (p *Proxy) copy(w http.ResponseWriter, resp *http.Response, stream bool) {
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)

	body := io.Reader(resp.Body)
	if !stream {
		body = io.LimitReader(resp.Body, p.opts.MaxBytes+1)
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, err := body.Read(buf)
		if k > 0 {
			n += int64(k)
			if !stream && n > p.opts.MaxBytes {
				// Headers are gone already: cut the connection so the client
				// sees a broken response instead of silently partial data.
				p.log.Warn("response too large, aborted", "store", p.name, "max_bytes", p.opts.MaxBytes)
				panic(http.ErrAbortHandler)
			}
			if _, werr := w.Write(buf[:k]); werr != nil {
				return
			}
			if stream {
				_ = rc.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"error\":%s}\n", strconv.Quote(msg))
}
