package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Client is the small slice of the GitHub REST API this tool reads.
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

// ErrNotFound is a 404: a repository without a workflows directory is
// ordinary, not an error.
var ErrNotFound = errors.New("not found")

// NewClient builds a client against baseURL (empty means the public API).
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) redact(s string) string {
	if c.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Token, "***")
}

func (c *Client) newRequest(ctx context.Context, rawURL, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ci-actions-fleet-pins")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, rawURL, accept string) ([]byte, http.Header, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := c.newRequest(ctx, rawURL, accept)
		if err != nil {
			return nil, nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = errors.New(c.redact(err.Error()))
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = errors.New(c.redact(rerr.Error()))
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, nil, ErrNotFound
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("GET %s: %s", c.redact(rawURL), resp.Status)
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
			return nil, nil, fmt.Errorf("GitHub API rate limit exhausted (resets at unix %s)", resp.Header.Get("X-RateLimit-Reset"))
		case resp.StatusCode >= 400:
			return nil, nil, fmt.Errorf("GET %s: %s: %s", c.redact(rawURL), resp.Status, c.redact(truncate(string(body), 200)))
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

// getJSONPages decodes every page of a list endpoint into out, which must
// be a pointer to a slice; each page is appended.
func (c *Client) getJSONPages(ctx context.Context, path string, pageFn func(raw json.RawMessage) error) error {
	next := c.BaseURL + path
	for next != "" {
		body, hdr, err := c.do(ctx, next, "application/vnd.github+json")
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

// Repo is the part of a repository the fleet scan needs.
type Repo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	DefaultBranch string `json:"default_branch"`
	Visibility    string `json:"visibility"`
}

// ListRepos returns the repositories of an organisation (or, failing
// that, a user account).
func (c *Client) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	var repos []Repo
	collect := func(raw json.RawMessage) error {
		var page []Repo
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		repos = append(repos, page...)
		return nil
	}
	q := "?per_page=100&type=all"
	err := c.getJSONPages(ctx, "/orgs/"+url.PathEscape(owner)+"/repos"+q, collect)
	if errors.Is(err, ErrNotFound) {
		repos = nil
		err = c.getJSONPages(ctx, "/users/"+url.PathEscape(owner)+"/repos"+q, collect)
	}
	return repos, err
}

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

// ListDir lists a directory of a repository at a ref. A missing directory
// is an empty list.
func (c *Client) ListDir(ctx context.Context, repo, dir, ref string) ([]string, error) {
	body, _, err := c.do(ctx, c.contentsURL(repo, dir, ref), "application/vnd.github+json")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []dirEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("listing %s/%s: %w", repo, dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.Type == "file" {
			names = append(names, e.Path)
		}
	}
	return names, nil
}

// File reads one file of a repository at a ref (a branch, tag or sha).
func (c *Client) File(ctx context.Context, repo, path, ref string) ([]byte, error) {
	body, _, err := c.do(ctx, c.contentsURL(repo, path, ref), "application/vnd.github.raw+json")
	return body, err
}

func (c *Client) contentsURL(repo, path, ref string) string {
	segs := strings.Split(path, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	u := c.BaseURL + "/repos/" + repo + "/contents/" + strings.Join(segs, "/")
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	return u
}

// Tag is one tag of a library with the COMMIT it names.
type Tag struct {
	Name   string
	Commit string
}

// Tags lists a repository's tags with their commits. The list endpoint
// dereferences annotated tags, which is the sha a `uses:` pin names.
func (c *Client) Tags(ctx context.Context, repo string) ([]Tag, error) {
	var tags []Tag
	err := c.getJSONPages(ctx, "/repos/"+repo+"/tags?per_page=100", func(raw json.RawMessage) error {
		var page []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, p := range page {
			tags = append(tags, Tag{Name: p.Name, Commit: p.Commit.SHA})
		}
		return nil
	})
	return tags, err
}

// Tree lists every file path of a repository at a ref with one request. A
// missing repository or ref is an empty list; a tree GitHub truncated is an
// error, because a scan that cannot see a file must not pass the repository.
func (c *Client) Tree(ctx context.Context, repo, ref string) ([]string, error) {
	body, _, err := c.do(ctx, c.BaseURL+"/repos/"+repo+"/git/trees/"+url.PathEscape(ref)+"?recursive=1", "application/vnd.github+json")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("listing the tree of %s: %w", repo, err)
	}
	if t.Truncated {
		return nil, fmt.Errorf("the tree of %s is too large for one listing", repo)
	}
	var out []string
	for _, e := range t.Tree {
		if e.Type == "blob" {
			out = append(out, e.Path)
		}
	}
	return out, nil
}
