package autorelease

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// rig is a bare origin, a clone whose master moved one commit past its tags,
// and a fake gh and devbox: what the tagging step asserts against is what the
// tag points at in the origin, not what the step said.
type rig struct {
	t       *testing.T
	origin  string
	work    string
	human   bool // a person's minor-release heading PR is open
	never   bool // the heading PR never merges
	ownPR   string
	state   string
	mergeAt string
	title   string
	calls   []string
	devbox  []string
	log     bytes.Buffer
	sum     string
	env     map[string]string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=fixture", "-c", "user.email=fixture@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRig: changelog is the file at HEAD ("" means none), tags are annotated
// on the first commit.
func newRig(t *testing.T, changelog string, tags ...string) *rig {
	t.Helper()
	d := t.TempDir()
	r := &rig{t: t, origin: filepath.Join(d, "o.git"), work: filepath.Join(d, "w"), sum: filepath.Join(d, "sum"), state: "OPEN"}
	git(t, d, "init", "-q", "--bare", "-b", "master", r.origin)
	git(t, d, "init", "-q", "-b", "master", r.work)
	git(t, r.work, "remote", "add", "origin", r.origin)
	if changelog != "" {
		os.WriteFile(filepath.Join(r.work, "CHANGELOG.md"), []byte(changelog+"\n"), 0o644)
	}
	os.WriteFile(filepath.Join(r.work, "dep"), []byte("1\n"), 0o644)
	git(t, r.work, "add", ".")
	git(t, r.work, "commit", "-q", "-m", "init")
	for _, tg := range tags {
		git(t, r.work, "tag", "-a", tg, "-m", tg)
	}
	os.WriteFile(filepath.Join(r.work, "dep"), []byte("2\n"), 0o644)
	git(t, r.work, "commit", "-q", "-am", "chore(deps): bump")
	git(t, r.work, "push", "-q", "origin", "master", "--tags")
	return r
}

// mergeNow is what GitHub does when the checks go green: fast-forward master
// to the heading branch.
func (r *rig) mergeNow() {
	branch := git(r.t, r.origin, "for-each-ref", "--format=%(refname)", "refs/heads/auto-release/")
	sha := git(r.t, r.origin, "rev-parse", branch)
	git(r.t, r.origin, "update-ref", "refs/heads/master", sha)
	r.state, r.mergeAt = "MERGED", sha
}

func (r *rig) exec(ctx context.Context, c runcmd.Cmd) error {
	switch c.Name {
	case "gh":
		a := strings.Join(c.Args, " ")
		r.calls = append(r.calls, a)
		switch {
		case strings.HasPrefix(a, "pr list") && strings.Contains(a, "--head"):
			c.Stdout.Write([]byte(r.ownPR + "\n"))
		case strings.HasPrefix(a, "pr list"):
			if r.human {
				c.Stdout.Write([]byte("1\n"))
			} else {
				c.Stdout.Write([]byte("0\n"))
			}
		case strings.HasPrefix(a, "pr create"):
			for i, x := range c.Args {
				if x == "--title" {
					r.title = c.Args[i+1]
				}
			}
			r.ownPR, r.state = "7", "OPEN"
			c.Stdout.Write([]byte("https://github.com/o/r/pull/7\n"))
		case strings.HasPrefix(a, "pr merge"):
			if !r.never {
				r.mergeNow()
			}
		case strings.HasPrefix(a, "pr view") && strings.Contains(a, "mergeCommit"):
			c.Stdout.Write([]byte(r.mergeAt + "\n"))
		case strings.HasPrefix(a, "pr view"):
			c.Stdout.Write([]byte(r.state + "\n"))
		case a == "api repos/o/r --jq if .allow_rebase_merge then \"rebase\" elif .allow_squash_merge then \"squash\" else \"merge\" end":
			c.Stdout.Write([]byte("rebase\n"))
		default:
			r.t.Fatalf("unexpected gh call: %s", a)
		}
		return nil
	case "devbox":
		r.devbox = append(r.devbox, strings.Join(c.Args, " "))
		if len(c.Args) < 3 || c.Args[0] != "run" || c.Args[1] != "--" {
			r.t.Fatalf("unexpected devbox call: %v", c.Args)
		}
		c.Name, c.Args = c.Args[2], c.Args[3:]
	}
	return runcmd.OS(ctx, c)
}

func (r *rig) run(over ...string) error {
	r.t.Helper()
	o := TagOptions{Prefix: "v", Bot: "bot", Repo: "o/r", Base: "master", WaitMinutes: "0", PollSeconds: "0",
		Summary: r.sum, Dir: r.work, Out: &r.log, Err: &r.log, Exec: r.exec,
		Now: func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }, Sleep: func(time.Duration) {}}
	for _, kv := range over {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HEADING_MODE":
			o.HeadingMode = v
		case "VERSION_BUMP_COMMAND":
			o.VersionBumpCommand = v
		}
	}
	return Tag(context.Background(), o)
}

