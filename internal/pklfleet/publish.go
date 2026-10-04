package pklfleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// PublishOptions are the "Open or update the pull request" step's environment.
type PublishOptions struct {
	Common
	Token        string
	Repo         string
	Base         string
	API          string
	Source       string
	Version      string
	From         string
	Breaking     string // "true" or not
	DirsFile     string
	BranchPrefix string
	DryRun       string // "true" or not
	AutoMerge    string // "true" or not
	GitUser      string
	GitEmail     string
	HTTP         *http.Client
}

type api struct {
	o      *PublishOptions
	http   *http.Client
	accept string // the Accept header; empty is application/vnd.github+json
}

// do is the curl -fsS call: an HTTP error status is an error.
func (a api) do(ctx context.Context, method, url string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false) // jq -c does not escape < > &
		if err := enc.Encode(body); err != nil {
			return nil, err
		}
		r = bytes.NewReader(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.o.Token)
	accept := a.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %s", method, url, resp.Status)
	}
	return b, nil
}

// obj keeps jq's key order, which is the order the request bodies are built in.
type obj []kv
type kv struct {
	k string
	v any
}

func (o obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		b.Write(k)
		b.WriteByte(':')
		var vb bytes.Buffer
		enc := json.NewEncoder(&vb)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(e.v); err != nil {
			return nil, err
		}
		b.Write(bytes.TrimSuffix(vb.Bytes(), []byte("\n")))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type pr struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	NodeID  string `json:"node_id"`
}

