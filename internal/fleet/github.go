package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/truvity/ci-actions/internal/ghapi"
)

// Client is the slice of the GitHub REST API the fleet scan reads, over the
// shared transport.
type Client struct{ *ghapi.Client }

// ErrNotFound is a 404: a repository without a workflows directory is
// ordinary, not an error.
var ErrNotFound = ghapi.ErrNotFound

// NewClient builds a client against baseURL (empty means the public API).
func NewClient(baseURL, token string) *Client { return &Client{ghapi.New(baseURL, token)} }

func (c *Client) getJSONPages(ctx context.Context, path string, pageFn func(raw json.RawMessage) error) error {
	return c.Pages(ctx, path, func(b []byte) error { return pageFn(json.RawMessage(b)) })
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
	body, _, err := c.Get(ctx, c.contentsURL(repo, dir, ref), "application/vnd.github+json")
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
	body, _, err := c.Get(ctx, c.contentsURL(repo, path, ref), "application/vnd.github.raw+json")
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
	body, _, err := c.Get(ctx, c.BaseURL+"/repos/"+repo+"/git/trees/"+url.PathEscape(ref)+"?recursive=1", "application/vnd.github+json")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	// A repository with no commit answers 409 "Git Repository is empty.":
	// there is nothing in it to pin, which is no error.
	var se *ghapi.StatusError
	if errors.As(err, &se) && se.Code == http.StatusConflict && strings.Contains(strings.ToLower(se.Body), "empty") {
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
