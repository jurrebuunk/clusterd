package kube

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
)

func TestConvertNodes(t *testing.T) {
	raw := `{"items":[
	 {"metadata":{"name":"w1","labels":{"topology.kubernetes.io/zone":"hv03"}},
	  "spec":{"unschedulable":true},
	  "status":{"conditions":[{"type":"Ready","status":"False"}],"nodeInfo":{"kubeletVersion":"v1.35.8+k3s1"}}},
	 {"metadata":{"name":"m1","labels":{"node-role.kubernetes.io/etcd":"true","node-role.kubernetes.io/control-plane":"true"}},
	  "spec":{},
	  "status":{"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"True"}],"nodeInfo":{"kubeletVersion":"v1.35.8+k3s1"}}}]}`
	var list nodeList
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	nodes := convertNodes(list)
	if len(nodes) != 2 || nodes[0].Name != "m1" {
		t.Fatalf("nodes = %+v", nodes)
	}
	m, w := nodes[0], nodes[1]
	if !m.Ready || !m.Schedulable || len(m.Roles) != 2 || m.Roles[0] != "control-plane" {
		t.Fatalf("m1 = %+v", m)
	}
	if w.Ready || w.Schedulable || w.Zone != "hv03" || len(w.Roles) != 0 {
		t.Fatalf("w1 = %+v", w)
	}
}

func TestFailing(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old, fresh := now.Add(-time.Hour), now.Add(-time.Minute)
	recent, longAgo := now.Add(-10*time.Minute), now.Add(-3*time.Hour)
	for _, c := range []struct {
		name string
		p    api.Pod
		want bool
	}{
		{"running", api.Pod{Phase: "Running", Ready: 1, Containers: 1, Created: old}, false},
		{"done", api.Pod{Phase: "Succeeded", Created: old}, false},
		{"failed", api.Pod{Phase: "Failed", Created: old}, true},
		{"crash loop", api.Pod{Phase: "Running", Containers: 1, Reason: "CrashLoopBackOff", Created: fresh}, true},
		{"restarted lately", api.Pod{Phase: "Running", Ready: 1, Containers: 1, LastRestart: &recent, Created: old}, true},
		{"restarted long ago", api.Pod{Phase: "Running", Ready: 1, Containers: 1, LastRestart: &longAgo, Created: old}, false},
		{"just pending", api.Pod{Phase: "Pending", Created: fresh}, false},
		{"stuck pending", api.Pod{Phase: "Pending", Created: old}, true},
		{"not ready for long", api.Pod{Phase: "Running", Containers: 2, Ready: 1, Created: old}, true},
	} {
		if got := Failing(c.p, now); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
