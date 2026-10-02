// Package ghapi is the small GitHub REST client shared by the fleet
// commands: one place that sets the headers, retries, pages, and keeps the
// token out of every error string.
package ghapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Client is the transport of the GitHub REST API the fleet commands read.
//
// The token comes from the caller and is only ever put in the
// Authorization header. Every error string passes through redact, so a
// token that an upstream proxy echoes into a body or a URL cannot reach a
// log.
type Client struct {
	BaseURL string // https://api.github.com
	Token   string
	HTTP    *http.Client
}

// StatusError is an HTTP error answer other than 404, with the (redacted)
// start of its body, so a caller can tell one kind of refusal from another.
type StatusError struct {
	Code int
	Body string
	msg  string
}

func (e *StatusError) Error() string { return e.msg }

// ErrNotFound is a 404: a repository without a workflows directory is
// ordinary, not an error.
var ErrNotFound = errors.New("not found")

// New builds a client against baseURL (empty means the public API).
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Redact(s string) string {
	if c.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Token, "***")
}

func (c *Client) newRequest(ctx context.Context, method, rawURL, accept string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader(body))
	if err != nil {
		return nil, errors.New(c.Redact(err.Error()))
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ci-actions")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// Get is a GET with retries on transport errors and 5xx. A 404 is
// ErrNotFound, any other 4xx an error carrying a redacted excerpt of the
// body. It returns the body and headers of a 2xx/3xx answer.
func (c *Client) Get(ctx context.Context, rawURL, accept string) ([]byte, http.Header, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := c.newRequest(ctx, http.MethodGet, rawURL, accept, nil)
		if err != nil {
			return nil, nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = errors.New(c.Redact(err.Error()))
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = errors.New(c.Redact(rerr.Error()))
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, nil, ErrNotFound
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("GET %s: %s", c.Redact(rawURL), resp.Status)
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
			return nil, nil, fmt.Errorf("GitHub API rate limit exhausted (resets at unix %s)", resp.Header.Get("X-RateLimit-Reset"))
		case resp.StatusCode >= 400:
			return nil, nil, &StatusError{Code: resp.StatusCode, Body: c.Redact(truncate(string(body), 200)),
				msg: fmt.Sprintf("GET %s: %s: %s", c.Redact(rawURL), resp.Status, c.Redact(truncate(string(body), 200)))}
		}
		return body, resp.Header, nil
	}
	return nil, nil, lastErr
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Pages calls pageFn with the body of every page of a list endpoint,
// following the Link header.
func (c *Client) Pages(ctx context.Context, path string, pageFn func(raw []byte) error) error {
	next := c.BaseURL + path
	for next != "" {
		body, hdr, err := c.Get(ctx, next, "application/vnd.github+json")
		if err != nil {
			return err
		}
		if err := pageFn(body); err != nil {
			return err
		}
		next = ""
		if m := nextLink.FindStringSubmatch(hdr.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return nil
}

func bodyReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

// Raw makes ONE request and returns whatever came back, status and body
// both: the caller is deciding between "nothing gates this" (a definitive
// 404 with a message) and "this token may not look" (a 403), which a
// client that turns every 4xx into an error cannot tell apart. A transport
// failure is status 0 and a redacted error.
func (c *Client) Raw(ctx context.Context, method, rawURL string, body []byte) (int, []byte, error) {
	req, err := c.newRequest(ctx, method, rawURL, "application/vnd.github+json", body)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, errors.New(c.Redact(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, errors.New(c.Redact(err.Error()))
	}
	return resp.StatusCode, data, nil
}
