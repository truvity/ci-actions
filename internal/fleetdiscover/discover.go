// Package fleetdiscover is the port of fleet-discover/discover.sh: which
// repositories a fleet job works on.
//
// The App's installation is the source: every repository it can see,
// narrowed by rules that each have a reason.
//
//	estate        public or private (or all). The public estate's job runs on
//	              GitHub-hosted runners and the private estate's on a
//	              self-hosted pool; a job must never touch the other estate's
//	              repositories, so the installation scope alone is not the
//	              boundary: this is.
//	archived      skipped, always. Nothing should open a PR there.
//	require-check the default branch must have at least one REQUIRED status
//	              check, from EITHER a repository ruleset or classic branch
//	              protection. A source that cannot be read is unknown, never
//	              no: the repository is kept and the summary says the rule was
//	              not decided.
//	require-file  a file that marks the repository as opted in.
//
// The required-check rule reads two sources (see gate), and the log,
// summary and output lines are the shell script's, byte for byte.
package fleetdiscover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/truvity/ci-actions/internal/ghapi"
)

// ErrReported marks a failure whose ::error:: line was already written to
// Out; the caller adds nothing.
var ErrReported = errors.New("fleet discover: reported")

// Options are the action's inputs and the runner's files.
type Options struct {
	Token        string
	Estate       string // public, private or all
	RequireCheck string // the string "true" turns the rule on
	RequireFile  string
	Filter       string
	Enrolled     string // JSON array of repository NAMES, or empty
	API          string // REST base; empty means the public API

	// Out is stdout. Summary is the step summary file ("" for none) and
	// Output the GITHUB_OUTPUT file ("" for none).
	Out     io.Writer
	Summary string
	Output  string

	Client *ghapi.Client // nil builds one from API and Token
}

