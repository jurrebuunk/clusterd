package server

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/status"
)

// Collections answer {"items": […]}; with {name} in the path the same
// handler answers that one item, or 404.

// one writes the item called name from items, or 404 when there is none.
func one[T any](w http.ResponseWriter, kind, name string, items []T, nameOf func(T) string) {
	for _, it := range items {
		if nameOf(it) == name {
			writeJSON(w, http.StatusOK, it)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no "+kind+" "+strconv.Quote(name))
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	items := s.status.Hosts(r.Context(), callerFrom(r.Context()).raw)
	if name := r.PathValue("name"); name != "" {
		one(w, "host", name, items, func(h api.HostStatus) string { return h.Name })
		return
	}
	writeJSON(w, http.StatusOK, api.List[api.HostStatus]{Items: items})
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	items := s.status.VMs(r.Context(), callerFrom(r.Context()).raw)
	if name := r.PathValue("name"); name != "" {
		one(w, "VM", name, items, func(v api.VMStatus) string { return v.Name })
		return
	}
	writeJSON(w, http.StatusOK, api.List[api.VMStatus]{Items: items})
}

// nodes lists Kubernetes nodes. When the k3s API can't be reached the list
// is empty and says why; one node then answers 503 rather than 404.
func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	k := s.status.K3s(r.Context())
	name := r.PathValue("name")
	if name != "" && !k.Reachable {
		writeError(w, http.StatusServiceUnavailable, "k3s API unreachable: "+k.Error)
		return
	}
	if name != "" {
		one(w, "node", name, k.Nodes, func(n api.K3sNode) string { return n.Name })
		return
	}
	l := api.List[api.K3sNode]{Items: k.Nodes}
	if l.Items == nil {
		l.Items = []api.K3sNode{}
	}
	if !k.Reachable {
		l.Errors = []string{"k3s API unreachable: " + k.Error}
	}
	writeJSON(w, http.StatusOK, l)
}

// pods lists Kubernetes pods; ?failing=true keeps only those that need a
// look.
func (s *Server) pods(w http.ResponseWriter, r *http.Request) {
	pods, err := s.status.Pods(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, api.List[api.Pod]{Items: []api.Pod{}, Errors: []string{err.Error()}})
		return
	}
	if flag(r, "failing") {
		pods = slices.DeleteFunc(pods, func(p api.Pod) bool { return !p.Failing })
	}
	writeJSON(w, http.StatusOK, api.List[api.Pod]{Items: pods})
}

func (s *Server) buckets(w http.ResponseWriter, r *http.Request) {
	st, items := s.status.S3(r.Context())
	if name := r.PathValue("name"); name != "" {
		one(w, "bucket", name, items, func(b api.S3Bucket) string { return b.Name })
		return
	}
	writeJSON(w, http.StatusOK, api.List[api.S3Bucket]{Items: items, Errors: st.Errors})
}

// operations lists past and running actions: ?since= (Go duration, default
// 24h), ?target= and ?limit= (default 100).
func (s *Server) operations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since := 24 * time.Hour
	if v := q.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 31*24*time.Hour {
			writeError(w, http.StatusBadRequest, "since: a duration up to 744h, e.g. 24h")
			return
		}
		since = d
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "limit: 1 to 1000")
			return
		}
		limit = n
	}
	ops, err := s.status.Operations(r.Context(), since, q.Get("target"), limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.List[api.Operation]{Items: ops})
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	op, err := s.status.Operation(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, status.ErrNoOperation):
		writeError(w, http.StatusNotFound, "no operation "+strconv.Quote(r.PathValue("id"))+" (it may not have reached the logs yet)")
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, op)
	}
}
