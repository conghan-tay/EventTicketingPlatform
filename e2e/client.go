//go:build e2e

// Package e2e contains the end-to-end test harness.
//
// Tests speak HTTP against a running app rather than calling Go functions directly,
// so routing, serialisation, validation and auth are all genuinely exercised. The
// suite doubles as the project's progress dashboard: as features land, more tests
// turn green.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// BaseURL is where the running app is expected to be listening.
func BaseURL() string {
	if v := os.Getenv("E2E_BASE_URL"); v != "" {
		return v
	}
	return "http://localhost:4000"
}

// Client is a thin typed HTTP client for the API under test.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient() *Client {
	return &Client{
		baseURL: BaseURL(),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// AsUser returns a copy of the client authenticated as the given user. Identity is
// carried in the Authorization header only; the API never trusts a user id in a body.
func (c *Client) AsUser(uid string) *Client {
	cp := *c
	cp.token = uid
	return &cp
}

// Anonymous returns a copy of the client with no credentials.
func (c *Client) Anonymous() *Client {
	cp := *c
	cp.token = ""
	return &cp
}

// Response is a decoded HTTP response.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// DecodeInto unmarshals the response body into v.
func (r *Response) DecodeInto(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decode %q: %w", truncate(r.Body), err)
	}
	return nil
}

// APIError is Encore's error envelope, used to assert on business error codes.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details"`
}

// Error returns the decoded error envelope, or a synthetic one if the body is not
// a recognisable Encore error.
func (r *Response) Error() APIError {
	var e APIError
	if err := json.Unmarshal(r.Body, &e); err != nil || e.Code == "" {
		return APIError{Code: "unknown", Message: string(truncate(r.Body))}
	}
	return e
}

func (c *Client) Get(path string, query url.Values) (*Response, error) {
	if len(query) > 0 {
		path = path + "?" + query.Encode()
	}
	return c.do(http.MethodGet, path, nil, nil)
}

func (c *Client) Post(path string, body any) (*Response, error) {
	return c.do(http.MethodPost, path, body, nil)
}

// PostWithHeaders is used for endpoints carrying an Idempotency-Key or hold token.
func (c *Client) PostWithHeaders(path string, body any, headers map[string]string) (*Response, error) {
	return c.do(http.MethodPost, path, body, headers)
}

func (c *Client) Delete(path string) (*Response, error) {
	return c.do(http.MethodDelete, path, nil, nil)
}

func (c *Client) do(method, path string, body any, headers map[string]string) (*Response, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return &Response{Status: resp.StatusCode, Body: raw, Header: resp.Header}, nil
}

// WaitForHealth blocks until the app responds to /health or the deadline passes.
func (c *Client) WaitForHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		resp, err := c.Get("/health", nil)
		if err == nil && resp.Status == http.StatusOK {
			return nil
		}
		last = err
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("app not healthy within %s: %v", timeout, last)
}

func truncate(b []byte) []byte {
	const max = 512
	if len(b) > max {
		return append(b[:max:max], []byte("...")...)
	}
	return b
}