func (r *rig) at(rev, file string) string { return git(r.t, r.origin, "show", rev+"^{commit}:"+file) }
func (r *rig) hasTag(tag string) bool {
	return exec.Command("git", "--git-dir", r.origin, "rev-parse", "-q", "--verify", "refs/tags/"+tag).Run() == nil
}
func (r *rig) onMaster(tag string) bool {
	return git(r.t, r.origin, "rev-parse", tag+"^{commit}") == git(r.t, r.origin, "rev-parse", "master")
}
func (r *rig) summary() string { b, _ := os.ReadFile(r.sum); return string(b) }
func (r *rig) prs() int {
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, "pr create") {
			n++
		}
	}
	return n
}

func has(t *testing.T, text, line string) bool {
	t.Helper()
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v\n", err)
	}
}

const (
	unrel   = "# Changelog\n\n## Unreleased\n\n- Something a consumer sees.\n\n## v1.2.0\n\n- First."
	nopatch = "# Changelog\n\n## Unreleased\n\n## v1.2.0\n\n- First."
	bump    = `printf "%s\n" "$VERSION" > ver; printf "%s\n" "$TAG" > tagname`
)

func TestRenameUnreleased(t *testing.T) {
	r := newRig(t, unrel, "v1.2.0")
	must(t, r.run())
	if !r.onMaster("v1.2.1") {
		t.Error("the tag must name master's tip, which carries the heading")
	}
	cl := r.at("v1.2.1", "CHANGELOG.md")
	if !has(t, cl, "## v1.2.1") || strings.Contains(cl, "## Unreleased") || !has(t, cl, "- Something a consumer sees.") {
		t.Errorf("heading not renamed:\n%s", cl)
	}
	if r.title != "docs(changelog): heading for the v1.2.1 release" {
		t.Errorf("title %q", r.title)
	}
	if !contains(r.calls, "pr merge 7 --repo o/r --auto --rebase") {
		t.Errorf("merge not armed with the repository's method: %v", r.calls)
	}
	if !strings.Contains(r.summary(), "opened #7 for the v1.2.1 heading and armed its merge\n") || !strings.HasSuffix(r.summary(), "released v1.2.1\n") {
		t.Errorf("summary %q", r.summary())
	}
	if out := git(t, r.origin, "for-each-ref", "refs/heads/auto-release/"); out != "" {
		t.Errorf("heading branch left behind: %s", out)
	}
	if m := git(t, r.origin, "tag", "-n1", "v1.2.1"); !strings.Contains(m, "v1.2.1 — automatic release: master moved past v1.2.0 (renovate automerge)") {
		t.Errorf("tag message %q", m)
	}
	if b := git(t, r.origin, "log", "-1", "--format=%an <%ae>", "v1.2.1^{commit}"); b != "bot[bot] <bot[bot]@users.noreply.github.com>" {
		t.Errorf("committer %q", b)
	}
	if b := git(t, r.origin, "log", "-1", "--format=%b", "v1.2.1^{commit}"); !strings.HasPrefix(b, "Written by the shared auto-release workflow so that the tag names the commit that carries its own heading.") || strings.Contains(b, "version bump") {
		t.Errorf("commit body %q", b)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestHeadingShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		changelog string
		tags      []string
		over      []string
		check     func(t *testing.T, r *rig, tag, cl string)
	}{
		{"dated headings date the new one (UTC)", "# Changelog\n\n## Unreleased\n\n- Entry.\n\n## v1.2.0 — 2026-09-01\n\n- First.", []string{"v1.2.0"}, nil,
			func(t *testing.T, r *rig, tag, cl string) {
				if !has(t, cl, "## v1.2.1 — 2026-10-04") {
					t.Errorf("not dated:\n%s", cl)
				}
			}},
		{"undated headings stay undated", unrel, []string{"v1.2.0"}, nil,
			func(t *testing.T, r *rig, tag, cl string) {
				if strings.Contains(cl, "## v1.2.1 ") {
					t.Errorf("dated:\n%s", cl)
				}
			}},
		{"sub-headings under Unreleased move with it", "# Changelog\n\n## Unreleased\n\n### Contracts\n\n- Entry.\n\n## v1.2.0\n\n- First.", []string{"v1.2.0"}, nil,
			func(t *testing.T, r *rig, tag, cl string) {
				if !has(t, cl, "### Contracts") || !has(t, cl, "## v1.2.1") {
					t.Errorf("\n%s", cl)
				}
			}},
		{"empty Unreleased, newest patch has a heading: Dependency updates", "# Changelog\n\n## Unreleased\n\n## v1.2.3\n\n- A patch with its own heading.\n\n## v1.2.0\n\n- First.", []string{"v1.2.0", "v1.2.3"}, nil,
			func(t *testing.T, r *rig, tag, cl string) {
				var h []string
				for _, l := range strings.Split(cl, "\n") {
					if strings.HasPrefix(l, "## ") || strings.HasPrefix(l, "- ") {
						h = append(h, l)
					}
				}
				if got := strings.Join(h, "|"); got != "## Unreleased|## v1.2.4|- Dependency updates.|## v1.2.3|- A patch with its own heading.|## v1.2.0|- First." {
					t.Errorf("got %s", got)
				}
			}},
		{"always writes the Dependency updates heading even with no convention", nopatch, []string{"v1.2.0"}, []string{"HEADING_MODE=always"},
			func(t *testing.T, r *rig, tag, cl string) {
				if !has(t, cl, "- Dependency updates.") {
					t.Errorf("\n%s", cl)
				}
			}},
		{"no Unreleased at all, patch convention: placed above the newest version", "# Changelog\n\n## v1.2.3\n\n- Patch.\n\n## v1.2.0\n\n- First.", []string{"v1.2.0", "v1.2.3"}, nil,
			func(t *testing.T, r *rig, tag, cl string) {
				for _, l := range strings.Split(cl, "\n") {
					if strings.HasPrefix(l, "## ") {
						if l != "## v1.2.4" {
							t.Errorf("first heading %q", l)
						}
						return
					}
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.changelog, tc.tags...)
			must(t, r.run(tc.over...))
			tag := "v1.2.1"
			if len(tc.tags) == 2 {
				tag = "v1.2.4"
			}
			if !r.onMaster(tag) {
				t.Errorf("tag %s is not on master", tag)
			}
			tc.check(t, r, tag, r.at(tag, "CHANGELOG.md"))
		})
	}
}

