// Package client is the Go client for the clusterd API, used by labctl.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/version"
)

// APIError is a non-2xx answer from the API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// Unauthorized reports whether the token was missing, invalid or not allowed.
func (e *APIError) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// UnreachableError means no HTTP answer came back at all.
type UnreachableError struct{ Err error }

func (e *UnreachableError) Error() string { return "API unreachable: " + e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// Client calls one clusterd API.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New returns a client for base (e.g. https://api.lan.buunk.org).
func New(base, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid API URL %q", base)
	}
	return &Client{base: u, token: token, http: &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}}, nil
}

// Do sends a GET and returns the raw response body; the caller closes it.
func (c *Client) Do(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
	return c.Send(ctx, http.MethodGet, path, q)
}

// Send is Do with any method.
func (c *Client) Send(ctx context.Context, method, path string, q url.Values) (io.ReadCloser, error) {
	resp, err := c.Request(ctx, method, path, q)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Call sends in as JSON (when not nil) and decodes the answer into out
// (when not nil and there is one).
func (c *Client) Call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.request(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Request sends a request and returns the response of a 2xx answer (the
// caller closes its body); other answers become an *APIError.
func (c *Client) Request(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	return c.request(ctx, method, path, q, nil)
}

func (c *Client) request(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "labctl/"+version.Version)
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, &UnreachableError{Err: err}
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e api.Error
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

// Get decodes a JSON response into out.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	body, err := c.Do(ctx, path, q)
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(body).Decode(out)
}

func (c *Client) Whoami(ctx context.Context) (api.Whoami, error) {
	var w api.Whoami
	return w, c.Get(ctx, "/v1/whoami", nil, &w)
}

func (c *Client) Status(ctx context.Context) (api.Status, error) {
	var s api.Status
	return s, c.Get(ctx, "/v1/status", nil, &s)
}
