// Package fleetsteps is the port of small shell steps the fleet workflows of
// truvity/ci-workflows carry in their discover, align and renovate jobs:
// printing the job's OIDC claims, resolving the commit author, reading a
// repository's parity settings, approving the non-major renovate pull requests
// that wait for a review, and summarising the majors available across an
// estate. The GitHub API is read with net/http (the shell used curl and jq).
package fleetsteps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Common is what every step shares.
type Common struct {
	Out     io.Writer
	Output  string // GITHUB_OUTPUT
	Summary string // GITHUB_STEP_SUMMARY
	HTTP    *http.Client
}

func (c *Common) init() {
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
}

func appendTo(path, text string) error {
	if path == "" {
		path = os.DevNull
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// jsonText is jq's compact output of a value: no HTML escaping, UTF-8 as is.
func jsonText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// plain is jq -r of a value: a string as is, anything else as JSON.
func plain(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return jsonText(v)
}

type client struct {
	http  *http.Client
	token string
	// versionHeader sends X-GitHub-Api-Version, as the shell's api() did.
	versionHeader bool
	// noAccept sends no Accept header, as the shell's bare curl did.
	noAccept bool
}

// do is the curl -fsS call: an HTTP error status is an error.
func (c client) do(ctx context.Context, method, u string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = strings.NewReader(jsonText(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if !c.noAccept {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	if c.versionHeader {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %s", method, u, resp.Status)
	}
	return b, nil
}

// obj keeps jq's key order in a request body.
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
		b.WriteString(jsonText(e.v))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// ── oidc-claims ──────────────────────────────────────────────────────────

// ClaimsOptions are the "Print this job's OIDC claims" step's environment.
type ClaimsOptions struct {
	Common
	RequestURL   string // ACTIONS_ID_TOKEN_REQUEST_URL
	RequestToken string // ACTIONS_ID_TOKEN_REQUEST_TOKEN
}

// OIDCClaims prints what an issuer's `github` matchers compare, as GitHub put
// it in this job's identity token. The token is minted for a throwaway
// audience, so it is good for nothing if it ever leaked, and only its claims
// are printed.
func OIDCClaims(ctx context.Context, o ClaimsOptions) error {
	o.init()
	if o.RequestURL == "" {
		fmt.Fprintln(o.Out, "::warning::no id-token permission in this job; nothing to print")
		return nil
	}
	// No timeout of its own: the shell's curl had none here.
	c := client{http: &http.Client{Timeout: o.HTTP.Timeout}, token: o.RequestToken, noAccept: true}
	raw, err := c.do(ctx, "GET", o.RequestURL+"&audience=debug-oidc-claims", nil)
	if err != nil {
		return err
	}
	var tok struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("the token response: %w", err)
	}
	jwt := plain(tok.Value)
	fmt.Fprintf(o.Out, "::add-mask::%s\n", jwt)
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return errors.New("the identity token has no payload")
	}
	payload := strings.NewReplacer("_", "/", "-", "+").Replace(parts[1])
	for len(payload)%4 != 0 {
		payload += "="
	}
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return fmt.Errorf("the identity token payload: %w", err)
	}
	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return fmt.Errorf("the identity token payload: %w", err)
	}
	var picked obj
	for _, k := range []string{"repository", "ref", "ref_type", "event_name", "workflow_ref", "job_workflow_ref", "sha"} {
		picked = append(picked, kv{k, claims[k]}) // a missing claim is null
	}
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(jsonText(picked)), "", "  "); err != nil {
		return err
	}
	fmt.Fprintln(o.Out, out.String())
	return nil
}

// ── commit-author ────────────────────────────────────────────────────────

// AuthorOptions are the "Resolve the commit author" step's environment.
type AuthorOptions struct {
	Common
	Token     string
	UserLogin string // USER_LOGIN
	Email     string
	API       string
}

