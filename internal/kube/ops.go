package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// Node returns one node, including its boot ID.
func (c *Client) Node(ctx context.Context, name string) (api.K3sNode, error) {
	var n nodeItem
	if err := c.get(ctx, "/api/v1/nodes/"+url.PathEscape(name), &n); err != nil {
		return api.K3sNode{}, err
	}
	return convertNodes(nodeList{Items: []nodeItem{n}})[0], nil
}

// SetUnschedulable cordons (true) or uncordons (false) a node.
func (c *Client) SetUnschedulable(ctx context.Context, name string, v bool) error {
	body := map[string]any{"spec": map[string]any{"unschedulable": v}}
	return c.do(ctx, http.MethodPatch, "/api/v1/nodes/"+url.PathEscape(name), "application/merge-patch+json", body, nil)
}

// Pod is what a drain needs to know about a pod.
type Pod struct {
	Namespace string
	Name      string
	UID       string
	Phase     string
	// OwnerKind is the kind of the controlling owner, "" for a bare pod.
	OwnerKind string
	Mirror    bool
}

type podList struct {
	Items []struct {
		Metadata struct {
			Namespace       string            `json:"namespace"`
			Name            string            `json:"name"`
			UID             string            `json:"uid"`
			Annotations     map[string]string `json:"annotations"`
			OwnerReferences []struct {
				Kind       string `json:"kind"`
				Controller bool   `json:"controller"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

// PodsOnNode lists every pod scheduled on the node.
func (c *Client) PodsOnNode(ctx context.Context, node string) ([]Pod, error) {
	var list podList
	q := url.Values{"fieldSelector": {"spec.nodeName=" + node}}
	if err := c.get(ctx, "/api/v1/pods?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	out := make([]Pod, 0, len(list.Items))
	for _, it := range list.Items {
		p := Pod{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, UID: it.Metadata.UID, Phase: it.Status.Phase}
		_, p.Mirror = it.Metadata.Annotations["kubernetes.io/config.mirror"]
		for _, o := range it.Metadata.OwnerReferences {
			if o.Controller {
				p.OwnerKind = o.Kind
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// PodGone reports whether the pod no longer exists (or was replaced).
func (c *Client) PodGone(ctx context.Context, p Pod) (bool, error) {
	var cur struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	err := c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(p.Namespace)+"/pods/"+url.PathEscape(p.Name), &cur)
	if IsStatus(err, http.StatusNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return cur.Metadata.UID != p.UID, nil
}

// Evict asks the API to evict a pod, respecting PodDisruptionBudgets. A 429
// answer (budget exhausted) comes back as a StatusError the caller retries.
func (c *Client) Evict(ctx context.Context, p Pod) error {
	body := map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]any{"name": p.Name, "namespace": p.Namespace},
	}
	err := c.do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+url.PathEscape(p.Namespace)+"/pods/"+url.PathEscape(p.Name)+"/eviction",
		"application/json", body, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// Readyz checks the API server's readiness endpoint.
func (c *Client) Readyz(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/readyz", "", nil, nil)
}

// Lease is a coordination.k8s.io/v1 Lease, used as a lock.
type Lease struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		ResourceVersion string `json:"resourceVersion,omitempty"`
	} `json:"metadata"`
	Spec struct {
		HolderIdentity       string    `json:"holderIdentity"`
		LeaseDurationSeconds int       `json:"leaseDurationSeconds"`
		AcquireTime          MicroTime `json:"acquireTime"`
		RenewTime            MicroTime `json:"renewTime"`
	} `json:"spec"`
}

// MicroTime is Kubernetes' RFC 3339 time with microseconds.
type MicroTime struct{ time.Time }

func (t MicroTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.UTC().Format("2006-01-02T15:04:05.000000Z07:00") + `"`), nil
}

func (t *MicroTime) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	v, err := time.Parse(`"`+time.RFC3339Nano+`"`, string(b))
	t.Time = v
	return err
}

func leasePath(ns, name string) string {
	return fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s", url.PathEscape(ns), url.PathEscape(name))
}

// GetLease returns the lease, or nil when it doesn't exist.
func (c *Client) GetLease(ctx context.Context, ns, name string) (*Lease, error) {
	var l Lease
	err := c.get(ctx, leasePath(ns, name), &l)
	if IsStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// CreateLease creates it; a 409 means someone else was faster.
func (c *Client) CreateLease(ctx context.Context, l *Lease) (*Lease, error) {
	l.APIVersion, l.Kind = "coordination.k8s.io/v1", "Lease"
	var out Lease
	path := fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases", url.PathEscape(l.Metadata.Namespace))
	return &out, c.do(ctx, http.MethodPost, path, "application/json", l, &out)
}

// UpdateLease replaces it; the resourceVersion makes this compare-and-swap
// (409 when it changed meanwhile).
func (c *Client) UpdateLease(ctx context.Context, l *Lease) (*Lease, error) {
	l.APIVersion, l.Kind = "coordination.k8s.io/v1", "Lease"
	var out Lease
	return &out, c.do(ctx, http.MethodPut, leasePath(l.Metadata.Namespace, l.Metadata.Name), "application/json", l, &out)
}

// DeleteLease removes it if it still has this resourceVersion.
func (c *Client) DeleteLease(ctx context.Context, l *Lease) error {
	body := map[string]any{"preconditions": map[string]any{"resourceVersion": l.Metadata.ResourceVersion}}
	err := c.do(ctx, http.MethodDelete, leasePath(l.Metadata.Namespace, l.Metadata.Name), "application/json", body, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