func TestNoHeadingPR(t *testing.T) {
	for _, tc := range []struct {
		name, changelog string
		tags            []string
		over            []string
		sum, warn       string
		keep            string // a line the tagged file must still contain
	}{
		{"empty Unreleased, no patch heading convention: C5 exempts it", nopatch, []string{"v1.2.0"}, nil, "no entries under Unreleased and no patch heading convention — v1.2.1 needs none (C5's automatic-patch case)", "", ""},
		{"never: tag on HEAD, file untouched", unrel, []string{"v1.2.0"}, []string{"HEADING_MODE=never"}, "", "", "## Unreleased"},
		{"no CHANGELOG file", "", []string{"v1.2.0"}, nil, "", "", ""},
		{"Unreleased not first: warns, rewrites nothing", "# Changelog\n\n## v1.2.0\n\n- First.\n\n## Unreleased\n\n- Misplaced.", []string{"v1.2.0"}, nil, "", "::warning::CHANGELOG.md has an Unreleased heading that is not the first — leaving it alone\n", "## Unreleased"},
		{"heading for the next version already exists: tagged as it stands", "# Changelog\n\n## Unreleased\n\n- Entry.\n\n## v1.2.1\n\n- Already written by hand.\n\n## v1.2.0\n\n- First.", []string{"v1.2.0"}, nil, "CHANGELOG.md already has a heading for v1.2.1 — tagging as it stands", "", "## Unreleased"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.changelog, tc.tags...)
			must(t, r.run(tc.over...))
			if !r.onMaster("v1.2.1") || r.prs() != 0 {
				t.Errorf("tag on master %v, PRs %d", r.onMaster("v1.2.1"), r.prs())
			}
			if tc.sum != "" && !strings.Contains(r.summary(), tc.sum+"\n") {
				t.Errorf("summary %q, want %q", r.summary(), tc.sum)
			}
			if tc.warn != "" && !strings.Contains(r.log.String(), tc.warn) {
				t.Errorf("log %q, want %q", r.log.String(), tc.warn)
			}
			if tc.changelog != "" {
				cl := r.at("v1.2.1", "CHANGELOG.md")
				if cl != tc.changelog {
					t.Errorf("file changed:\n%s", cl)
				}
			}
		})
	}
}