// escapeURI is jq's @uri: everything but the unreserved characters.
func escapeURI(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte("-_.~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// CommitAuthor resolves the commit author: the App's bot noreply address,
// looked up when the caller does not give it.
func CommitAuthor(ctx context.Context, o AuthorOptions) error {
	o.init()
	email := o.Email
	if email == "" {
		raw, err := client{http: o.HTTP, token: o.Token}.do(ctx, "GET", o.API+"/users/"+escapeURI(o.UserLogin), nil)
		if err != nil {
			return err
		}
		var u struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(raw, &u); err != nil {
			return fmt.Errorf("the user %s: %w", o.UserLogin, err)
		}
		id := "null"
		if len(u.ID) > 0 {
			id = plain(u.ID)
		}
		email = id + "+" + o.UserLogin + "@users.noreply.github.com"
	}
	if err := appendTo(o.Output, "git-email="+email+"\n"); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "commits as %s <%s>\n", o.UserLogin, email)
	return nil
}

// ── parity-settings ──────────────────────────────────────────────────────

// SettingsOptions are the "Read the default branch and parity settings" step's
// environment.
type SettingsOptions struct {
	Common
	Token   string
	Repo    string
	API     string
	RunMode string // RUN_MODE
}

// ParitySettings reads the repository's default branch and its optional
// `.devbox-parity.json`. Absent, unreadable or not an object, the file is `{}`
// and every setting keeps its run-wide default.
func ParitySettings(ctx context.Context, o SettingsOptions) error {
	o.init()
	c := client{http: o.HTTP, token: o.Token, versionHeader: true}
	raw, err := c.do(ctx, "GET", o.API+"/repos/"+o.Repo, nil)
	if err != nil {
		return err
	}
	var repo struct {
		DefaultBranch json.RawMessage `json:"default_branch"`
	}
	if err := json.Unmarshal(raw, &repo); err != nil {
		return fmt.Errorf("the repository %s: %w", o.Repo, err)
	}
	base := "null"
	if len(repo.DefaultBranch) > 0 {
		base = plain(repo.DefaultBranch)
	}

	settings := map[string]json.RawMessage{}
	if raw, err := c.do(ctx, "GET", o.API+"/repos/"+o.Repo+"/contents/.devbox-parity.json?ref="+base, nil); err == nil {
		var f struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &f) == nil && f.Content != "" {
			clean := strings.Map(func(r rune) rune {
				if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
					return -1
				}
				return r
			}, f.Content)
			if b, err := base64.StdEncoding.DecodeString(clean); err == nil {
				var m map[string]json.RawMessage
				if json.Unmarshal(b, &m) == nil && m != nil {
					settings = m
				}
			}
		}
	}

	dirs := "[]"
	if v, ok := settings["module-dirs"]; ok && string(v) != "null" && string(v) != "false" {
		var strs []string
		if json.Unmarshal(v, &strs) == nil {
			dirs = jsonText(strs)
		} else {
			var buf bytes.Buffer
			if json.Compact(&buf, v) == nil {
				dirs = buf.String()
			}
		}
	}
	mode := ""
	if v, ok := settings["mode"]; ok && string(v) != "null" && string(v) != "false" {
		mode = plain(v)
	}
	switch mode {
	case "auto", "align":
		fmt.Fprintf(o.Out, "%s: mode %s from .devbox-parity.json\n", o.Repo, mode)
	case "":
		mode = o.RunMode
	default:
		fmt.Fprintf(o.Out, "::warning::%s: .devbox-parity.json mode '%s' is not auto or align — using this run's mode %s\n", o.Repo, mode, o.RunMode)
		mode = o.RunMode
	}
	if err := appendTo(o.Output, fmt.Sprintf("base=%s\nmodule-dirs=%s\nmode=%s\n", base, dirs, mode)); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "%s: default branch %s, module-dirs %s, mode %s\n", o.Repo, base, dirs, mode)
	return nil
}

// ── approve-renovate ─────────────────────────────────────────────────────

// ApproveOptions are the "Approve non-major renovate PRs that need a review"
// step's environment.
type ApproveOptions struct {
	Common
	Token string
	Repo  string
	API   string
}

var renovateLogin = regexp.MustCompile(`renovate`)

