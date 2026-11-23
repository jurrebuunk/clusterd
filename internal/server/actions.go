package server

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jurrebuunk/clusterd/internal/actions"
	"github.com/jurrebuunk/clusterd/internal/config"
)

// forwardedHeader marks a request one instance passed to another, so it is
// never passed on twice.
const forwardedHeader = "X-Clusterd-Forwarded-By"

func peerClient(cfg *config.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:       &tls.Config{ServerName: cfg.ServerName, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}}
}

// vmRestart: ?dry_run=1 returns the plan; otherwise the restart runs and its
// progress streams back as JSON lines. A request for a VM on another host is
// passed to that host's clusterd with the caller's token.
func (s *Server) vmRestart(w http.ResponseWriter, r *http.Request) {
	if s.actions == nil {
		writeError(w, http.StatusServiceUnavailable, "actions need Kubernetes access, which is not configured")
		return
	}
	name := r.PathValue("name")
	vm, err := s.actions.VM(name)
	if err != nil {
		s.actionError(w, err)
		return
	}
	if vm.Host != s.cfg.Host {
		if by := r.Header.Get(forwardedHeader); by != "" {
			writeError(w, http.StatusMisdirectedRequest, name+" is not on "+s.cfg.Host+" (forwarded by "+by+")")
			return
		}
		s.forward(w, r, vm.Host)
		return
	}

	c := callerFrom(r.Context())
	caller := actions.Caller{Token: c.token.Name, Scope: c.token.Scope.String(), RequestID: w.Header().Get("X-Request-Id")}
	if dry, _ := strconv.ParseBool(r.URL.Query().Get("dry_run")); dry {
		plan, _, err := s.actions.PlanVMRestart(r.Context(), name)
		if err != nil {
			s.actionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, plan)
		return
	}

	run, err := s.actions.StartVMRestart(r.Context(), name, caller)
	if err != nil {
		s.actionError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	enc := json.NewEncoder(w)
	for ev := range run.Events() {
		if err := enc.Encode(ev); err != nil {
			// The client left; the action carries on and is in the journal.
			return
		}
		_ = rc.Flush()
	}
}

func (s *Server) actionError(w http.ResponseWriter, err error) {
	var busy *actions.BusyError
	switch {
	case errors.Is(err, actions.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, actions.ErrPrecondition):
		writeError(w, http.StatusPreconditionFailed, err.Error())
	case errors.As(err, &busy):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("action failed to start", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// forward passes the request to host's clusterd and streams the answer back.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, host string) {
	var target *config.Host
	for i := range s.cfg.Hosts {
		if s.cfg.Hosts[i].Name == host {
			target = &s.cfg.Hosts[i]
		}
	}
	if target == nil {
		writeError(w, http.StatusInternalServerError, "unknown host "+host)
		return
	}
	u := strings.TrimRight(target.API, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	req.Header.Set(forwardedHeader, s.cfg.Host)
	resp, err := s.peers.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, host+" is unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("X-Clusterd-Executed-By", host)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 8<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}
