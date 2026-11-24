// Package s3admin reads the Ceph RGW admin API (buckets, users), signing
// requests with AWS Signature Version 4 like any S3 client.
package s3admin

import (
	"bytes"
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
	"strconv"
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

// StatusError is a 4xx answer: about the request (a missing user, a name
// taken, missing caps), so the same on every gateway.
type StatusError struct {
	Path   string
	Status int
	Code   string // RGW's error code, e.g. NoSuchUser, BucketAlreadyExists
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rgw %s: %d %s", e.Path, e.Status, e.Body)
}

// IsStatus reports whether err is an RGW answer with that HTTP status.
func IsStatus(err error, status int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, c.Cred, out)
}

// do sends one signed request to the first gateway that answers; 5xx and
// connection errors try the next. out may be nil.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body []byte, cred Credentials, out any) error {
	var lastErr error
	for _, gw := range c.Gateways {
		u, err := url.Parse(strings.TrimRight(gw, "/") + path)
		if err != nil {
			return err
		}
		u.RawQuery = q.Encode()
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
		if err != nil {
			return err
		}
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		Sign(req, cred, Region, "s3", now())
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		rb, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s: %s", gw, resp.Status)
			continue
		}
		if resp.StatusCode/100 != 2 {
			var e struct {
				Code string `json:"Code"`
			}
			json.Unmarshal(rb, &e)
			if e.Code == "" {
				// S3 API errors are XML: <Error><Code>…</Code>.
				if _, after, ok := strings.Cut(string(rb), "<Code>"); ok {
					e.Code, _, _ = strings.Cut(after, "</Code>")
				}
			}
			return &StatusError{Path: path, Status: resp.StatusCode, Code: e.Code, Body: strings.TrimSpace(string(rb[:min(len(rb), 200)]))}
		}
		if out == nil || len(rb) == 0 {
			return nil
		}
		return json.Unmarshal(rb, out)
	}
	if lastErr == nil {
		lastErr = errors.New("no gateways configured")
	}
	return fmt.Errorf("rgw %s: %w", path, lastErr)
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

// Key is one S3 key pair of a user.
type Key struct {
	User      string `json:"user"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// UserInfo is /admin/user?uid=…&stats=true. It holds the user's secret keys:
// never pass it on as is.
type UserInfo struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Suspended   int    `json:"suspended"`
	Keys        []Key  `json:"keys"`
	Stats       *struct {
		SizeActual int64 `json:"size_actual"`
		NumObjects int64 `json:"num_objects"`
	} `json:"stats"`
}

// User returns one user with usage statistics.
func (c *Client) User(ctx context.Context, uid string) (UserInfo, error) {
	var u UserInfo
	return u, c.get(ctx, "/admin/user", url.Values{"uid": {uid}, "stats": {"true"}, "format": {"json"}}, &u)
}

// CreateUser creates a user with one generated S3 key.
func (c *Client) CreateUser(ctx context.Context, uid, displayName string) (UserInfo, error) {
	var u UserInfo
	q := url.Values{"uid": {uid}, "display-name": {displayName}, "key-type": {"s3"}, "generate-key": {"true"}, "format": {"json"}}
	return u, c.do(ctx, http.MethodPut, "/admin/user", q, nil, c.Cred, &u)
}

// DeleteUser removes a user. It fails while the user owns buckets.
func (c *Client) DeleteUser(ctx context.Context, uid string) error {
	return c.do(ctx, http.MethodDelete, "/admin/user", url.Values{"uid": {uid}, "purge-data": {"false"}}, nil, c.Cred, nil)
}

// NewKey generates another S3 key for the user and returns all its keys.
func (c *Client) NewKey(ctx context.Context, uid string) ([]Key, error) {
	var keys []Key
	q := url.Values{"key": {""}, "uid": {uid}, "key-type": {"s3"}, "generate-key": {"true"}, "format": {"json"}}
	return keys, c.do(ctx, http.MethodPut, "/admin/user", q, nil, c.Cred, &keys)
}

// DeleteKey removes one of the user's keys.
func (c *Client) DeleteKey(ctx context.Context, uid, accessKey string) error {
	q := url.Values{"key": {""}, "uid": {uid}, "access-key": {accessKey}, "key-type": {"s3"}}
	return c.do(ctx, http.MethodDelete, "/admin/user", q, nil, c.Cred, nil)
}

// CreateBucket creates a bucket through the S3 API as its owner: the admin
// API has no bucket creation, and this way the owner is right from the start.
func (c *Client) CreateBucket(ctx context.Context, owner Credentials, bucket string) error {
	return c.do(ctx, http.MethodPut, "/"+url.PathEscape(bucket), nil, nil, owner, nil)
}

// DeleteBucket removes a bucket; with purge, the objects in it as well.
func (c *Client) DeleteBucket(ctx context.Context, bucket string, purge bool) error {
	return c.do(ctx, http.MethodDelete, "/admin/bucket", url.Values{"bucket": {bucket}, "purge-objects": {strconv.FormatBool(purge)}}, nil, c.Cred, nil)
}

// SetBucketQuota sets a size limit in bytes on a bucket; 0 removes it. The
// settings go as query parameters: RGW refuses a SigV4-signed body for this
// admin call (501 NotImplemented), but reads the same settings from the query
// when there is no body.
func (c *Client) SetBucketQuota(ctx context.Context, owner, bucket string, maxBytes int64) error {
	q := url.Values{"quota": {""}, "uid": {owner}, "bucket": {bucket},
		"enabled": {strconv.FormatBool(maxBytes > 0)}, "max-size": {"-1"}, "max-objects": {"-1"}}
	if maxBytes > 0 {
		q.Set("max-size", strconv.FormatInt(maxBytes, 10))
	}
	return c.do(ctx, http.MethodPut, "/admin/bucket", q, nil, c.Cred, nil)
}