type repo struct {
	FullName      string `json:"full_name"`
	Visibility    string `json:"visibility"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
}

type skip struct{ repo, reason string }
type undecidedRepo struct{ repo, detail string }

// Run discovers the repositories and writes the log, the step summary and
// the outputs.
func Run(ctx context.Context, o Options) error {
	switch o.Estate {
	case "public", "private", "all":
	default:
		fmt.Fprintf(o.Out, "::error::estate must be public, private or all (got '%s')\n", o.Estate)
		return ErrReported
	}
	c := o.Client
	if c == nil {
		c = ghapi.New(o.API, o.Token)
	}
	d := &discoverer{o: o, c: c}

	all, err := d.listRepos(ctx)
	if err != nil {
		return err
	}

	enrolled, err := parseEnrolled(o.Enrolled)
	if err != nil {
		fmt.Fprintf(o.Out, "::error::repositories is not a non-empty JSON array of names: %s\n", o.Enrolled)
		return ErrReported
	}
	var filter *regexp.Regexp
	if o.Filter != "" {
		filter, err = regexp.Compile(o.Filter)
		if err != nil {
			// The shell version's jq could not compile it either and so
			// skipped every repository without a word; say so once.
			fmt.Fprintf(o.Out, "::warning::filter is not a valid RE2 expression (%v): every repository will be skipped\n", err)
		}
	}

	var kept []string
	var skipped []skip
	var undecided []undecidedRepo
	for _, r := range all {
		reason := ""
		name := r.FullName[strings.Index(r.FullName, "/")+1:]
		switch {
		case o.Enrolled != "" && !contains(enrolled, name):
			reason = "not enrolled"
		case o.Filter != "" && (filter == nil || !filter.MatchString(r.FullName)):
			reason = "does not match filter"
		case r.Archived:
			reason = "archived"
		case o.Estate != "all" && r.Visibility != o.Estate:
			reason = "visibility " + r.Visibility + ", estate " + o.Estate
		default:
			if o.RequireCheck == "true" {
				g, detail := d.gate(ctx, r.FullName, r.DefaultBranch)
				switch g {
				case gateNo:
					reason = "no required status check on " + r.DefaultBranch + " (" + detail + ")"
				case gateUnknown:
					// Never a skip. A repository dropped because a read
					// 403'd would go unprocessed with nothing but a table
					// row to say so, which is the failure this rule exists
					// to prevent, one level up.
					fmt.Fprintf(o.Out, "::warning::%s: could not decide the required-check rule (%s) — processing it anyway\n", r.FullName, detail)
					undecided = append(undecided, undecidedRepo{r.FullName, detail})
				}
			}
			if reason == "" && o.RequireFile != "" {
				if !d.fileExists(ctx, r.FullName, o.RequireFile, r.DefaultBranch) {
					reason = "no " + o.RequireFile
				}
			}
		}
		if reason == "" {
			kept = append(kept, r.FullName)
		} else {
			skipped = append(skipped, skip{r.FullName, reason})
		}
	}
	sort.Strings(kept)
	if kept == nil {
		kept = []string{}
	}

	// Enrolled but invisible to the App: not installed on it, renamed or
	// deleted. Error annotations, one per repository, so the run page names
	// every fault; the rest of the estate still runs.
	seen := map[string]bool{}
	for _, r := range all {
		seen[r.FullName[strings.Index(r.FullName, "/")+1:]] = true
	}
	missing := []string{}
	for _, n := range enrolled {
		if !seen[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	for _, n := range missing {
		fmt.Fprintf(o.Out, "::error::%s is enrolled but the App cannot see it — install the App on it, or remove it from the list\n", n)
	}

	var sum bytes.Buffer
	fmt.Fprintf(&sum, "## fleet-discover — estate `%s`\n\n", o.Estate)
	fmt.Fprintf(&sum, "**%d** repositories to process out of %d the App can see.\n\n", len(kept), len(all))
	if len(kept) > 0 {
		for _, k := range kept {
			fmt.Fprintf(&sum, "- %s\n", k)
		}
		sum.WriteString("\n")
	}
	if len(missing) > 0 {
		fmt.Fprintf(&sum, "**Enrolled but not reachable by the App:** %s\n\n", strings.Join(missing, ", "))
	}
	if len(undecided) > 0 {
		sum.WriteString("**Required-check rule not decided** (kept regardless — a read failed, which is not the same as no check):\n\n")
		for _, u := range undecided {
			fmt.Fprintf(&sum, "- %s — %s\n", u.repo, u.detail)
		}
		sum.WriteString("\n")
	}
	// "not enrolled" is the normal case for most of an installation once a
	// list is in use; listing each one would bury the rest.
	var shown []skip
	for _, s := range skipped {
		if s.reason != "not enrolled" {
			shown = append(shown, s)
		}
	}
	if len(shown) > 0 {
		sum.WriteString("| skipped | why |\n|---|---|\n")
		for _, s := range shown {
			fmt.Fprintf(&sum, "| %s | %s |\n", s.repo, s.reason)
		}
	}
	if _, err := o.Out.Write(sum.Bytes()); err != nil {
		return err
	}
	if o.Summary != "" {
		if err := appendFile(o.Summary, sum.Bytes()); err != nil {
			return err
		}
	}
	if o.Output != "" {
		out := fmt.Sprintf("repositories=%s\ncount=%d\nmissing=%s\n", jsonLine(kept), len(kept), jsonLine(missing))
		if err := appendFile(o.Output, []byte(out)); err != nil {
			return err
		}
	}
	return nil
}

// jsonLine is `jq -c`: compact, and no HTML escaping.
func jsonLine(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// parseEnrolled reads the `repositories` input: empty means none enrolled
// (every repository), anything else must be a non-empty JSON array.
func parseEnrolled(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil || len(raw) == 0 {
		return nil, errors.New("repositories is not a non-empty JSON array of names")
	}
	var names []string
	for _, r := range raw {
		var n string
		if json.Unmarshal(r, &n) == nil {
			names = append(names, n)
		}
	}
	return names, nil
}

type discoverer struct {
	o Options
	c *ghapi.Client
}

// listRepos reads every repository the installation reaches, paginated,
// reduced to the four fields discovery reads.
func (d *discoverer) listRepos(ctx context.Context) ([]repo, error) {
	var all []repo
	for page := 1; ; page++ {
		status, body, err := d.c.Raw(ctx, "GET", fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", d.c.BaseURL, page), nil)
		if err != nil {
			return nil, fmt.Errorf("listing the installation's repositories: %w", err)
		}
		if status >= 400 {
			return nil, fmt.Errorf("listing the installation's repositories: HTTP %d", status)
		}
		var p struct {
			Repositories *[]repo `json:"repositories"`
		}
		if err := json.Unmarshal(body, &p); err != nil || p.Repositories == nil {
			return nil, errors.New("listing the installation's repositories: unexpected answer")
		}
		all = append(all, *p.Repositories...)
		if len(*p.Repositories) < 100 {
			return all, nil
		}
	}
}

func (d *discoverer) fileExists(ctx context.Context, full, file, branch string) bool {
	segs := strings.Split(file, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	u := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", d.c.BaseURL, full, strings.Join(segs, "/"), url.QueryEscape(branch))
	status, _, err := d.c.Raw(ctx, "GET", u, nil)
	return err == nil && status > 0 && status < 400
}

const unknown = -1 // a read that failed: not zero

// THE REQUIRED-CHECK RULE READS TWO SOURCES.
//
// A branch can require a status check in two entirely separate ways, and a
// repository is gated when EITHER of them requires at least one:
//
//	rulesets   GET /repos/{owner}/{repo}/rules/branches/{branch}: the
//	           EFFECTIVE rules for that branch, already merged across every
//	           ruleset that applies to it, at repository and organisation
//	           level. Needs Metadata: read, which every installation token
//	           carries.
//	classic    branch protection. GET .../branches/{branch}/protection
//	           first: it is authoritative and viewer-independent, and its 404
//	           "Branch not protected" is a definitive no. It needs
//	           Administration: read, which the fleet App does not carry, so
//	           when it is not readable the reader falls back to GraphQL's
//	           refUpdateRule, which needs no permission beyond seeing the
//	           repository but reports the rule AS IT APPLIES TO THE VIEWER.
//
// Reading classic protection alone skipped every repository that had moved
// its merge gate into a ruleset, silently, since a skip is the normal
// outcome for most of an installation. Each reader returns a COUNT or
// unknown. Unknown is not zero: a repository must never be dropped because
// a read failed.
func (d *discoverer) rulesetChecks(ctx context.Context, full, branch string) int {
	status, body, err := d.c.Raw(ctx, "GET", fmt.Sprintf("%s/repos/%s/rules/branches/%s?per_page=100", d.c.BaseURL, full, escapePath(branch)), nil)
	if err != nil || status != 200 {
		return unknown
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []json.RawMessage `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &rules); err != nil {
		return unknown
	}
	n := 0
	for _, r := range rules {
		if r.Type == "required_status_checks" {
			n += len(r.Parameters.Checks)
		}
	}
	return n
}

