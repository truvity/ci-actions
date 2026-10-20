package devboxparity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/truvity/ci-actions/internal/ghapi"
)

// The pull-request phase talks to GitHub through the REST and GraphQL API,
// with the same client the gates use. It needs no `gh` binary: a runner image
// that does not carry the GitHub CLI (the self-hosted pool, a container task)
// runs it exactly as a hosted runner does.

// pullRequest is what the phase needs of a pull request.
type pullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	NodeID string `json:"node_id"`
}

// apiError turns a refusal into an error that carries a redacted excerpt of
// the answer, never the token.
func apiError(c *ghapi.Client, what string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return fmt.Errorf("%s: HTTP %d: %s", what, status, c.Redact(msg))
}

// call makes one request and decodes a 2xx answer into out (nil to ignore it).
// Any other status is an error.
func call(ctx context.Context, c *ghapi.Client, method, path string, in, out any) (int, error) {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = b
	}
	status, data, err := c.Raw(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return 0, err
	}
	if status < 200 || status > 299 {
		return status, apiError(c, method+" "+path, status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return status, fmt.Errorf("%s %s: unreadable answer: %w", method, path, err)
		}
	}
	return status, nil
}

// ensureLabel is `gh label create --force`: create the label, or bring an
// existing one to the same colour and description.
func ensureLabel(ctx context.Context, c *ghapi.Client, repo, name string) error {
	const color, desc = "0366d6", "Dependency updates"
	status, err := call(ctx, c, "POST", "/repos/"+repo+"/labels",
		map[string]string{"name": name, "color": color, "description": desc}, nil)
	if err == nil {
		return nil
	}
	if status != 422 {
		return err
	}
	// 422 is "already_exists" (the only way this body is refused): update it.
	_, err = call(ctx, c, "PATCH", "/repos/"+repo+"/labels/"+url.PathEscape(name),
		map[string]string{"color": color, "description": desc}, nil)
	return err
}

// openPullRequest returns the URL of the open pull request from branch (in
// the same repository), or "".
func openPullRequest(ctx context.Context, c *ghapi.Client, repo, branch string) (string, error) {
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"head": {owner + ":" + branch}, "state": {"open"}, "per_page": {"1"}}
	var prs []pullRequest
	if _, err := call(ctx, c, "GET", "/repos/"+repo+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return "", err
	}
	if len(prs) == 0 {
		return "", nil
	}
	return prs[0].URL, nil
}

// createPullRequest opens the pull request and puts the label on it.
func createPullRequest(ctx context.Context, c *ghapi.Client, repo, branch, base, title, body, label string) (pullRequest, error) {
	var pr pullRequest
	if _, err := call(ctx, c, "POST", "/repos/"+repo+"/pulls",
		map[string]string{"title": title, "body": body, "head": branch, "base": base}, &pr); err != nil {
		return pr, err
	}
	if _, err := call(ctx, c, "POST", fmt.Sprintf("/repos/%s/issues/%d/labels", repo, pr.Number),
		map[string][]string{"labels": {label}}, nil); err != nil {
		return pr, fmt.Errorf("labelling #%d: %w", pr.Number, err)
	}
	return pr, nil
}

// armAutoMerge is `gh pr merge --auto --rebase`: GitHub merges the pull
// request by rebase once its required checks pass.
func armAutoMerge(ctx context.Context, c *ghapi.Client, nodeID string) error {
	q := map[string]any{
		"query":     "mutation($id:ID!){enablePullRequestAutoMerge(input:{pullRequestId:$id,mergeMethod:REBASE}){pullRequest{number}}}",
		"variables": map[string]string{"id": nodeID},
	}
	var g struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := call(ctx, c, "POST", "/graphql", q, &g); err != nil {
		return err
	}
	if len(g.Errors) > 0 {
		return fmt.Errorf("enablePullRequestAutoMerge: %s", c.Redact(g.Errors[0].Message))
	}
	return nil
}

// repository names the repository the run works on, as owner/name: the origin
// remote of the checkout, else the runner's own GITHUB_REPOSITORY. The remote
// comes first because a fleet job checks out each repository of the fleet, so
// GITHUB_REPOSITORY names the workflow's repository, not the one being worked on.
func (r *run) repository(ctx context.Context) (string, error) {
	repo, err := r.originRepository(ctx)
	if err == nil {
		return repo, nil
	}
	if v := envLookup(r.o.Environ, "GITHUB_REPOSITORY"); strings.Count(v, "/") == 1 {
		return v, nil
	}
	return "", err
}

// originRepository reads owner/name from the origin remote of the checkout.
func (r *run) originRepository(ctx context.Context) (string, error) {
	var out strings.Builder
	if err := r.cmd(ctx, ".", &out, r.stderr(), "git", "remote", "get-url", "origin"); err != nil {
		return "", fmt.Errorf("git remote get-url: %w", err)
	}
	u := strings.TrimSpace(out.String())
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		u = u[strings.Index(u+"/", "/")+1:]
	} else if i := strings.Index(u, ":"); i >= 0 {
		u = u[i+1:]
	}
	parts := strings.Split(strings.Trim(u, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("cannot read owner/name from the origin remote")
	}
	return strings.Join(parts[len(parts)-2:], "/"), nil
}
