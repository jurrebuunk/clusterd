package s3admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AWS's published SigV4 test vector "get-vanilla".
func TestSignVector(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	Sign(req, Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"},
		"us-east-1", "service", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// AWS test vector "get-vanilla-query-order-key-case": sorted query parameters.
func TestSignQueryOrder(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1", nil)
	Sign(req, Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"},
		"us-east-1", "service", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	if !strings.HasSuffix(req.Header.Get("Authorization"), "Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500") {
		t.Fatalf("got %s", req.Header.Get("Authorization"))
	}
}

func TestClient(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	rgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") || r.Header.Get("X-Amz-Content-Sha256") == "" {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/admin/bucket":
			json.NewEncoder(w).Encode([]map[string]any{{"bucket": "b1", "owner": "app", "creation_time": "2026-11-23T10:00:00.000000Z",
				"usage":        map[string]any{"rgw.main": map[string]any{"size_actual": 4096, "num_objects": 2}},
				"bucket_quota": map[string]any{"enabled": true, "max_size": 1 << 20, "max_objects": -1}}})
		case "/admin/metadata/user":
			json.NewEncoder(w).Encode([]string{"app", "jurre"})
		}
	}))
	defer rgw.Close()
	c := &Client{Gateways: []string{down.URL, rgw.URL}, Cred: Credentials{AccessKey: "AK", SecretKey: "SK"}, HTTP: http.DefaultClient}
	b, err := c.Buckets(context.Background())
	if err != nil || len(b) != 1 || b[0].Usage["rgw.main"].SizeActual != 4096 || !b[0].BucketQuota.Enabled {
		t.Fatalf("buckets = %+v, %v", b, err)
	}
	u, err := c.Users(context.Background())
	if err != nil || len(u) != 2 {
		t.Fatalf("users = %v, %v", u, err)
	}
	bad := &Client{Gateways: []string{rgw.URL}, Cred: Credentials{AccessKey: "XX", SecretKey: "SK"}, HTTP: http.DefaultClient}
	if _, err := bad.Users(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("wrong key: %v", err)
	}
}
