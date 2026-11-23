// Package s3admin reads the Ceph RGW admin API (buckets, users), signing
// requests with AWS Signature Version 4 like any S3 client.
package s3admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Credentials of an RGW user.
type Credentials struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// LoadCredentials reads {"access_key": …, "secret_key": …}.
func LoadCredentials(path string) (Credentials, error) {
	var c Credentials
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return c, fmt.Errorf("%s: access_key and secret_key are required", path)
	}
	return c, nil
}

// Region is what RGW accepts in the credential scope.
const Region = "us-east-1"

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// uriEncode encodes like SigV4 wants: everything but unreserved characters.
func uriEncode(s string, slash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Sign adds SigV4 headers to a request without a body. signed lists the
// headers to sign besides host (lower case, must be set on req).
func Sign(req *http.Request, cred Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	payload := req.Header.Get("X-Amz-Content-Sha256")

	headers := map[string]string{"host": req.URL.Host, "x-amz-date": amzDate}
	if payload != "" {
		headers["x-amz-content-sha256"] = payload
	} else {
		payload = sha256Hex(nil)
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, strings.Join(parts, "&"), canonHeaders.String(), signedHeaders, payload}, "\n")

	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	key := hmacSHA256([]byte("AWS4"+cred.SecretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cred.AccessKey, scope, signedHeaders, sig))
}

// Client talks to the admin API of one or more gateways, first one first.
type Client struct {
	Gateways []string // base URLs, e.g. http://10.10.30.11:7480
	Cred     Credentials
	HTTP     *http.Client
	Now      func() time.Time
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	var lastErr error
	for _, gw := range c.Gateways {
		u, err := url.Parse(strings.TrimRight(gw, "/") + path)
		if err != nil {
			return err
		}
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex(nil))
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		Sign(req, c.Cred, Region, "s3", now())
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s: %s", gw, resp.Status)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			// 4xx is about us (credentials, caps), the same on every gateway.
			return fmt.Errorf("rgw admin %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
		}
		return json.Unmarshal(body, out)
	}
	if lastErr == nil {
		lastErr = errors.New("no gateways configured")
	}
	return fmt.Errorf("rgw admin %s: %w", path, lastErr)
}

// BucketStats is what /admin/bucket?stats=true says about one bucket.
type BucketStats struct {
	Bucket       string                 `json:"bucket"`
	Owner        string                 `json:"owner"`
	CreationTime string                 `json:"creation_time"`
	Usage        map[string]BucketUsage `json:"usage"`
	BucketQuota  Quota                  `json:"bucket_quota"`
}

// BucketUsage is one usage category (rgw.main, rgw.multimeta…).
type BucketUsage struct {
	SizeActual int64 `json:"size_actual"`
	NumObjects int64 `json:"num_objects"`
}

// Quota of a bucket; negative or disabled means none.
type Quota struct {
	Enabled    bool  `json:"enabled"`
	MaxSize    int64 `json:"max_size"`
	MaxObjects int64 `json:"max_objects"`
}

// Buckets returns every bucket with its statistics.
func (c *Client) Buckets(ctx context.Context) ([]BucketStats, error) {
	var out []BucketStats
	return out, c.get(ctx, "/admin/bucket", url.Values{"stats": {"true"}, "format": {"json"}}, &out)
}

// Users returns the user IDs.
func (c *Client) Users(ctx context.Context) ([]string, error) {
	var out []string
	return out, c.get(ctx, "/admin/metadata/user", url.Values{"format": {"json"}}, &out)
}
