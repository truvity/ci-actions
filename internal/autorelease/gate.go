// Package autorelease is the port of the shared auto-release workflow's two
// shell blocks: the gate that decides whether a push to the default branch
// releases now or waits for Monday's batch, and the step that cuts the next
// patch tag (writing the CHANGELOG heading through a pull request first when
// the release needs one).
//
// git, gh and devbox stay the executed programs; the rules around them are
// Go, table-tested against a fake gh and a real git repository.
package autorelease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// ErrReported marks a failure whose ::error:: line is already written.
var ErrReported = errors.New("auto-release: failed")

var (
	conv = regexp.MustCompile(`^[A-Za-z]+(\([^)]*\))?!?: `)
	fix  = regexp.MustCompile(`^fix(\([^)]*\))?!?: `)
	reno = regexp.MustCompile(`(?i)renovate`)
)

// GateOptions are the gate step's environment.
type GateOptions struct {
	Repo    string // REPO
	SHA     string // SHA
	Summary string // GITHUB_STEP_SUMMARY
	Output  string // GITHUB_OUTPUT
	Out     io.Writer
	Err     io.Writer
	Exec    runcmd.Exec
}

type user struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u user) renovate() bool { return u.Type == "Bot" && reno.MatchString(u.Login) }

type pull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	User   user   `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (p pull) has(label string) bool {
	for _, l := range p.Labels {
		if l.Name == label {
			return true
		}
	}
	return false
}

func appendTo(path, line string) error {
	if path == "" {
		path = os.DevNull
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Gate decides release (nothing written to GITHUB_OUTPUT) or batch
// (`skip=true`), and says why in the step summary. Security-labelled merges
// and fixes release; everything else waits. An error means a gh call failed.
func Gate(ctx context.Context, o GateOptions) error {
	if o.Exec == nil {
		o.Exec = runcmd.OS
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	gh := func(args ...string) ([]byte, error) {
		var b bytes.Buffer
		err := o.Exec(ctx, runcmd.Cmd{Name: "gh", Args: append([]string{"api"}, args...), Stdout: &b, Stderr: o.Err})
		return b.Bytes(), err
	}
	release := func(why string) error {
		return appendTo(o.Summary, why+" — releasing outside the weekly batch")
	}

	raw, err := gh(fmt.Sprintf("repos/%s/commits/%s/pulls", o.Repo, o.SHA))
	if err != nil {
		return fmt.Errorf("gh api: %w", err)
	}
	var prs []pull
	if err := json.Unmarshal(raw, &prs); err != nil {
		return fmt.Errorf("the pulls of %s: %w", o.SHA, err)
	}

	if len(prs) > 0 {
		// Security lane: any associated PR labelled `security`.
		for _, p := range prs {
			if p.has("security") {
				return release("security-labeled merge")
			}
		}
		// Fix lane, for each PR that renovate did not open. A conventional
		// title decides alone; only an unconventional one defers to the
		// PR's commits, any one of which being a fix releases.
		for _, p := range prs {
			if p.User.renovate() || p.has("dependencies") {
				continue
			}
			if fix.MatchString(p.Title) {
				return release(fmt.Sprintf("fix merge (#%d)", p.Number))
			} else if conv.MatchString(p.Title) {
				continue
			}
			out, err := gh(fmt.Sprintf("repos/%s/pulls/%d/commits", o.Repo, p.Number), "--paginate",
				"--jq", `.[].commit.message | split("\n")[0]`)
			if err != nil {
				return fmt.Errorf("gh api: %w", err)
			}
			for _, subject := range strings.Split(string(out), "\n") {
				if fix.MatchString(subject) {
					return release(fmt.Sprintf("fix commit in unconventionally titled merge (#%d)", p.Number))
				}
			}
		}
	} else {
		// No PR (direct push): the head commit's subject, same rules.
		raw, err := gh(fmt.Sprintf("repos/%s/commits/%s", o.Repo, o.SHA))
		if err != nil {
			return fmt.Errorf("gh api: %w", err)
		}
		var head struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
			Author user `json:"author"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return fmt.Errorf("the head commit %s: %w", o.SHA, err)
		}
		subject, _, _ := strings.Cut(head.Commit.Message, "\n")
		if fix.MatchString(subject) && !head.Author.renovate() {
			return release("fix commit pushed directly")
		}
	}

	if o.Output != "" {
		if err := appendTo(o.Output, "skip=true"); err != nil {
			return err
		}
	}
	return appendTo(o.Summary, "push without a security label or a hand-written fix — Monday's batch carries it")
}
