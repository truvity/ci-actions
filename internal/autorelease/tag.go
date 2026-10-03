package autorelease

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// TagOptions are the tagging step's environment. Empty values take the
// defaults the shell gave them.
type TagOptions struct {
	Prefix             string // PREFIX
	Bot                string // BOT
	Repo               string // REPO
	Base               string // BASE, default master
	HeadingMode        string // HEADING_MODE: auto (default), always, never
	Changelog          string // CHANGELOG, default CHANGELOG.md
	WaitMinutes        string // WAIT_MINUTES, default 20
	VersionBumpCommand string // VERSION_BUMP_COMMAND
	PollSeconds        string // POLL_SECONDS, default 30
	Summary            string // GITHUB_STEP_SUMMARY
	Dir                string // the checkout; "" is the working directory

	Out   io.Writer
	Err   io.Writer
	Exec  runcmd.Exec
	Now   func() time.Time
	Sleep func(time.Duration)
}

// waitingPattern is the title of a release PR a person is preparing for a
// minor or major: the gh query below counts open PRs matching it.
const waitingPattern = `^docs\(changelog\): heading for the v[0-9]+\.[0-9]+\.0 release`

var waitingForPerson = regexp.MustCompile(waitingPattern)

var (
	dated    = regexp.MustCompile(` — [0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	patchTag = regexp.MustCompile(`^[^.]*[0-9]+\.[0-9]+\.[0-9]+$`)
	version  = regexp.MustCompile(`^v[0-9]`)
)

type tagger struct {
	o   TagOptions
	ctx context.Context
}

// run executes a command and returns its stdout.
func (t *tagger) run(name string, args ...string) (string, error) {
	var b bytes.Buffer
	err := t.o.Exec(t.ctx, runcmd.Cmd{Dir: t.o.Dir, Name: name, Args: args, Stdout: &b, Stderr: t.o.Err})
	return b.String(), err
}

// show executes a command whose stdout is the step's log.
func (t *tagger) show(env []string, name string, args ...string) error {
	return t.o.Exec(t.ctx, runcmd.Cmd{Dir: t.o.Dir, Env: env, Name: name, Args: args, Stdout: t.o.Out, Stderr: t.o.Err})
}

func (t *tagger) summary(s string) { _ = appendTo(t.o.Summary, s) }

func (t *tagger) fail(format string, a ...any) error {
	fmt.Fprintf(t.o.Out, "::error::"+format+"\n", a...)
	return ErrReported
}

func hasHeading(text, tag string) bool {
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "##" && f[1] == tag {
			return true
		}
	}
	return false
}

// lines is what awk sees: the records of the text, a final unterminated one
// included, and no empty record after a final newline.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// awkNum is awk's string to number: the leading numeric prefix, else 0.
var leadingNum = regexp.MustCompile(`^[ \t]*[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?`)

func awkNum(s string) int {
	m := leadingNum.FindString(s)
	if m == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(m), 64)
	if err != nil {
		return 0
	}
	return int(f)
}

// nextPatch is `awk -F. '{printf "%s.%s.%d", $1, $2, $3+1}'`.
func nextPatch(latest string) string {
	f := strings.Split(latest, ".")
	for len(f) < 3 {
		f = append(f, "")
	}
	return fmt.Sprintf("%s.%s.%d", f[0], f[1], awkNum(f[2])+1)
}

// Tag cuts the next patch tag when the default branch moved past the newest
// one, behind a CHANGELOG heading pull request when the release needs one.
// A reported failure is ErrReported; a failed command wraps its own error.
func Tag(ctx context.Context, o TagOptions) error {
	if o.Exec == nil {
		o.Exec = runcmd.OS
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	t := &tagger{o: o, ctx: ctx}
	if o.Changelog == "" {
		o.Changelog = "CHANGELOG.md"
	}
	if o.HeadingMode == "" {
		o.HeadingMode = "auto"
	}
	if o.WaitMinutes == "" {
		o.WaitMinutes = "20"
	}
	if o.Base == "" {
		o.Base = "master"
	}
	t.o = o
	bump := o.VersionBumpCommand
	mode := o.HeadingMode
	cl := o.Changelog

	// A bump lands in the heading's pull request, so it cannot land with no
	// pull request at all.
	if bump != "" && mode == "never" {
		return t.fail("version-bump-command needs the heading pull request to carry the bump, so it cannot be combined with changelog-heading: never")
	}

	tags, err := t.run("git", "tag", "--list", o.Prefix+"*", "--sort=-version:refname")
	if err != nil {
		return fmt.Errorf("git tag: %w", err)
	}
	var latest string
	if l := lines(tags); len(l) > 0 {
		latest = l[0]
	}
	if latest == "" {
		t.summary(fmt.Sprintf("no %s* releases yet — a human cuts the first one", o.Prefix))
		return nil
	}
	latestSHA, err := t.run("git", "rev-parse", latest+"^{commit}")
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	headSHA, err := t.run("git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	if strings.TrimSpace(latestSHA) == strings.TrimSpace(headSHA) {
		t.summary(fmt.Sprintf("master == %s — nothing to release", latest))
		return nil
	}
	next := nextPatch(latest)
	for _, kv := range [][2]string{{"user.name", o.Bot + "[bot]"}, {"user.email", o.Bot + "[bot]@users.noreply.github.com"}} {
		if _, err := t.run("git", "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("git config: %w", err)
		}
	}

	// ── The CHANGELOG heading, so the tag never ships under `## Unreleased` ──
	// target is the commit the tag will name: HEAD unless the heading has to
	// be written, then the commit that wrote it.
	target := strings.TrimSpace(headSHA)
	edit := ""
	path := cl
	if o.Dir != "" {
		path = o.Dir + string(os.PathSeparator) + cl
	}
	raw, rerr := os.ReadFile(path)
	exists := rerr == nil
	text := string(raw)

	if mode != "never" && exists {
		// A release PR a person is preparing: do not race it.
		out, err := t.run("gh", "pr", "list", "--repo", o.Repo, "--state", "open", "--limit", "100", "--json", "title",
			"--jq", `[.[] | select(.title | test("`+strings.ReplaceAll(waitingPattern, `\`, `\\`)+`"))] | length`)
		if err != nil {
			return fmt.Errorf("gh pr list: %w", err)
		}
		if w := strings.TrimSpace(out); w != "" && w != "0" {
			t.summary(fmt.Sprintf("a changelog-heading PR for a minor or major release is open — leaving %s to it", next))
			return nil
		}

		if hasHeading(text, next) {
			t.summary(fmt.Sprintf("%s already has a heading for %s — tagging as it stands", cl, next))
		} else {
			first, entries, n := "", 0, 0
			unreleasedAnywhere := false
			for _, l := range lines(text) {
				if strings.HasPrefix(l, "## ") {
					if first == "" {
						first = l
					}
					n++
					continue
				}
				if n == 1 && strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") {
					entries++
				}
			}
			for _, l := range lines(text) {
				if strings.HasPrefix(l, "## Unreleased") {
					unreleasedAnywhere = true
				}
			}
			firstUnreleased := strings.HasPrefix(first, "## Unreleased")
			if !firstUnreleased && unreleasedAnywhere {
				fmt.Fprintf(o.Out, "::warning::%s has an Unreleased heading that is not the first — leaving it alone\n", cl)
				mode = "never"
			} else if firstUnreleased && entries > 0 {
				edit = "rename"
			}
		}
	}

	if mode != "never" && exists && edit == "" && !hasHeading(text, next) {
		// Nothing under Unreleased: a dependency-only patch. C5 exempts it
		// when the repo has not been writing patch headings; write one only
		// where it has (the newest patch tag has its heading) or the caller
		// asked.
		all, err := t.run("git", "tag", "--list", o.Prefix+"*", "--sort=-version:refname")
		if err != nil {
			return fmt.Errorf("git tag: %w", err)
		}
		patch := ""
		for _, tg := range lines(all) {
			if patchTag.MatchString(tg) && awkNum(strings.Split(tg, ".")[2]) > 0 {
				patch = tg
				break
			}
		}
		if mode == "always" || bump != "" || (patch != "" && hasHeading(text, patch)) {
			edit = "dependency"
		} else {
			t.summary(fmt.Sprintf("no entries under Unreleased and no patch heading convention — %s needs none (C5's automatic-patch case)", next))
		}
	}

	if edit != "" {
		// The newest version heading says whether this repo dates its headings.
		newest := ""
		for _, l := range lines(text) {
			f := strings.Fields(l)
			if len(f) >= 2 && f[0] == "##" && version.MatchString(f[1]) {
				newest = l
				break
			}
		}
		title := "## " + next
		if dated.MatchString(newest) {
			title += " — " + o.Now().UTC().Format("2006-01-02")
		}
		var out strings.Builder
		done := false
		for _, l := range lines(text) {
			switch {
			case edit == "rename" && !done && strings.HasPrefix(l, "## Unreleased"):
				out.WriteString(title + "\n")
				done = true
				continue
			case edit == "dependency" && !done && strings.HasPrefix(l, "## ") && !strings.HasPrefix(l, "## Unreleased"):
				out.WriteString(title + "\n\n- Dependency updates.\n\n")
				done = true
			}
			out.WriteString(l + "\n")
		}
		if edit == "dependency" && !done {
			t.summary(fmt.Sprintf("%s has no version heading to place %s above — tagging as it stands", cl, next))
			edit = ""
		}
		if edit != "" {
			if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
				return err
			}
		}
	}

	// With a bump configured, a tag no pull request prepared would name a
	// commit whose files still declare the old version. A heading already
	// there means a person prepared the release (bump included).
	if bump != "" && edit == "" {
		if exists && hasHeading(text, next) {
			t.summary(fmt.Sprintf("version-bump-command skipped: %s already has the %s heading, so a person prepared this release and the bump is theirs", cl, next))
		} else {
			return t.fail("version-bump-command is set but no heading pull request can carry the bump (%s missing, misplaced, or without a version heading to place %s above) — no tag cut", cl, next)
		}
	}

	if edit != "" {
		target, err = t.headingPR(next, latest, edit, bump, target)
		if err != nil {
			return err
		}
	}

	msg := fmt.Sprintf("%s — automatic release: master moved past %s (renovate automerge)", next, latest)
	if _, err := t.run("git", "tag", "-a", next, target, "-m", msg); err != nil {
		return fmt.Errorf("git tag: %w", err)
	}
	if err := t.show(nil, "git", "push", "origin", next); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	t.summary("released " + next)
	return nil
}

// headingPR opens (or resumes) the pull request that writes the heading,
// waits for its merge and returns the merge commit the tag will name.
func (t *tagger) headingPR(next, latest, edit, bump, target string) (string, error) {
	o := t.o
	cl := o.Changelog
	branch := "auto-release/changelog-" + next
	out, err := t.run("gh", "pr", "list", "--repo", o.Repo, "--state", "open", "--head", branch, "--json", "number", "--jq", ".[0].number // empty")
	if err != nil {
		return "", fmt.Errorf("gh pr list: %w", err)
	}
	n := strings.TrimSpace(out)
	if n != "" {
		if _, err := t.run("git", "checkout", "-q", "--", cl); err != nil {
			return "", fmt.Errorf("git checkout: %w", err)
		}
		t.summary(fmt.Sprintf("resuming #%s, the heading PR an earlier run opened for %s", n, next))
	} else {
		for _, a := range [][]string{{"checkout", "-q", "-b", branch}, {"add", "--", cl}} {
			if _, err := t.run("git", a...); err != nil {
				return "", fmt.Errorf("git %s: %w", a[0], err)
			}
		}
		if bump != "" {
			// The caller's bump, in the caller's devbox, after the heading is
			// staged: whatever it changes beyond that is its own work.
			env := []string{"VERSION=" + strings.TrimPrefix(next, o.Prefix), "TAG=" + next}
			if err := t.show(env, "devbox", "run", "--", "bash", "-e", "-o", "pipefail", "-c", bump); err != nil {
				return "", fmt.Errorf("version-bump-command: %w", err)
			}
			d, err := t.run("git", "diff", "--name-only")
			if err != nil {
				return "", fmt.Errorf("git diff: %w", err)
			}
			u, err := t.run("git", "ls-files", "--others", "--exclude-standard")
			if err != nil {
				return "", fmt.Errorf("git ls-files: %w", err)
			}
			if strings.TrimSpace(d+u) == "" {
				return "", t.fail("version-bump-command changed nothing — a bump that bumps nothing is a misconfiguration; no PR opened, no tag cut")
			}
			if _, err := t.run("git", "add", "-A"); err != nil {
				return "", fmt.Errorf("git add: %w", err)
			}
		}
		body := "Written by the shared auto-release workflow so that the tag names the commit that carries its own heading."
		prBody := fmt.Sprintf("The shared auto-release workflow is about to cut %s. This gives the CHANGELOG its heading first, and the tag follows the merge, so a release never ships under `## Unreleased`.", next)
		if bump != "" {
			body += " It also carries the version bump of the version-bump-command input."
			prBody += " The version-bump-command input ran in the same commit, so the tagged commit declares the version it is tagged as."
		}
		title := fmt.Sprintf("docs(changelog): heading for the %s release", next)
		if _, err := t.run("git", "commit", "-q", "-m", title, "-m", body); err != nil {
			return "", fmt.Errorf("git commit: %w", err)
		}
		if _, err := t.run("git", "push", "-q", "--force", "origin", branch); err != nil {
			return "", fmt.Errorf("git push: %w", err)
		}
		url, err := t.run("gh", "pr", "create", "--repo", o.Repo, "--base", o.Base, "--head", branch, "--title", title, "--body", prBody)
		if err != nil {
			return "", fmt.Errorf("gh pr create: %w", err)
		}
		url = strings.TrimSpace(url)
		n = url[strings.LastIndex(url, "/")+1:]
		method, err := t.run("gh", "api", "repos/"+o.Repo, "--jq", `if .allow_rebase_merge then "rebase" elif .allow_squash_merge then "squash" else "merge" end`)
		if err != nil {
			return "", fmt.Errorf("gh api: %w", err)
		}
		method = strings.TrimSpace(method)
		if t.show(nil, "gh", "pr", "merge", n, "--repo", o.Repo, "--auto", "--"+method) != nil &&
			t.show(nil, "gh", "pr", "merge", n, "--repo", o.Repo, "--"+method) != nil {
			return "", t.fail("could not arm or perform the merge of #%s", n)
		}
		t.summary(fmt.Sprintf("opened #%s for the %s heading and armed its merge", n, next))
	}

	wait, err := strconv.Atoi(strings.TrimSpace(o.WaitMinutes))
	if err != nil {
		return "", fmt.Errorf("WAIT_MINUTES %q: %w", o.WaitMinutes, err)
	}
	poll := 30
	if o.PollSeconds != "" {
		if poll, err = strconv.Atoi(strings.TrimSpace(o.PollSeconds)); err != nil {
			return "", fmt.Errorf("POLL_SECONDS %q: %w", o.PollSeconds, err)
		}
	}
	start := o.Now()
	deadline := start.Add(time.Duration(wait) * time.Minute)
	for {
		state, err := t.run("gh", "pr", "view", n, "--repo", o.Repo, "--json", "state", "--jq", ".state")
		if err != nil {
			return "", fmt.Errorf("gh pr view: %w", err)
		}
		state = strings.TrimSpace(state)
		if state == "MERGED" {
			break
		}
		if state == "CLOSED" {
			return "", t.fail("#%s was closed without merging — no tag cut; the next run opens a fresh one", n)
		}
		if !o.Now().Before(deadline) {
			return "", t.fail("#%s did not merge within %s minutes (checks, or a review the ruleset wants) — no tag cut; the next run resumes it", n, o.WaitMinutes)
		}
		o.Sleep(time.Duration(poll) * time.Second)
	}
	merged, err := t.run("gh", "pr", "view", n, "--repo", o.Repo, "--json", "mergeCommit", "--jq", ".mergeCommit.oid")
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	target = strings.TrimSpace(merged)
	if _, err := t.run("git", "fetch", "-q", "origin", o.Base); err != nil {
		return "", fmt.Errorf("git fetch: %w", err)
	}
	_, ancestor := t.run("git", "merge-base", "--is-ancestor", target, "origin/"+o.Base)
	content, shown := t.run("git", "show", target+":"+cl)
	if ancestor != nil || shown != nil || !hasHeading(content, next) {
		return "", t.fail("the merge commit %s of #%s is not on %s with the %s heading — no tag cut", target, n, o.Base, next)
	}
	_, _ = t.run("git", "push", "-q", "origin", "--delete", branch)
	return target, nil
}
