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
	"github.com/jurrebuunk/clusterd/internal/api"
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

// vmAction serves restart, stop and start: ?dry_run=1 returns the plan;
// otherwise the action runs and its progress streams back as JSON lines. A
// request for a VM on another host is passed to that host's clusterd with
// the caller's token.
func (s *Server) vmAction(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			s.forwardTo(w, r, vm.Host, name)
			return
		}
		caller := s.caller(w, r)
		if dry, _ := strconv.ParseBool(r.URL.Query().Get("dry_run")); dry {
			var plan any
			switch kind {
			case "restart":
				plan, _, err = s.actions.PlanVMRestart(r.Context(), name)
			case "stop":
				plan, err = s.actions.PlanVMStop(r.Context(), name)
			case "start":
				plan, err = s.actions.PlanVMStart(r.Context(), name)
			}
			if err != nil {
				s.actionError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, plan)
			return
		}
		var run *actions.Run
		switch kind {
		case "restart":
			run, err = s.actions.StartVMRestart(r.Context(), name, caller)
		case "stop":
			run, err = s.actions.StartVMStop(r.Context(), name, caller)
		case "start":
			run, err = s.actions.StartVMStart(r.Context(), name, caller)
		}
		if err != nil {
			s.actionError(w, err)
			return
		}
		s.stream(w, run)
	}
}

// hostAction serves maintenance and resume, on the host itself.
func (s *Server) hostAction(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.actions == nil {
			writeError(w, http.StatusServiceUnavailable, "actions need Kubernetes access, which is not configured")
			return
		}
		host := r.PathValue("name")
		known := false
		for _, h := range s.cfg.Hosts {
			known = known || h.Name == host
		}
		if !known {
			writeError(w, http.StatusNotFound, "no host "+strconv.Quote(host))
			return
		}
		if host != s.cfg.Host {
			s.forwardTo(w, r, host, host)
			return
		}
		q := r.URL.Query()
		reboot, _ := strconv.ParseBool(q.Get("reboot"))
		caller := s.caller(w, r)
		if dry, _ := strconv.ParseBool(q.Get("dry_run")); dry {
			var plan any
			var err error
			if kind == "maintenance" {
				plan, err = s.actions.PlanHostMaintenance(r.Context(), host, reboot)
			} else {
				plan, err = s.actions.PlanHostResume(r.Context(), host)
			}
			if err != nil {
				s.actionError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, plan)
			return
		}
		var run *actions.Run
		var err error
		if kind == "maintenance" {
			run, err = s.actions.StartHostMaintenance(r.Context(), host, reboot, caller)
		} else {
			run, err = s.actions.StartHostResume(r.Context(), host, caller)
		}
		if err != nil {
			s.actionError(w, err)
			return
		}
		go func() {
			// What status shows about maintenance is cached; refresh it.
			run.Wait()
			s.status.Invalidate()
		}()
		s.stream(w, run)
	}
}

// nodeCordon cordons or uncordons a node; no forwarding, no lock.
func (s *Server) nodeCordon(cordon bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.actions == nil {
			writeError(w, http.StatusServiceUnavailable, "actions need Kubernetes access, which is not configured")
			return
		}
		msg, err := s.actions.SetCordon(r.Context(), r.PathValue("name"), cordon, s.caller(w, r))
		if err != nil {
			s.actionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, api.ActionEvent{Time: time.Now().UTC(), Status: api.EventDone, Message: msg})
	}
}

func (s *Server) caller(w http.ResponseWriter, r *http.Request) actions.Caller {
	c := callerFrom(r.Context())
	return actions.Caller{Token: c.token.Name, Scope: c.token.Scope.String(), RequestID: w.Header().Get("X-Request-Id")}
}

// forwardTo passes the request to host, unless it was forwarded already.
func (s *Server) forwardTo(w http.ResponseWriter, r *http.Request, host, what string) {
	if by := r.Header.Get(forwardedHeader); by != "" {
		writeError(w, http.StatusMisdirectedRequest, what+" is not on "+s.cfg.Host+" (forwarded by "+by+")")
		return
	}
	s.forward(w, r, host)
}

// stream writes the action's events as JSON lines until it ends or the
// client leaves (the action carries on regardless).
func (s *Server) stream(w http.ResponseWriter, run *actions.Run) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	enc := json.NewEncoder(w)
	for ev := range run.Events() {
		if err := enc.Encode(ev); err != nil {
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