func TestNothingToRelease(t *testing.T) {
	r := newRig(t, unrel, "v1.2.0")
	git(t, r.work, "tag", "-a", "v1.2.1", "-m", "x")
	git(t, r.work, "push", "-q", "origin", "v1.2.1")
	must(t, r.run())
	if r.hasTag("v1.2.2") || r.prs() != 0 || !strings.Contains(r.summary(), "master == v1.2.1 — nothing to release\n") {
		t.Errorf("summary %q", r.summary())
	}

	r = newRig(t, unrel)
	must(t, r.run())
	if r.prs() != 0 || !strings.Contains(r.summary(), "no v* releases yet — a human cuts the first one\n") {
		t.Errorf("summary %q", r.summary())
	}
}

func TestStandsAsideForAPersonsReleasePR(t *testing.T) {
	r := newRig(t, unrel, "v1.2.0")
	r.human = true
	must(t, r.run())
	if r.hasTag("v1.2.1") || r.prs() != 0 || !strings.Contains(r.summary(), "leaving v1.2.1 to it") {
		t.Errorf("summary %q", r.summary())
	}
	// The same state the gh query filters on: a minor or major, not a patch.
	for title, want := range map[string]bool{
		"docs(changelog): heading for the v1.3.0 release":  true,
		"docs(changelog): heading for the v2.0.0 release":  true,
		"docs(changelog): heading for the v1.2.1 release":  false,
		"docs(changelog): heading for the v1.2.10 release": false,
		"feat: heading for the v1.3.0 release":             false,
	} {
		if got := waitingForPerson.MatchString(title); got != want {
			t.Errorf("%q: %v want %v", title, got, want)
		}
	}
}

