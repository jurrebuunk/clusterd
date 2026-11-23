// Package kubefake is an in-memory stand-in for the Kubernetes API, shared
// by the actions and server tests.
package kubefake

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/kube"
)

// Kube is an in-memory Kubernetes API for tests: nodes m1–m3 (etcd) and w1,
// two evictable pods, a DaemonSet pod and a finished pod on w1.
type Kube struct {
	Mu        sync.Mutex
	NodeMap   map[string]*api.K3sNode
	PodMap    map[string][]kube.Pod // by node
	Gone      map[string]bool       // pod UID
	PDBBlock  map[string]int        // pod name → 429s left
	EvictErr  error
	LeaseObj  *kube.Lease
	RV        int
	ReadyzErr error
}

func New() *Kube {
	f := &Kube{NodeMap: map[string]*api.K3sNode{}, PodMap: map[string][]kube.Pod{}, Gone: map[string]bool{}, PDBBlock: map[string]int{}}
	for _, n := range []struct {
		name string
		etcd bool
	}{{"m1", true}, {"m2", true}, {"m3", true}, {"w1", false}} {
		roles := []string{}
		if n.etcd {
			roles = []string{"control-plane", "etcd"}
		}
		f.NodeMap[n.name] = &api.K3sNode{Name: n.name, Ready: true, Schedulable: true, Roles: roles, BootID: "boot-1"}
	}
	f.PodMap["w1"] = []kube.Pod{
		{Namespace: "apps", Name: "web-1", UID: "u1", Phase: "Running", OwnerKind: "ReplicaSet"},
		{Namespace: "apps", Name: "web-2", UID: "u2", Phase: "Running", OwnerKind: "ReplicaSet"},
		{Namespace: "kube-system", Name: "flannel", UID: "u3", Phase: "Running", OwnerKind: "DaemonSet"},
		{Namespace: "apps", Name: "job-done", UID: "u4", Phase: "Succeeded"},
	}
	return f
}

func (f *Kube) Nodes(ctx context.Context) ([]api.K3sNode, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []api.K3sNode
	for _, n := range f.NodeMap {
		out = append(out, *n)
	}
	slices.SortFunc(out, func(a, b api.K3sNode) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
func (f *Kube) Node(ctx context.Context, name string) (api.K3sNode, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return *f.NodeMap[name], nil
}
func (f *Kube) SetUnschedulable(ctx context.Context, name string, v bool) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.NodeMap[name].Schedulable = !v
	return nil
}
func (f *Kube) PodsOnNode(ctx context.Context, node string) ([]kube.Pod, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return slices.Clone(f.PodMap[node]), nil
}
func (f *Kube) Evict(ctx context.Context, p kube.Pod) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.EvictErr != nil {
		return f.EvictErr
	}
	if f.PDBBlock[p.Name] > 0 {
		f.PDBBlock[p.Name]--
		return &kube.StatusError{Code: http.StatusTooManyRequests}
	}
	f.Gone[p.UID] = true
	return nil
}
func (f *Kube) PodGone(ctx context.Context, p kube.Pod) (bool, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Gone[p.UID], nil
}
func (f *Kube) Readyz(ctx context.Context) error { return f.ReadyzErr }
func (f *Kube) GetLease(ctx context.Context, ns, name string) (*kube.Lease, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.LeaseObj == nil {
		return nil, nil
	}
	c := *f.LeaseObj
	return &c, nil
}
func (f *Kube) CreateLease(ctx context.Context, l *kube.Lease) (*kube.Lease, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.LeaseObj != nil {
		return nil, &kube.StatusError{Code: http.StatusConflict}
	}
	f.RV++
	c := *l
	c.Metadata.ResourceVersion = strconv.Itoa(f.RV)
	f.LeaseObj = &c
	out := c
	return &out, nil
}
func (f *Kube) UpdateLease(ctx context.Context, l *kube.Lease) (*kube.Lease, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.LeaseObj == nil || f.LeaseObj.Metadata.ResourceVersion != l.Metadata.ResourceVersion {
		return nil, &kube.StatusError{Code: http.StatusConflict}
	}
	f.RV++
	c := *l
	c.Metadata.ResourceVersion = strconv.Itoa(f.RV)
	f.LeaseObj = &c
	out := c
	return &out, nil
}
func (f *Kube) DeleteLease(ctx context.Context, l *kube.Lease) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.LeaseObj != nil && f.LeaseObj.Metadata.ResourceVersion == l.Metadata.ResourceVersion {
		f.LeaseObj = nil
	}
	return nil
}