// ApproveRenovate approves each open non-major renovate pull request whose
// review decision is REVIEW_REQUIRED and that no bot has approved yet, so
// auto-merge can proceed; GitHub still holds the merge until every required
// check passes. A major is left for a human.
func ApproveRenovate(ctx context.Context, o ApproveOptions) error {
	o.init()
	c := client{http: o.HTTP, token: o.Token, versionHeader: true}
	owner, name, _ := strings.Cut(o.Repo, "/")
	if i := strings.LastIndex(o.Repo, "/"); i >= 0 { // ${REPO%/*} and ${REPO#*/}
		owner = o.Repo[:i]
	}
	approved := 0
	raw, err := c.do(ctx, "GET", o.API+"/repos/"+o.Repo+"/pulls?state=open&per_page=100", nil)
	if err != nil {
		return err
	}
	var prs []struct {
		Number int `json:"number"`
		User   struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw, &prs); err != nil {
		return fmt.Errorf("open pull requests: %w", err)
	}
	for _, p := range prs {
		if p.User.Type != "Bot" || !renovateLogin.MatchString(p.User.Login) {
			continue
		}
		major := false
		for _, l := range p.Labels {
			major = major || l.Name == "major"
		}
		if major {
			fmt.Fprintf(o.Out, "#%d: major — left for a human\n", p.Number)
			continue
		}
		raw, err := c.do(ctx, "POST", o.API+"/graphql", obj{
			{"query", "query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewDecision}}}"},
			{"variables", obj{{"o", owner}, {"r", name}, {"n", p.Number}}},
		})
		if err != nil {
			return err
		}
		var res struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewDecision *string `json:"reviewDecision"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &res)
		decision := "NONE"
		if d := res.Data.Repository.PullRequest.ReviewDecision; d != nil {
			decision = *d
		}
		if decision != "REVIEW_REQUIRED" {
			fmt.Fprintf(o.Out, "#%d: review %s — nothing to add\n", p.Number, decision)
			continue
		}
		raw, err = c.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", o.API, o.Repo, p.Number), nil)
		if err != nil {
			return err
		}
		var reviews []struct {
			State string `json:"state"`
			User  struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(raw, &reviews); err != nil {
			return fmt.Errorf("reviews of #%d: %w", p.Number, err)
		}
		byBot := false
		for _, r := range reviews {
			byBot = byBot || (r.State == "APPROVED" && strings.HasSuffix(r.User.Login, "[bot]"))
		}
		if byBot {
			fmt.Fprintf(o.Out, "#%d: already approved by a bot\n", p.Number)
			continue
		}
		if _, err := c.do(ctx, "POST", fmt.Sprintf("%s/repos/%s/pulls/%d/reviews", o.API, o.Repo, p.Number),
			obj{{"event", "APPROVE"}, {"body", "Non-major dependency update from renovate. Approved by the fleet job so auto-merge can proceed; GitHub still holds the merge until every required check passes."}}); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "#%d: approved\n", p.Number)
		approved++
	}
	return appendTo(o.Summary, fmt.Sprintf("approved %d renovate PR(s)\n", approved))
}

// ── available-majors ─────────────────────────────────────────────────────

// MajorsOptions are the "Available majors across the estate" step's environment.
type MajorsOptions struct {
	Common
	Dir string // where reports/ is; "" is the working directory
}

type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

// parseObject reads a JSON object keeping its key order, as jq iterates it.
func parseObject(raw json.RawMessage) (orderedObject, bool) {
	o := orderedObject{vals: map[string]json.RawMessage{}}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return o, false
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return o, false
		}
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return o, false
		}
		key := k.(string)
		if _, dup := o.vals[key]; !dup {
			o.keys = append(o.keys, key)
		}
		o.vals[key] = v
	}
	return o, true
}

type major struct {
	pkg, manager, repo, current, newest string
	hasPkg                              bool
}

func firstOf(raws ...json.RawMessage) (json.RawMessage, bool) {
	for _, r := range raws {
		if len(r) > 0 && string(r) != "null" && string(r) != "false" {
			return r, true
		}
	}
	return nil, false
}

// AvailableMajors reads the reports renovate wrote (reports/**/*.json) and
// prints one markdown table of the majors available, one row per package, to
// the log and the step summary.
func AvailableMajors(ctx context.Context, o MajorsOptions) error {
	o.init()
	tee := func(s string) error {
		fmt.Fprint(o.Out, s)
		return appendTo(o.Summary, s)
	}
	root := filepath.Join(o.Dir, "reports")
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".json") {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	if len(files) == 0 {
		return tee("no reports — every repository job failed before renovate wrote one\n")
	}

	var ms []major
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var rep struct {
			Repositories json.RawMessage `json:"repositories"`
		}
		if err := json.Unmarshal(b, &rep); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		repos, ok := parseObject(rep.Repositories)
		if !ok {
			return fmt.Errorf("%s: .repositories is not an object", f)
		}
		for _, repo := range repos.keys {
			var rv struct {
				PackageFiles json.RawMessage `json:"packageFiles"`
			}
			if err := json.Unmarshal(repos.vals[repo], &rv); err != nil {
				return fmt.Errorf("%s: %s: %w", f, repo, err)
			}
			managers, ok := parseObject(rv.PackageFiles)
			if !ok {
				return fmt.Errorf("%s: %s: .packageFiles is not an object", f, repo)
			}
			for _, manager := range managers.keys {
				var pfs []struct {
					Deps []struct {
						DepName        json.RawMessage `json:"depName"`
						PackageName    json.RawMessage `json:"packageName"`
						CurrentVersion json.RawMessage `json:"currentVersion"`
						CurrentValue   json.RawMessage `json:"currentValue"`
						Updates        []struct {
							UpdateType string          `json:"updateType"`
							NewVersion json.RawMessage `json:"newVersion"`
							NewValue   json.RawMessage `json:"newValue"`
						} `json:"updates"`
					} `json:"deps"`
				}
				if err := json.Unmarshal(managers.vals[manager], &pfs); err != nil {
					return fmt.Errorf("%s: %s: %w", f, manager, err)
				}
				for _, pf := range pfs {
					for _, dep := range pf.Deps {
						for _, u := range dep.Updates {
							if u.UpdateType != "major" {
								continue
							}
							m := major{manager: manager, repo: repo, current: "?", newest: "?"}
							if v, ok := firstOf(dep.DepName, dep.PackageName); ok {
								m.pkg, m.hasPkg = plain(v), true
							}
							if v, ok := firstOf(dep.CurrentVersion, dep.CurrentValue); ok {
								m.current = plain(v)
							}
							if v, ok := firstOf(u.NewVersion, u.NewValue); ok {
								m.newest = plain(v)
							}
							ms = append(ms, m)
						}
					}
				}
			}
		}
	}

	type row struct {
		pkg, manager, newest string
		repos                []string
		null                 bool // no package name: jq prints null and sorts it first
	}
	groups := map[string][]major{}
	var names []string
	for _, m := range ms {
		k := m.pkg
		if !m.hasPkg {
			k = "\x00null" // jq sorts null before every string
		}
		if _, ok := groups[k]; !ok {
			names = append(names, k)
		}
		groups[k] = append(groups[k], m)
	}
	sort.Strings(names)
	var rows []row
	for _, k := range names {
		g := groups[k]
		r := row{pkg: g[0].pkg, manager: g[0].manager, null: !g[0].hasPkg}
		if r.null {
			r.pkg = "null"
		}
		best := g[0].newest
		for _, m := range g[1:] {
			if cmpMajor(m.newest, best) >= 0 { // the last of equals, as max_by
				best = m.newest
			}
		}
		r.newest = best
		seen := map[string]bool{}
		for _, m := range g {
			rn := m.repo
			if i := strings.Index(rn, "/"); i >= 0 {
				rest := rn[i+1:]
				if j := strings.Index(rest, "/"); j >= 0 {
					rest = rest[:j]
				}
				rn = rest
			} else {
				rn = "" // split("/")[1] of a name with no slash is null
			}
			e := rn + " " + m.current
			if !seen[e] {
				seen[e] = true
				r.repos = append(r.repos, e)
			}
		}
		sort.Strings(r.repos)
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if len(rows[i].repos) != len(rows[j].repos) {
			return len(rows[i].repos) > len(rows[j].repos)
		}
		if rows[i].null != rows[j].null {
			return rows[i].null
		}
		return rows[i].pkg < rows[j].pkg
	})

	var b strings.Builder
	b.WriteString("## Available majors\n\n")
	if len(rows) == 0 {
		b.WriteString("None: every enrolled repository is on the newest major of everything it uses.\n")
	} else {
		b.WriteString("Decide these once, in the estate policy. Each row is one package; the right column is where it is used and at which version.\n\n")
		b.WriteString("| package | manager | newest major | repositories (current) |\n")
		b.WriteString("|---|---|---|---|\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.pkg, r.manager, r.newest, strings.Join(r.repos, ", "))
		}
	}
	return tee(b.String())
}

var digits = regexp.MustCompile(`[0-9]+`)

// cmpMajor orders versions as jq's `ltrimstr("v") | [scan("[0-9]+") | tonumber]`
// does: by their numbers, element by element, the shorter first on a tie.
func cmpMajor(a, b string) int {
	na := digits.FindAllString(strings.TrimPrefix(a, "v"), -1)
	nb := digits.FindAllString(strings.TrimPrefix(b, "v"), -1)
	for i := 0; i < len(na) && i < len(nb); i++ {
		x, _ := new(big.Int).SetString(na[i], 10)
		y, _ := new(big.Int).SetString(nb[i], 10)
		if c := x.Cmp(y); c != 0 {
			return c
		}
	}
	return len(na) - len(nb)
}
