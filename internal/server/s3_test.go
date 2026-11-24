package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/auth"
)

func TestS3Writes(t *testing.T) {
	h := newActionHarness(t)
	op, mg := h.tokens[auth.ScopeOperator], h.tokens[auth.ScopeManage]

	// Reads never show keys; the API's own user is left out.
	w := do(h.handler, "GET", "/v1/s3/users", op)
	var users api.List[api.S3User]
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &users) != nil || len(users.Items) != 2 || strings.Contains(w.Body.String(), "SK") {
		t.Fatalf("users: %d %s", w.Code, w.Body.String())
	}
	if users.Items[0].Name != "app" || users.Items[0].Buckets != 1 || users.Items[0].Objects != 40 || users.Items[0].Keys != 1 {
		t.Errorf("app = %+v", users.Items[0])
	}
	if w := do(h.handler, "GET", "/v1/s3/users/clusterd", op); w.Code != 404 {
		t.Errorf("own user: %d", w.Code)
	}

	// Writes need manage.
	if w := doBody(h.handler, "POST", "/v1/s3/users", op, `{"name":"web"}`); w.Code != 403 {
		t.Fatalf("operator create: %d", w.Code)
	}
	for _, body := range []string{`{"name":"Web!"}`, `{"name":"clusterd"}`, `{"name":"web","admin":true}`, `nope`} {
		if w := doBody(h.handler, "POST", "/v1/s3/users", mg, body); w.Code != 400 {
			t.Errorf("create %s: %d", body, w.Code)
		}
	}
	w = doBody(h.handler, "POST", "/v1/s3/users", mg, `{"name":"web"}`)
	var cred api.S3Credentials
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &cred) != nil || cred.User != "web" || cred.SecretKey == "" ||
		cred.Endpoint != "https://s3.test" || w.Header().Get("Location") != "/v1/s3/users/web" {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := doBody(h.handler, "POST", "/v1/s3/users", mg, `{"name":"web"}`); w.Code != 409 {
		t.Errorf("create twice: %d", w.Code)
	}

	// A bucket for it, with a quota; the owner's own key creates it.
	if w := doBody(h.handler, "POST", "/v1/s3/buckets", mg, `{"name":"web-assets","owner":"nobody"}`); w.Code != 400 {
		t.Errorf("unknown owner: %d", w.Code)
	}
	w = doBody(h.handler, "POST", "/v1/s3/buckets", mg, `{"name":"web-assets","owner":"web","quota_bytes":5000000000}`)
	var b api.S3Bucket
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &b) != nil || b.Owner != "web" || b.QuotaBytes != 5e9 {
		t.Fatalf("bucket create: %d %s", w.Code, w.Body.String())
	}
	if w := doBody(h.handler, "POST", "/v1/s3/buckets", mg, `{"name":"web-assets","owner":"web"}`); w.Code != 409 {
		t.Errorf("bucket twice: %d %s", w.Code, w.Body.String())
	}
	w = doBody(h.handler, "PATCH", "/v1/s3/buckets/web-assets", mg, `{"quota_bytes":0}`)
	if json.Unmarshal(w.Body.Bytes(), &b) != nil || w.Code != 200 || b.QuotaBytes != 0 {
		t.Fatalf("quota off: %d %s", w.Code, w.Body.String())
	}

	// A user that owns buckets stays; rotation swaps the key.
	if w := do(h.handler, "DELETE", "/v1/s3/users/web", mg); w.Code != 412 || !strings.Contains(w.Body.String(), "web-assets") {
		t.Errorf("delete owner: %d %s", w.Code, w.Body.String())
	}
	old := h.rgw.UserKeys("web")
	w = do(h.handler, "POST", "/v1/s3/users/web/keys", mg)
	var nc api.S3Credentials
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &nc) != nil || nc.AccessKey == old[0] {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if keys := h.rgw.UserKeys("web"); len(keys) != 1 || keys[0] != nc.AccessKey {
		t.Errorf("keys after rotation: %v", keys)
	}

	// Buckets with objects need force.
	if w := do(h.handler, "DELETE", "/v1/s3/buckets/backups", mg); w.Code != 412 {
		t.Errorf("delete full bucket: %d", w.Code)
	}
	if w := do(h.handler, "DELETE", "/v1/s3/buckets/backups?force=true", mg); w.Code != 204 {
		t.Errorf("force delete: %d %s", w.Code, w.Body.String())
	}
	if w := do(h.handler, "DELETE", "/v1/s3/buckets/web-assets", mg); w.Code != 204 {
		t.Errorf("delete empty: %d", w.Code)
	}
	if w := do(h.handler, "DELETE", "/v1/s3/users/web", mg); w.Code != 204 {
		t.Errorf("delete user: %d %s", w.Code, w.Body.String())
	}
	if w := do(h.handler, "GET", "/v1/s3/users/web", op); w.Code != 404 {
		t.Errorf("deleted user: %d", w.Code)
	}
}