func TestPRNeverMergesThenResumes(t *testing.T) {
	r := newRig(t, unrel, "v1.2.0")
	r.never = true
	err := r.run()
	if !errors.Is(err, ErrReported) || r.hasTag("v1.2.1") {
		t.Fatalf("err %v, tag %v", err, r.hasTag("v1.2.1"))
	}
	if !strings.Contains(r.log.String(), "::error::#7 did not merge within 0 minutes (checks, or a review the ruleset wants) — no tag cut; the next run resumes it") {
		t.Errorf("log %q", r.log.String())
	}
	r.never = false
	r.mergeNow()
	git(t, r.work, "checkout", "-q", "master")
	must(t, r.run())
	if r.prs() != 1 || !r.onMaster("v1.2.1") || !has(t, r.at("v1.2.1", "CHANGELOG.md"), "## v1.2.1") {
		t.Errorf("PRs %d", r.prs())
	}
	if !strings.Contains(r.summary(), "resuming #7, the heading PR an earlier run opened for v1.2.1\n") {
		t.Errorf("summary %q", r.summary())
	}
}

func TestPRClosedUnmerged(t *testing.T) {
	r := newRig(t, unrel, "v1.2.0")
	r.never = true
	_ = r.run()
	r.state = "CLOSED"
	git(t, r.work, "checkout", "-q", "master")
	err := r.run()
	if !errors.Is(err, ErrReported) || r.hasTag("v1.2.1") || !strings.Contains(r.log.String(), "::error::#7 was closed without merging — no tag cut; the next run opens a fresh one") {
		t.Errorf("err %v log %q", err, r.log.String())
	}
}

func TestMergeCommitMustCarryTheHeading(t *testing.T) {
	r := newRig(t, unrel, "v1.2.0")
	r.never = true
	_ = r.run()
	// The PR "merged", but master does not hold its commit.
	r.state, r.mergeAt = "MERGED", git(t, r.work, "rev-parse", "HEAD")
	git(t, r.work, "checkout", "-q", "master")
	err := r.run()
	if !errors.Is(err, ErrReported) || r.hasTag("v1.2.1") || !strings.Contains(r.log.String(), "is not on master with the v1.2.1 heading — no tag cut") {
		t.Errorf("err %v log %q", err, r.log.String())
	}
}

