// Package releasegate is the port of the private release workflow's "Require
// green checks on the tagged commit" step: a release builds only from a commit
// whose every check, other than the release jobs themselves, is green.
package releasegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrNotGreen marks a refusal whose ::error:: lines are already written.
var ErrNotGreen = errors.New("release-gate: checks are not green")

// Options are the step's environment.
type Options struct {
	Token string // GH_TOKEN
	Repo  string // GITHUB_REPOSITORY
	SHA   string // GITHUB_SHA: the tagged commit
	RunID string // GITHUB_RUN_ID: this run, whose own check is not a prerequisite
	API   string // default https://api.github.com
	HTTP  *http.Client
	Out   io.Writer

	// Wait bounds how long the gate waits for the tagged commit's checks to
	// finish. A tag is usually pushed right after its commit lands, so the
	// commit's own `check` is still queued or running when the release job
	// starts; judging it at once would refuse every such release. Zero judges
	// once, immediately. Poll is the pause between looks (default 25s) and
	// Sleep the clock behind it (default time.Sleep with context), both
	// replaceable in tests.
	Wait  time.Duration
	Poll  time.Duration
	Sleep func(ctx context.Context, d time.Duration) error
}

type checkRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	DetailsURL string  `json:"details_url"`
	Conclusion *string `json:"conclusion"`
}

type page struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []checkRun `json:"check_runs"`
}

// Lines is the gate's filter, over every page of check runs, as
// `name<TAB>conclusion` lines sorted by name. The newest run of each name
// counts (max id and never started_at: a QUEUED run has started_at=null, which
// sorts lowest, and the gate would pick an older COMPLETED run and release on
// stale green). Three exclusions, all load-bearing:
//
//  1. Its own run, BY RUN ID, not by name: the check run is named
//     "<caller-job> / <called-job>" once the job lives in a reusable
//     workflow, and a name filter stopped matching, so every release refused
//     itself. The run id cannot drift with a job name.
//  2. SIBLING release runs, by name. When two projects tag the SAME commit,
//     each other's release job posts a check on it under a different run id,
//     pending while the sibling builds, and the two gates deadlock. A release
//     job's own success is never a prerequisite for a release, so every
//     release-shaped check ("release" inline, "release / ..." reusable) is
//     dropped regardless of run id.
//  3. BOT runs, by name: "renovate / ..." and "update / ...". A dependency bot
//     on a schedule posts a check on whatever commit is the tip, which says
//     nothing about the commit's quality.
func Lines(pages [][]checkRun, runID string) []string {
	newest := map[string]checkRun{}
	for _, p := range pages {
		for _, c := range p {
			if strings.Contains(c.DetailsURL, "/runs/"+runID+"/") {
				continue
			}
			if c.Name == "release" || strings.HasPrefix(c.Name, "release /") {
				continue
			}
			if strings.HasPrefix(c.Name, "renovate /") || strings.HasPrefix(c.Name, "update /") {
				continue
			}
			if cur, ok := newest[c.Name]; !ok || c.ID >= cur.ID {
				newest[c.Name] = c
			}
		}
	}
	names := make([]string, 0, len(newest))
	for n := range newest {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		conclusion := "pending"
		if c := newest[n].Conclusion; c != nil {
			conclusion = *c
		}
		out = append(out, n+"\t"+conclusion)
	}
	return out
}

// Run reads the checks of the tagged commit and refuses unless the commit has
// a successful `check` and nothing else is red or pending. With Options.Wait it
// first waits, polling, for pending checks to settle: it returns as soon as
// `check` is green and nothing is pending, refuses at once when any check has
// concluded red, and refuses with the usual message once Wait has elapsed. curl,
// not gh: the ARC runner image ships no gh; the API is read directly.
func Run(ctx context.Context, o Options) error {
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if o.Poll <= 0 {
		o.Poll = 25 * time.Second
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	var waited time.Duration
	for {
		runs, err := fetch(ctx, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "checks on %s:\n", o.SHA)
		if len(runs) == 0 {
			fmt.Fprintln(o.Out, "  ")
		}
		for _, l := range runs {
			fmt.Fprintf(o.Out, "  %s\n", l)
		}

		hasCheck, red, pending := false, false, false
		var bad []string
		for _, l := range runs {
			switch {
			case l == "check\tsuccess":
				hasCheck = true
			case strings.HasSuffix(l, "\tpending"):
				pending = true
			case !strings.HasSuffix(l, "\tsuccess") && !strings.HasSuffix(l, "\tneutral") && !strings.HasSuffix(l, "\tskipped"):
				red = true
			}
			if !strings.HasSuffix(l, "\tsuccess") && !strings.HasSuffix(l, "\tneutral") && !strings.HasSuffix(l, "\tskipped") {
				bad = append(bad, l)
			}
		}
		if hasCheck && len(bad) == 0 {
			fmt.Fprintf(o.Out, "all checks green on %s\n", o.SHA)
			return nil
		}
		// Not decided yet: nothing is red and either `check` has not
		// finished (or has not appeared) or another check is pending.
		if !red && (pending || !hasCheck) && waited < o.Wait {
			d := o.Poll
			if rest := o.Wait - waited; d > rest {
				d = rest
			}
			fmt.Fprintf(o.Out, "waiting %s for the tagged commit's checks to finish\n", d)
			if err := o.Sleep(ctx, d); err != nil {
				return err
			}
			waited += d
			continue
		}
		if !hasCheck {
			if o.Wait > 0 && !red {
				fmt.Fprintf(o.Out, "::error::the tagged commit has no successful check run after %s — releases build only from check-green commits\n", o.Wait)
			} else {
				fmt.Fprintln(o.Out, "::error::the tagged commit has no successful check run — releases build only from check-green commits")
			}
			return ErrNotGreen
		}
		fmt.Fprintln(o.Out, "::error::not-green checks on the tagged commit:")
		fmt.Fprintln(o.Out, strings.Join(bad, "\n"))
		return ErrNotGreen
	}
}

// fetch reads every page of the commit's check runs and filters them.
func fetch(ctx context.Context, o Options) ([]string, error) {
	var pages [][]checkRun
	for n := 1; ; n++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?per_page=100&page=%d", o.API, o.Repo, o.SHA, n), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+o.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := o.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		var p page
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("check runs of %s: %s", o.SHA, resp.Status)
			}
			return json.NewDecoder(resp.Body).Decode(&p)
		}()
		if err != nil {
			return nil, err
		}
		pages = append(pages, p.CheckRuns)
		if n*100 >= p.TotalCount {
			break
		}
	}
	return Lines(pages, o.RunID), nil
}