func (d *discoverer) classicChecks(ctx context.Context, full, branch string) int {
	status, body, err := d.c.Raw(ctx, "GET", fmt.Sprintf("%s/repos/%s/branches/%s/protection", d.c.BaseURL, full, escapePath(branch)), nil)
	if err == nil {
		switch status {
		case 200:
			var p struct {
				RSC *struct {
					Contexts []json.RawMessage `json:"contexts"`
				} `json:"required_status_checks"`
			}
			if json.Unmarshal(body, &p) != nil {
				return unknown
			}
			if p.RSC == nil {
				return 0
			}
			return len(p.RSC.Contexts)
		case 404:
			// "Branch not protected" is the endpoint saying there is no
			// classic protection. A bare "Not Found" is this token being
			// told nothing, which is not the same answer: fall through.
			var m struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(body, &m) == nil && m.Message == "Branch not protected" {
				return 0
			}
		}
	}

	owner, name := full[:strings.Index(full, "/")], full[strings.Index(full, "/")+1:]
	q, _ := json.Marshal(map[string]any{
		"query":     "query($o:String!,$r:String!){repository(owner:$o,name:$r){defaultBranchRef{refUpdateRule{requiredStatusCheckContexts}}}}",
		"variables": map[string]string{"o": owner, "r": name},
	})
	status, body, err = d.c.Raw(ctx, "POST", d.c.BaseURL+"/graphql", q)
	if err != nil || status != 200 {
		return unknown
	}
	// GraphQL answers 200 with an `errors` array, so the status is not the
	// whole story; a null repository is a read this token did not get.
	var g struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Repository *struct {
				DefaultBranchRef struct {
					RefUpdateRule *struct {
						Contexts []json.RawMessage `json:"requiredStatusCheckContexts"`
					} `json:"refUpdateRule"`
				} `json:"defaultBranchRef"`
			} `json:"repository"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &g) != nil || len(g.Errors) > 0 || g.Data.Repository == nil {
		return unknown
	}
	if g.Data.Repository.DefaultBranchRef.RefUpdateRule == nil {
		return 0
	}
	return len(g.Data.Repository.DefaultBranchRef.RefUpdateRule.Contexts)
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return strings.Join(segs, "/")
}

func describe(n int) string {
	switch n {
	case unknown:
		return "could not read"
	case 0:
		return "none"
	}
	return fmt.Sprint(n)
}

type gateResult int

const (
	gateYes gateResult = iota
	gateNo
	gateUnknown
)

// gate answers yes, no or unknown, and names BOTH sources so a skip line
// says what was actually consulted.
func (d *discoverer) gate(ctx context.Context, full, branch string) (gateResult, string) {
	rulesets := d.rulesetChecks(ctx, full, branch)
	classic := d.classicChecks(ctx, full, branch)
	detail := "rulesets: " + describe(rulesets) + ", classic protection: " + describe(classic)
	switch {
	case rulesets >= 1 || classic >= 1:
		return gateYes, detail
	case rulesets == unknown || classic == unknown:
		return gateUnknown, detail
	}
	return gateNo, detail
}