func TestVersionBump(t *testing.T) {
	t.Run("bump rides in the heading commit", func(t *testing.T) {
		r := newRig(t, unrel, "v1.2.0")
		must(t, r.run("VERSION_BUMP_COMMAND="+bump))
		if !r.onMaster("v1.2.1") || r.at("v1.2.1", "ver") != "1.2.1" || r.at("v1.2.1", "tagname") != "v1.2.1" || !has(t, r.at("v1.2.1", "CHANGELOG.md"), "## v1.2.1") {
			t.Error("tag, VERSION, TAG or heading wrong")
		}
		if git(t, r.origin, "rev-list", "--count", "v1.2.0..v1.2.1") != "2" ||
			strings.ReplaceAll(git(t, r.origin, "diff", "--name-only", "v1.2.1~1", "v1.2.1"), "\n", ",") != "CHANGELOG.md,tagname,ver" {
			t.Error("one heading commit must carry all three files")
		}
		if len(r.devbox) != 1 || r.prs() != 1 {
			t.Errorf("devbox %v PRs %d", r.devbox, r.prs())
		}
		if b := git(t, r.origin, "log", "-1", "--format=%b", "v1.2.1^{commit}"); !strings.Contains(b, "It also carries the version bump of the version-bump-command input.") {
			t.Errorf("body %q", b)
		}
	})
	t.Run("a bump that changes nothing fails loudly", func(t *testing.T) {
		r := newRig(t, unrel, "v1.2.0")
		err := r.run("VERSION_BUMP_COMMAND=true")
		if !errors.Is(err, ErrReported) || !strings.Contains(r.log.String(), "::error::version-bump-command changed nothing") ||
			r.prs() != 0 || r.hasTag("v1.2.1") || git(t, r.origin, "for-each-ref", "refs/heads/auto-release/") != "" {
			t.Errorf("err %v log %q", err, r.log.String())
		}
	})
	t.Run("a bump that fails: no PR, no tag", func(t *testing.T) {
		r := newRig(t, unrel, "v1.2.0")
		err := r.run("VERSION_BUMP_COMMAND=false")
		if err == nil || errors.Is(err, ErrReported) || r.prs() != 0 || r.hasTag("v1.2.1") {
			t.Errorf("err %v", err)
		}
		if runcmd.ExitCode(err) != 1 {
			t.Errorf("exit code %d", runcmd.ExitCode(err))
		}
	})
	t.Run("empty bump: unchanged behaviour", func(t *testing.T) {
		r := newRig(t, unrel, "v1.2.0")
		must(t, r.run("VERSION_BUMP_COMMAND="))
		if len(r.devbox) != 0 || git(t, r.origin, "diff", "--name-only", "v1.2.1~1", "v1.2.1") != "CHANGELOG.md" {
			t.Error("devbox ran, or the heading commit holds more than the changelog")
		}
	})
	t.Run("bump with changelog-heading never is refused", func(t *testing.T) {
		r := newRig(t, unrel, "v1.2.0")
		err := r.run("VERSION_BUMP_COMMAND="+bump, "HEADING_MODE=never")
		if !errors.Is(err, ErrReported) || !strings.Contains(r.log.String(), "cannot be combined with changelog-heading: never") || r.prs() != 0 || r.hasTag("v1.2.1") || len(r.devbox) != 0 {
			t.Errorf("err %v", err)
		}
	})
	t.Run("a heading a person wrote: the bump is theirs", func(t *testing.T) {
		r := newRig(t, "# Changelog\n\n## Unreleased\n\n- Entry.\n\n## v1.2.1\n\n- By hand.\n\n## v1.2.0\n\n- First.", "v1.2.0")
		must(t, r.run("VERSION_BUMP_COMMAND="+bump))
		if len(r.devbox) != 0 || r.prs() != 0 || !r.onMaster("v1.2.1") || !strings.Contains(r.summary(), "version-bump-command skipped") {
			t.Errorf("summary %q", r.summary())
		}
	})
	t.Run("dependency-only patch still gets a PR for the bump", func(t *testing.T) {
		r := newRig(t, nopatch, "v1.2.0")
		must(t, r.run("VERSION_BUMP_COMMAND="+bump))
		if !has(t, r.at("v1.2.1", "CHANGELOG.md"), "- Dependency updates.") || r.at("v1.2.1", "ver") != "1.2.1" {
			t.Error("heading and bump must both be in the tagged commit")
		}
	})
	t.Run("no PR possible: refused, no tag", func(t *testing.T) {
		r := newRig(t, "# Changelog\n\n## v1.2.0\n\n- First.\n\n## Unreleased\n\n- Misplaced.", "v1.2.0")
		err := r.run("VERSION_BUMP_COMMAND=" + bump)
		if !errors.Is(err, ErrReported) || r.hasTag("v1.2.1") || !strings.Contains(r.log.String(), "::error::version-bump-command is set but no heading pull request can carry the bump") {
			t.Errorf("err %v log %q", err, r.log.String())
		}
	})
	t.Run("resumed open PR is tagged, not re-bumped", func(t *testing.T) {
		const cmd = "echo bumped >> bump.log"
		r := newRig(t, unrel, "v1.2.0")
		r.never = true
		if err := r.run("VERSION_BUMP_COMMAND=" + cmd); err == nil {
			t.Fatal("the unmerged PR must fail the run")
		}
		r.never = false
		r.mergeNow()
		git(t, r.work, "checkout", "-q", "master")
		must(t, r.run("VERSION_BUMP_COMMAND="+cmd))
		if len(r.devbox) != 1 || r.prs() != 1 || r.at("v1.2.1", "bump.log") != "bumped" {
			t.Errorf("devbox %d PRs %d", len(r.devbox), r.prs())
		}
	})
}

func TestNextPatch(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.0": "v1.2.1", "v1.2.9": "v1.2.10", "1.0.99": "1.0.100", "v1.2.3-rc1": "v1.2.4", "v1.2": "v1.2.1", "v1": "v1..1", "v1.2.3.4": "v1.2.4",
	} {
		if got := nextPatch(in); got != want {
			t.Errorf("nextPatch(%q) = %q, want %q", in, got, want)
		}
	}
}
