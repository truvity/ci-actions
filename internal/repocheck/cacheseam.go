// Package repocheck holds the checks this repository runs on ITSELF in its own
// gate: facts about its files and its pins that no caller's run would ever
// notice going wrong. They are commands of the ci-actions binary so the gate
// needs nothing but Go.
package repocheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// CacheSeamOptions configures the check.
type CacheSeamOptions struct {
	Root    string // the repository root
	BaseURL string // where ci-cache's raw files are served; empty is raw.githubusercontent.com/truvity/ci-cache
	HTTP    *http.Client
	Out     io.Writer
}

// ErrFailed means at least one case failed; the lines are already written.
var ErrFailed = fmt.Errorf("repocheck: failed")

// What `setup-devbox` does about caches, now that it does not do the caching.
//
// The wiring moved to truvity/ci-cache's own `setup` action, which is tested
// there. What stays here is the SEAM: that this composite delegates, that it
// hands over every input the cache needs, that the retired input is not
// silently ignored, and that nothing here wires a cache any more.
//
// The seam is worth its own check because it is where the two repositories can
// disagree without either being wrong on its own: an input added there and not
// passed here is a cache that quietly does less, which is exactly the failure
// mode this whole line of work was chasing.
func CacheSeam(ctx context.Context, o CacheSeamOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://raw.githubusercontent.com/truvity/ci-cache"
	}
	action := filepath.Join(o.Root, "setup-devbox", "action.yaml")
	b, err := os.ReadFile(action)
	if err != nil {
		fmt.Fprintf(o.Out, "no action at %s\n", action)
		return err
	}
	var doc struct {
		Runs struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	step := func(name string) map[string]any {
		for _, s := range doc.Runs.Steps {
			if s["name"] == name {
				return s
			}
		}
		return nil
	}
	str := func(m map[string]any, k string) string {
		if v, ok := m[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}

	fail, checked := 0, 0
	say := func(format string, a ...any) { fmt.Fprintf(o.Out, format+"\n", a...) }

	// --- the delegation itself ---
	//
	// Pinned by SHA, not by tag or branch. A moving ref here would let the
	// cache wiring of every repository in both organizations change without a
	// single pin moving, which is the property this repository exists to
	// prevent.
	checked++
	const wire = "Wire the fleet caches"
	uses := ""
	if s := step(wire); s != nil {
		uses = str(s, "uses")
	}
	if m := regexp.MustCompile(`^truvity/ci-cache/setup@[0-9a-f]`).FindString(uses); m != "" {
		if sha := uses[strings.LastIndex(uses, "@")+1:]; len(sha) != 40 {
			say("FAIL [pin]: ci-cache/setup is pinned to \"%s\", which is not a full 40-character SHA", sha)
			fail++
		}
	} else {
		say("FAIL [pin]: the cache step does not use a SHA-pinned truvity/ci-cache/setup; it uses \"%s\"", uses)
		fail++
	}

	// --- every input the cache needs crosses the seam ---
	//
	// DERIVED from the downstream action at the SHA this one pins, not from a
	// list kept here: a list kept here cannot notice when the action
	// downstream grows an input this one does not pass. It cost something to
	// learn: ci-cache/setup declared nine inputs and this action passed five,
	// so the moon and yarn checks could not fire in any job in the estate,
	// silently, because an unset value makes those branches no-ops.
	//
	// Fetched at the PINNED sha, so this asks about the version actually in
	// use rather than whatever ci-cache's master says today.
	checked++
	sha := ""
	if i := strings.LastIndex(uses, "@"); i >= 0 {
		sha = uses[i+1:]
	}
	if !regexp.MustCompile(`^[0-9a-f]`).MatchString(sha) {
		say("FAIL [inputs]: cannot read a sha out of \"%s\"", uses)
		fail++
		sha = ""
	}
	if sha != "" {
		url := o.BaseURL + "/" + sha + "/setup/action.yaml"
		// A guard that passes when it could not look is the failure mode this
		// whole check exists to prevent, so a fetch failure is a FAILURE and
		// not a skip.
		down, ferr := fetchYAML(ctx, o.HTTP, url)
		if ferr != nil {
			say("FAIL [inputs]: could not fetch %s — the seam went unchecked", url)
			fail++
		} else {
			// Declared THERE but deliberately not passed here, each with the
			// reason. Anything not on this list must cross, or this fails.
			//
			//	languages       setup-devbox does not second-guess detection; the
			//	                action reads the tree, which is the whole point of
			//	                it owning detection.
			//	client-version  the action pins the clients that match its own
			//	                release; a caller choosing them would defeat
			//	                versioning the bundle together.
			//	bazel-remote    NOT WIRED YET, and the reason is not this action's:
			//	npm-registry    the estate has no org variable naming either host,
			//	                so there is nothing to pass. Remove them from this
			//	                list the moment a variable exists, and the check
			//	                will then insist they cross.
			exempt := map[string]bool{"languages": true, "client-version": true, "bazel-remote": true, "npm-registry": true}
			var names []string
			for k := range down.Inputs {
				names = append(names, k)
			}
			sort.Strings(names)
			var with map[string]any
			if s := step(wire); s != nil {
				with, _ = s["with"].(map[string]any)
			}
			for _, to := range names {
				if exempt[to] {
					continue
				}
				got := ""
				if v, ok := with[to]; ok && v != nil {
					got = fmt.Sprint(v)
				}
				if !strings.Contains(got, "inputs.") {
					say("FAIL [inputs]: ci-cache/setup declares \"%s\" and this action does not pass it", to)
					say("               (wired to \"%s\"); pass it, or exempt it with a reason", got)
					fail++
				}
			}
		}
	}

	// --- the retired input is refused loudly, not ignored quietly ---
	//
	// go-cache-server pointed at a cache server that was measured out of the Go
	// path. Dropping it silently would repeat the fault that started all this:
	// a variable that named something gone, a fall-through that cost four
	// milliseconds, and days before anyone noticed. What the warning says is
	// tested in internal/setupdevbox; here, that it is wired to the input.
	checked++
	const retired = "Warn on the retired cache-server input"
	cond := ""
	if s := step(retired); s != nil {
		cond = str(s, "if")
	}
	if !strings.Contains(cond, "go-cache-server != ''") {
		say("FAIL [retired]: the warning step's condition is \"%s\"; it must fire when go-cache-server is set", cond)
		fail++
	}

	// --- nothing here wires a cache any more ---
	//
	// If a `run:` step in this composite starts writing GOCACHEPROG again, two
	// places decide the same thing and the last one in file order wins, which is
	// not a decision anybody made. The shell is gone from this composite, so the
	// places to look are the step bodies that remain and the Go that backs them.
	checked++
	for _, s := range doc.Runs.Steps {
		if strings.Contains(str(s, "run"), "GOCACHEPROG") {
			say("FAIL [ownership]: a run: step in setup-devbox writes GOCACHEPROG; that belongs to ci-cache/setup now")
			fail++
			break
		}
	}
	if ents, _ := filepath.Glob(filepath.Join(o.Root, "internal", "setupdevbox", "*.go")); len(ents) > 0 {
		for _, f := range ents {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			if data, _ := os.ReadFile(f); strings.Contains(string(data), "GOCACHEPROG") {
				say("FAIL [ownership]: %s writes GOCACHEPROG; that belongs to ci-cache/setup now", filepath.Base(f))
				fail++
			}
		}
	}

	// --- the local guards that stay ---
	//
	// devbox re-applies devbox.json's env block over the job environment, so a
	// GOPROXY (or an AWS profile) pinned there silently beats whatever the
	// cache wiring set. Those checks are about devbox, not about caches, so
	// they did not move; what they say is tested in internal/setupdevbox.
	for _, g := range []struct{ tag, name string }{
		{"guard", "Guard GOPROXY against devbox.json"},
		{"aws-guard", "Guard AWS config against devbox.json"},
	} {
		checked++
		if step(g.name) == nil {
			say("FAIL [%s]: the \"%s\" step is gone; it is about devbox, not about caches", g.tag, g.name)
			fail++
		}
	}

	if checked == 0 {
		say("NOTHING CHECKED — the check found no cases, which is a failure of the check")
		return ErrFailed
	}
	if fail != 0 {
		say("%d case(s) failed of %d", fail, checked)
		return ErrFailed
	}
	say("cache seam holds (%d cases checked)", checked)
	return nil
}

type downstream struct {
	Inputs map[string]any `yaml:"inputs"`
}

func fetchYAML(ctx context.Context, c *http.Client, url string) (*downstream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var d downstream
	if err := yaml.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}
