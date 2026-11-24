package kube

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
)

type allPodList struct {
	Items []struct {
		Metadata struct {
			Namespace         string    `json:"namespace"`
			Name              string    `json:"name"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase             string            `json:"phase"`
			Reason            string            `json:"reason"`
			ContainerStatuses []containerStatus `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type containerStatus struct {
	Ready        bool `json:"ready"`
	RestartCount int  `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
	} `json:"state"`
	LastState struct {
		Terminated *struct {
			FinishedAt time.Time `json:"finishedAt"`
		} `json:"terminated"`
	} `json:"lastState"`
}

// Pods lists every pod in the cluster with how it is doing.
func (c *Client) Pods(ctx context.Context) ([]api.Pod, error) {
	var list allPodList
	if err := c.get(ctx, "/api/v1/pods", &list); err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]api.Pod, 0, len(list.Items))
	for _, it := range list.Items {
		p := api.Pod{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, Node: it.Spec.NodeName,
			Phase: it.Status.Phase, Reason: it.Status.Reason, Created: it.Metadata.CreationTimestamp,
			Containers: len(it.Status.ContainerStatuses)}
		for _, cs := range it.Status.ContainerStatuses {
			if cs.Ready {
				p.Ready++
			}
			p.Restarts += cs.RestartCount
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				p.Reason = w.Reason
			}
			if t := cs.LastState.Terminated; t != nil && !t.FinishedAt.IsZero() && (p.LastRestart == nil || t.FinishedAt.After(*p.LastRestart)) {
				f := t.FinishedAt
				p.LastRestart = &f
			}
		}
		p.Failing = Failing(p, now)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// failingReasons are container waits that don't fix themselves.
var failingReasons = []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "CreateContainerError", "InvalidImageName"}

// Failing says whether a pod needs a look; see api.Pod.Failing.
func Failing(p api.Pod, now time.Time) bool {
	settled := now.Sub(p.Created) > 5*time.Minute
	switch {
	case p.Phase == "Succeeded":
		return false
	case p.Phase == "Failed", p.Phase == "Unknown":
		return true
	case slices.Contains(failingReasons, p.Reason):
		return true
	case p.LastRestart != nil && now.Sub(*p.LastRestart) < time.Hour:
		return true
	case p.Phase == "Pending" && settled:
		return true
	case p.Phase == "Running" && p.Ready < p.Containers && settled:
		return true
	}
	return false
}
