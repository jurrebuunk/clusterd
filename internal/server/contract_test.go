package server

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jurrebuunk/clusterd/internal/auth"
)

// The contract test keeps the handwritten server and api/openapi.yaml in
// step: the same operations with the same scopes, and real responses that
// match their schemas.

type spec struct {
	doc map[string]any
}

func loadSpec(t *testing.T) spec {
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return spec{doc}
}

func (s spec) operations() map[string]string {
	ops := map[string]string{}
	for path, item := range s.doc["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			m := strings.ToUpper(method)
			if m != "GET" && m != "POST" && m != "PUT" && m != "PATCH" && m != "DELETE" {
				continue
			}
			scope := "none"
			if sc, ok := op.(map[string]any)["x-scope"].(string); ok {
				scope = sc
			}
			ops[m+" "+path] = scope
		}
	}
	return ops
}

func (s spec) schema(name string) map[string]any {
	return s.doc["components"].(map[string]any)["schemas"].(map[string]any)[name].(map[string]any)
}

func (s spec) resolve(sch map[string]any) map[string]any {
	if ref, ok := sch["$ref"].(string); ok {
		return s.schema(strings.TrimPrefix(ref, "#/components/schemas/"))
	}
	return sch
}

// check validates v against sch: types, required fields and no fields the
// schema doesn't declare.
func (s spec) check(path string, sch map[string]any, v any) []string {
	sch = s.resolve(sch)
	var errs []string
	typ, _ := sch["type"].(string)
	switch typ {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want object, got %T", path, v)}
		}
		props, _ := sch["properties"].(map[string]any)
		for k, val := range obj {
			p, ok := props[k]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.%s: not in the spec", path, k))
				continue
			}
			errs = append(errs, s.check(path+"."+k, p.(map[string]any), val)...)
		}
		if req, ok := sch["required"].([]any); ok {
			for _, r := range req {
				if _, ok := obj[r.(string)]; !ok {
					errs = append(errs, fmt.Sprintf("%s.%s: required but missing", path, r))
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want array, got %T", path, v)}
		}
		items, _ := sch["items"].(map[string]any)
		for i, it := range arr {
			errs = append(errs, s.check(fmt.Sprintf("%s[%d]", path, i), items, it)...)
		}
	case "string":
		if _, ok := v.(string); !ok {
			errs = append(errs, fmt.Sprintf("%s: want string, got %T", path, v))
		}
	case "integer", "number":
		if _, ok := v.(float64); !ok {
			errs = append(errs, fmt.Sprintf("%s: want number, got %T", path, v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			errs = append(errs, fmt.Sprintf("%s: want boolean, got %T", path, v))
		}
	}
	return errs
}

func TestContractOperations(t *testing.T) {
	sp := loadSpec(t)
	h := newActionHarness(t)
	got := map[string]string{}
	for _, r := range h.srv.Routes() {
		got[r.Method+" "+r.Path] = r.Scope
	}
	want := sp.operations()
	var problems []string
	for op, scope := range want {
		if s, ok := got[op]; !ok {
			problems = append(problems, "in the spec, not in the server: "+op)
		} else if s != scope {
			problems = append(problems, fmt.Sprintf("%s: server scope %s, spec %s", op, s, scope))
		}
	}
	for op := range got {
		if _, ok := want[op]; !ok {
			problems = append(problems, "in the server, not in the spec: "+op)
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

func TestContractResponses(t *testing.T) {
	sp := loadSpec(t)
	h := newActionHarness(t)
	tok := h.tokens[auth.ScopeOperator]
	// The progress stream: every line is an ActionEvent.
	w := do(h.handler, "POST", "/v1/vms/w1/restart?wait=true", tok)
	for _, line := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("stream line %q: %v", line, err)
		}
		for _, e := range sp.check("event", sp.schema("ActionEvent"), v) {
			t.Error(e)
		}
	}
	for _, c := range []struct{ method, path, schema string }{
		{"GET", "/v1/health", "Health"},
		{"GET", "/v1/whoami", "Whoami"},
		{"GET", "/v1/internal/node", "Node"},
		{"GET", "/v1/status", "Status"},
		{"GET", "/v1/checks", "Checks"},
		{"GET", "/v1/hosts", "HostList"},
		{"GET", "/v1/hosts/hv1", "HostStatus"},
		{"GET", "/v1/vms", "VMList"},
		{"GET", "/v1/vms/w1", "VMStatus"},
		{"GET", "/v1/nodes", "NodeList"},
		{"GET", "/v1/nodes/w1", "K3sNode"},
		{"GET", "/v1/ceph", "CephStatus"},
		{"GET", "/v1/ceph/osds", "OSDList"},
		{"GET", "/v1/disks", "DiskList"},
		{"GET", "/v1/filesystems", "FilesystemList"},
		{"GET", "/v1/vips", "VIPList"},
		{"GET", "/v1/s3", "S3Status"},
		{"GET", "/v1/s3/buckets", "BucketList"},
		{"GET", "/v1/s3/buckets/backups", "S3Bucket"},
		{"GET", "/v1/operations", "OperationList"},
		{"GET", "/v1/operations/aaaaaaaaaaaaaaaa", "Operation"},
		{"POST", "/v1/vms/w1/stop?dry_run=true", "RestartPlan"},
		{"POST", "/v1/vms/w1/restart?dry_run=true", "RestartPlan"},
		{"POST", "/v1/nodes/w1/uncordon", "Operation"},
		{"GET", "/v1/nope", "Error"},
		{"GET", "/v1/vms/nope", "Error"},
		{"GET", "/v1/operations/0000000000000000", "Error"},
		{"POST", "/v1/vms/nope/restart", "Error"},
		{"POST", "/v1/vms/w1/restart", "Operation"}, // starts it: keep last
	} {
		w := do(h.handler, c.method, c.path, tok)
		var v any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Errorf("%s %s: not JSON: %v", c.method, c.path, err)
			continue
		}
		for _, e := range sp.check(c.path, sp.schema(c.schema), v) {
			t.Error(e)
		}
	}
	waitIdle(t, h, tok)
}
