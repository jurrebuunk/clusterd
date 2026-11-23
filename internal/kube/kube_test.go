package kube

import (
	"encoding/json"
	"testing"
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