// Publish commits the rewritten tree to the version's branch, pushes it (only
// when the content differs from the branch already there), opens or updates
// the pull request, arms auto-merge for a bump that is not breaking, and
// closes older versions' pull requests as superseded.
func Publish(ctx context.Context, o PublishOptions) error {
	o.init()
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	a := api{o: &o, http: o.HTTP}
	run := func(env []string, name string, args ...string) (string, error) { return o.out(ctx, env, name, args...) }
	_, name, _ := strings.Cut(o.Source, "/")
	branch := o.BranchPrefix + o.Version
	from := strings.ReplaceAll(o.From, ",", ", v")

	if st, err := run(nil, "git", "status", "--porcelain"); err != nil {
		return fmt.Errorf("git status: %w", err)
	} else if st == "" {
		fmt.Fprintln(o.Out, "the tree is unchanged — nothing to propose")
		return nil
	}
	if o.DryRun == "true" {
		fmt.Fprintf(o.Out, "dry run: would push %s and open a pull request with:\n", branch)
		for _, c := range [][]string{{"status", "--short"}, {"diff", "--stat"}} {
			s, err := run(nil, "git", c...)
			if err != nil {
				return fmt.Errorf("git %s: %w", c[0], err)
			}
			fmt.Fprint(o.Out, s)
		}
		f := o.From
		if f == "" {
			f = "?"
		}
		return appendFile(o.Summary, fmt.Sprintf("- %s: dry run, %s -> %s\n", o.Repo, f, o.Version))
	}

	// The token is presented to git for these commands only, through the
	// environment: it is in no config file and no argument.
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + o.Token))
	fmt.Fprintf(o.Out, "::add-mask::%s\n", basic)
	gitx := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basic}

	title := fmt.Sprintf("chore(deps): update %s to v%s", name, o.Version)
	for _, c := range [][]string{{"checkout", "-q", "-B", branch}, {"add", "-A"}} {
		if _, err := run(nil, "git", c...); err != nil {
			return fmt.Errorf("git %s: %w", c[0], err)
		}
	}
	if _, err := run(nil, "git", "-c", "user.name="+o.GitUser, "-c", "user.email="+o.GitEmail, "commit", "-q",
		"-m", title, "-m", fmt.Sprintf("Moves the dependencies on %s from v%s to v%s, resolves PklProject.deps.json and regenerates what the repository generates.", o.Source, from, o.Version)); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	// Push only when the content differs from the branch already there, so a
	// poll that finds an open pull request does not force a new commit under
	// it, and its checks, every hour.
	same := false
	if _, err := run(gitx, "git", "fetch", "-q", "--depth=1", "origin", "refs/heads/"+branch); err == nil {
		head, e1 := run(nil, "git", "rev-parse", "HEAD^{tree}")
		fetched, e2 := run(nil, "git", "rev-parse", "FETCH_HEAD^{tree}")
		same = e1 == nil && e2 == nil && head == fetched
	}
	if same {
		fmt.Fprintf(o.Out, "%s already holds exactly this change — not pushed again\n", branch)
	} else if _, err := run(gitx, "git", "push", "-q", "--force", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	label := func(n, colour, desc string) { // 422: it exists
		_, _ = a.do(ctx, "POST", o.API+"/repos/"+o.Repo+"/labels", obj{{"name", n}, {"color", colour}, {"description", desc}})
	}
	labels := []string{"dependencies"}
	label("dependencies", "0366d6", "Dependency updates")
	if o.Breaking == "true" {
		labels = append(labels, "major")
		label("major", "b60205", "Major version update")
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Moves the Pkl contracts packages (%s) from v%s to **v%s** in:\n\n", o.Source, from, o.Version)
	dirs, err := os.ReadFile(o.DirsFile)
	if err != nil {
		return err
	}
	for _, d := range strings.Split(string(dirs), "\n") {
		if d != "" {
			fmt.Fprintf(&body, "- `%s`\n", d)
		}
	}
	body.WriteString("\n`PklProject.deps.json` is resolved again, and the repository's `generate` recipe has run where it has one, so committed generated files follow the new contract.\n\n")
	fmt.Fprintf(&body, "Release notes: https://github.com/%s/releases/tag/v%s\n\n", o.Source, o.Version)
	switch {
	case o.Breaking == "true":
		body.WriteString("This bump is **breaking** (a new major, or a new minor below 1.0): it is not auto-merged. Read the release notes' breaking entries, fix what they name, and merge by hand.")
	case o.AutoMerge == "true":
		body.WriteString("Not breaking: auto-merge is armed and GitHub still holds the merge until every required check passes.")
	default:
		body.WriteString("Not breaking, but auto-merge is not armed on this run: merge when the checks are green.")
	}
	body.WriteString("\n\nOpened by the fleet's pkl job.")

	owner, _, _ := strings.Cut(o.Repo, "/")
	raw, err := a.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/pulls?state=open&head=%s:%s&per_page=100", o.API, o.Repo, owner, branch), nil)
	if err != nil {
		return err
	}
	var open []pr
	if err := json.Unmarshal(raw, &open); err != nil {
		return fmt.Errorf("open pull requests: %w", err)
	}
	var cur pr
	if len(open) > 0 {
		cur = open[0]
		if _, err := a.do(ctx, "PATCH", fmt.Sprintf("%s/repos/%s/pulls/%d", o.API, o.Repo, cur.Number), obj{{"title", title}, {"body", body.String()}}); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "pull request #%d for %s updated\n", cur.Number, branch)
	} else {
		raw, err := a.do(ctx, "POST", o.API+"/repos/"+o.Repo+"/pulls", obj{{"title", title}, {"body", body.String()}, {"head", branch}, {"base", o.Base}})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &cur); err != nil {
			return fmt.Errorf("the new pull request: %w", err)
		}
		fmt.Fprintf(o.Out, "pull request opened: %s\n", cur.HTMLURL)
	}
	number := cur.Number
	if _, err := a.do(ctx, "POST", fmt.Sprintf("%s/repos/%s/issues/%d/labels", o.API, o.Repo, number), obj{{"labels", labels}}); err != nil {
		return err
	}

	// Auto-merge only where there is a green to wait for, and never for a
	// breaking bump. A refusal (auto-merge off in the repository's settings)
	// leaves the pull request for a human.
	if o.Breaking != "true" && o.AutoMerge == "true" {
		raw, err := a.do(ctx, "POST", o.API+"/graphql", obj{
			{"query", "mutation($id:ID!){enablePullRequestAutoMerge(input:{pullRequestId:$id,mergeMethod:REBASE}){clientMutationId}}"},
			{"variables", obj{{"id", cur.NodeID}}},
		})
		if err != nil {
			return err
		}
		var res struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		var generic map[string]json.RawMessage
		_ = json.Unmarshal(raw, &generic)
		if e, ok := generic["errors"]; ok && string(bytes.TrimSpace(e)) != "null" && string(bytes.TrimSpace(e)) != "false" {
			_ = json.Unmarshal(raw, &res)
			msg := []byte("null")
			if len(res.Errors) > 0 {
				msg, _ = json.Marshal(res.Errors[0].Message)
			}
			fmt.Fprintf(o.Out, "::warning::%s#%d: auto-merge could not be armed: %s\n", o.Repo, number, msg)
		} else {
			fmt.Fprintf(o.Out, "auto-merge armed on #%d\n", number)
		}
	} else {
		fmt.Fprintf(o.Out, "auto-merge not armed on #%d\n", number)
	}

	// An open pull request for an older version of the same library is
	// superseded by this one.
	raw, err = a.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/pulls?state=open&per_page=100", o.API, o.Repo), nil)
	if err != nil {
		return err
	}
	var all []struct {
		Number int `json:"number"`
		Head   struct {
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return fmt.Errorf("open pull requests: %w", err)
	}
	for _, p := range all {
		if p.Head.Repo == nil || p.Head.Repo.FullName != o.Repo || !strings.HasPrefix(p.Head.Ref, o.BranchPrefix) || p.Head.Ref == branch {
			continue
		}
		if _, err := a.do(ctx, "POST", fmt.Sprintf("%s/repos/%s/issues/%d/comments", o.API, o.Repo, p.Number), obj{{"body", fmt.Sprintf("Superseded by #%d (v%s).", number, o.Version)}}); err != nil {
			return err
		}
		if _, err := a.do(ctx, "PATCH", fmt.Sprintf("%s/repos/%s/pulls/%d", o.API, o.Repo, p.Number), obj{{"state", "closed"}}); err != nil {
			return err
		}
		_, _ = a.do(ctx, "DELETE", fmt.Sprintf("%s/repos/%s/git/refs/heads/%s", o.API, o.Repo, p.Head.Ref), nil)
		fmt.Fprintf(o.Out, "closed #%d (%s), superseded\n", p.Number, p.Head.Ref)
	}

	if err := appendFile(o.Output, "url="+cur.HTMLURL+"\n"); err != nil {
		return err
	}
	return appendFile(o.Summary, fmt.Sprintf("- %s: %s\n", o.Repo, cur.HTMLURL))
}
