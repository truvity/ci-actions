// Package publicrunners is the port of public-runners/public-runners.sh: a
// PUBLIC repository's jobs run on GitHub-hosted runners, always.
//
// GitHub's own guidance is blunt about why: self-hosted runners should
// almost never be used for public repositories, because any user can open
// pull requests against the repository and compromise the environment. A
// fork's pull request receives no secrets and no id-token, but the RUNNER
// is the exposure, not the token: the job inherits whatever the runner's
// identity can reach, and it writes the shared build caches TRUSTED jobs
// read afterwards. A poisoned cache entry is a supply-chain compromise no
// ephemerality undoes.
//
// The runner group's "allow public repositories" setting is the hard stop
// and stays off. This is the second lock: a public repository that asks for
// a self-hosted tier, by copying a private repository's caller, is told so
// in seconds, on a hosted runner, before anything of the estate's has run.
//
// Output lines and exit status are the shell script's, byte for byte.
package publicrunners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ErrRefused is returned when the guard says no: the visibility is unknown
// or a public repository asked for a self-hosted runner. The reason has
// already been written to Out as an ::error:: line.
var ErrRefused = errors.New("public-runners: refused")

// Options configures a run.
type Options struct {
	Runners    string // whitespace-separated labels; empty entries ignored
	Visibility string // normally github.event.repository.visibility; empty asks the API
	Repository string // owner/repo, GITHUB_REPOSITORY
	Token      string // the job's own token, for the API fallback
	APIURL     string // GitHub API base URL; empty means the public API
	HTTP       *http.Client
	Out        io.Writer
}

// hosted labels are the three platforms GitHub runs. Everything else is
// somebody's own machine, including this estate's tiers and GitHub's own
// larger runners, which are billed even for a public repository.
var hostedRe = regexp.MustCompile(`^(ubuntu|windows|macos)-`)

// Run applies the rule.
func Run(ctx context.Context, o Options) error {
	visibility := o.Visibility

	// The event payload carries the visibility for every webhook this
	// library is called from, but not for every event that exists. Asking
	// the API costs one request and makes the guard independent of the
	// payload shape; `contents: read`, which every caller has, is enough.
	if visibility == "" {
		visibility = o.lookupVisibility(ctx)
	}
	if visibility == "" {
		fmt.Fprintf(o.Out, "::error::cannot tell whether %s is public; refusing to guess. Pass visibility: ${{ github.event.repository.visibility }}.\n", o.Repository)
		return ErrRefused
	}
	if visibility != "public" {
		fmt.Fprintf(o.Out, "%s is %s — any runner is allowed\n", o.Repository, visibility)
		return nil
	}

	fail := false
	for _, label := range strings.Fields(o.Runners) {
		if hostedRe.MatchString(label) {
			fmt.Fprintf(o.Out, "ok        %s\n", label)
		} else {
			fmt.Fprintf(o.Out, "SELF-HOSTED  %s\n", label)
			fail = true
		}
	}
	if fail {
		fmt.Fprintf(o.Out, "::error::%s is public, so its jobs run on GitHub-hosted runners only. A fork's code must never execute on the estate's own infrastructure. Drop the runner inputs and take the hosted default; if the work genuinely needs the estate, run it from a private repository that checks this one out at a pinned version.\n", o.Repository)
		return ErrRefused
	}
	fmt.Fprintln(o.Out, "public repository, hosted runners only — checked")
	return nil
}

// lookupVisibility is `gh api repos/<repo> --jq .visibility`, with any
// failure read as "unknown" (the caller then refuses to guess).
func (o Options) lookupVisibility(ctx context.Context) string {
	if o.Repository == "" {
		return ""
	}
	base := strings.TrimRight(o.APIURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+o.Repository, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ci-actions-public-runners")
	if o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Visibility string `json:"visibility"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return ""
	}
	return body.Visibility
}
