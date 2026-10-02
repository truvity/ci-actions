// Package devboxparity is the port of the devbox-parity composite action: one
// repository's version parity, as a pull request.
//
// devbox LEADS; the other manifests FOLLOW. The toolchain in the shell is
// what builds, lints and tests, so anything that names the same tool
// elsewhere has to agree with the devbox pin, never run ahead of it:
//
//	pair        leader (devbox)          follower
//	go          the go binary + the Go   go.mod's `toolchain` directive;
//	            golangci-lint was built  never the `go` language line
//	            with (nixpkgs)
//	playwright  playwright-driver /      @playwright/test and playwright in
//	            playwright-test (nixpkgs) package.json (browsers come from
//	            nixpkgs; npm must not get ahead)
//
// devbox, git and gh stay what they were: programs this package executes.
// What moved into Go is the logic around them: the mode, the version
// comparisons, the go.mod and package.json reading, the movement gate and
// the auto-merge gate.
package devboxparity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/truvity/ci-actions/internal/gates"
	"github.com/truvity/ci-actions/internal/ghapi"
)

// Options are the action's inputs and the runner's environment.
type Options struct {
	Token         string
	WorkDir       string // working-directory
	Base          string
	Label         string
	Mode          string
	FullDay       string
	ModuleDirs    string // JSON array
	GitUser       string
	GitEmail      string
	GoDLURL       string // test seam: the go.dev release list
	APIURL        string // GitHub API base; empty means the public API
	GHVersion     string // test seam: the gh release to install when it is missing
	GHBaseURL     string // test seam: where gh archives are downloaded from
	RunnerTemp    string
	GithubOutput  string
	GithubPath    string
	Environ       []string
	Now           func() time.Time
	Out, Err      io.Writer
	Exec          Exec
	HTTPClient    *ghapi.Client // nil builds one from APIURL and Token
	ArchOverride  string        // test seam: uname -m
	SkipGHInstall bool
}

type run struct {
	o   Options
	env []string // environment of children, without the token
}

func (r *run) printf(format string, a ...any) { fmt.Fprintf(r.o.Out, format, a...) }
func (r *run) stdout() io.Writer              { return r.o.Out }
func (r *run) stderr() io.Writer              { return r.o.Err }

// cmd runs a program in dir, relative to the working directory ("." is it).
func (r *run) cmd(ctx context.Context, dir string, stdout, stderr io.Writer, name string, args ...string) error {
	d := dir
	if !filepath.IsAbs(d) {
		d = filepath.Join(r.o.WorkDir, d)
	}
	return r.o.Exec(ctx, Cmd{Dir: d, Env: r.env, Name: name, Args: args, Stdout: stdout, Stderr: stderr})
}

// devbox runs `devbox run -- <args>` in dir. The shell version reached jq
// through the runner's own, never `devbox run -- jq` (devbox re-quotes argv
// and mangles the program); nothing here does either.
func (r *run) devbox(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	return r.cmd(ctx, dir, stdout, stderr, "devbox", append([]string{"run", "--"}, args...)...)
}

func (o *Options) defaults() {
	if o.WorkDir == "" {
		o.WorkDir = "."
	}
	if o.Base == "" {
		o.Base = "master"
	}
	if o.Label == "" {
		o.Label = "dependencies"
	}
	if o.FullDay == "" {
		o.FullDay = "1"
	}
	if o.ModuleDirs == "" {
		o.ModuleDirs = "[]"
	}
	if o.GitUser == "" {
		o.GitUser = "github-actions[bot]"
	}
	if o.GitEmail == "" {
		o.GitEmail = "41898282+github-actions[bot]@users.noreply.github.com"
	}
	if o.Mode == "" {
		o.Mode = "auto"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Exec == nil {
		o.Exec = OSExec
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.GHVersion == "" {
		o.GHVersion = "2.97.0"
	}
	if o.GHBaseURL == "" {
		o.GHBaseURL = "https://github.com/cli/cli/releases/download"
	}
}

// Run does the whole action. Its output lines are the shell version's; the
// token reaches only the processes of the pull-request phase.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	r := &run{o: o, env: withEnv(o.Environ, []string{"TOKEN", "GH_TOKEN", "GITHUB_TOKEN"})}

	if !o.SkipGHInstall {
		if err := r.ensureGH(ctx); err != nil {
			return err
		}
	}

	// Decide the mode.
	weekday := int(o.Now().UTC().Weekday())
	if weekday == 0 {
		weekday = 7
	}
	var full bool
	switch o.Mode {
	case "full":
		full = true
	case "align":
	default:
		full = fmt.Sprint(weekday) == o.FullDay
	}
	r.printf("mode=%s (weekday %d, full-update-day %s) -> full=%t\n", o.Mode, weekday, o.FullDay, full)

	if full {
		if err := r.cmd(ctx, ".", r.stdout(), r.stderr(), "devbox", "update"); err != nil {
			return fmt.Errorf("devbox update: %w", err)
		}
	}

	r.group("Pair: go toolchain follows the devbox go")
	if err := r.alignGo(ctx); err != nil {
		return err
	}
	r.endgroup()
	r.group("Pair: @playwright/test follows the devbox playwright")
	if err := r.alignPlaywright(ctx); err != nil {
		return err
	}
	r.endgroup()
	r.group("Open a pull request if anything moved")
	err := r.openPR(ctx)
	r.endgroup()
	return err
}

func (r *run) group(name string) { r.printf("::group::%s\n", name) }
func (r *run) endgroup()         { r.printf("::endgroup::\n") }

func (r *run) setOutput(k, v string) error {
	if r.o.GithubOutput == "" {
		return nil
	}
	f, err := os.OpenFile(r.o.GithubOutput, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s=%s\n", k, v)
	return err
}

// openPR is the last step: the movement gate, one fixed branch, the pull
// request, and auto-merge only where a required check exists.
func (r *run) openPR(ctx context.Context) error {
	o := r.o
	r.env = withEnv(r.env, nil, "GH_TOKEN="+o.Token)
	wd := "."

	// The movement gate names every follower, so a change to any of them is
	// a PR: a go.mod-only change was invisible before v2.10.0.
	paths := []string{"devbox.json", "devbox.lock", "go.mod", "package.json", "yarn.lock", "pnpm-lock.yaml", "package-lock.json"}
	var dirs []string
	if err := unmarshalStrings(o.ModuleDirs, &dirs); err != nil {
		return fmt.Errorf("module-dirs is not a JSON array of strings: %w", err)
	}
	for _, d := range dirs {
		paths = append(paths, d+"/go.mod")
	}

	if r.cmd(ctx, wd, io.Discard, io.Discard, "git", append([]string{"diff", "--quiet", "--"}, paths...)...) == nil {
		r.printf("everything already at parity\n")
		return r.setOutput("changed", "false")
	}

	// ONE fixed branch, force-pushed: a same-day re-run UPDATES the open PR
	// instead of colliding with yesterday's branch name.
	branch := "chore/devbox-update"
	for _, c := range [][]string{
		{"config", "user.name", o.GitUser},
		{"config", "user.email", o.GitEmail},
		{"checkout", "-B", branch},
	} {
		if err := r.cmd(ctx, wd, r.stdout(), r.stderr(), "git", c...); err != nil {
			return fmt.Errorf("git %s: %w", c[0], err)
		}
	}
	// Stage only followers that exist. One `git add` naming a path that is
	// absent (pnpm-lock.yaml in a yarn repository) refuses the WHOLE command
	// and stages nothing.
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(o.WorkDir, p)); err == nil {
			if err := r.cmd(ctx, wd, r.stdout(), r.stderr(), "git", "add", "--", p); err != nil {
				return fmt.Errorf("git add %s: %w", p, err)
			}
		}
	}
	if r.cmd(ctx, wd, io.Discard, io.Discard, "git", "diff", "--cached", "--quiet") == nil {
		var st strings.Builder
		_ = r.cmd(ctx, wd, &st, io.Discard, "git", "status", "--porcelain")
		r.printf("::error::followers changed but nothing was staged: %s\n", strings.ReplaceAll(strings.TrimRight(st.String(), "\n"), "\n", " ")+trailingSpace(st.String()))
		return errReported
	}
	// Inside devbox, like the push below: a repository whose pre-commit hook
	// calls one of its own tools fails with "command not found" on a hosted
	// runner otherwise. The hooks are the safety net that vets the aligned
	// directive, so they must be able to run, not be skipped.
	if err := r.devbox(ctx, ".", r.stdout(), r.stderr(), "git", "commit", "-m", "chore(deps): update devbox packages and align the followers"); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	if err := r.devbox(ctx, ".", r.stdout(), r.stderr(), "git", "push", "--force", "origin", branch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	if err := r.cmd(ctx, wd, io.Discard, r.stderr(), "gh", "label", "create", o.Label, "--force", "--color", "0366d6", "--description", "Dependency updates"); err != nil {
		return fmt.Errorf("gh label create: %w", err)
	}

	var out strings.Builder
	if err := r.cmd(ctx, wd, &out, r.stderr(), "gh", "pr", "list", "--head", branch, "--state", "open", "--json", "url", "--jq", ".[0].url"); err != nil {
		return fmt.Errorf("gh pr list: %w", err)
	}
	if url := strings.TrimRight(out.String(), "\n"); url != "" {
		r.printf("open PR for %s already exists — updated in place by the push\n", branch)
		if err := r.setOutput("changed", "true"); err != nil {
			return err
		}
		return r.setOutput("url", url)
	}

	out.Reset()
	if err := r.cmd(ctx, wd, &out, r.stderr(), "gh", "pr", "create",
		"--title", "chore(deps): update devbox packages and align the followers",
		"--body", "Automated parity: `devbox update` on the configured weekday, and every follower aligned to its devbox pin — the go toolchain directive (language minor capped by golangci-lint's build Go, patch = newest of that line, never downward) and the playwright npm package (exactly the nix-pinned driver). Nix packages are outside Renovate's reach and the followers are owned here, so the pairs can never disagree.",
		"--base", o.Base, "--label", o.Label); err != nil {
		return fmt.Errorf("gh pr create: %w", err)
	}
	url := strings.TrimRight(out.String(), "\n")

	// Arm auto-merge ONLY where required checks exist. With none, `--auto`
	// merges IMMEDIATELY, unvalidated. A branch can require a check in two
	// unrelated ways (a ruleset, or classic branch protection) and EITHER is
	// a green to wait for. An unreadable source still means "leave it for a
	// human": failing toward a manual merge is the safe side.
	out.Reset()
	if err := r.cmd(ctx, wd, &out, r.stderr(), "gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner"); err != nil {
		return fmt.Errorf("gh repo view: %w", err)
	}
	repo := strings.TrimRight(out.String(), "\n")
	c := o.HTTPClient
	if c == nil {
		c = ghapi.New(o.APIURL, o.Token)
	}
	rulesets := gates.Rulesets(ctx, c, repo, o.Base)
	classic := gates.Classic(ctx, c, repo, o.Base, true)
	if rulesets < 0 {
		rulesets = 0
	}
	if classic < 0 {
		classic = 0
	}
	if rulesets >= 1 || classic >= 1 {
		r.printf("required checks on %s: %d from rulesets, %d from classic protection — arming auto-merge\n", o.Base, rulesets, classic)
		if err := r.cmd(ctx, wd, r.stdout(), r.stderr(), "gh", "pr", "merge", "--auto", "--rebase", branch); err != nil {
			return fmt.Errorf("gh pr merge: %w", err)
		}
	} else {
		r.printf("no required status check on %s — neither a ruleset nor classic branch protection requires one; leaving the PR for a human to merge\n", o.Base)
	}

	if err := r.setOutput("changed", "true"); err != nil {
		return err
	}
	return r.setOutput("url", url)
}

// errReported marks a failure whose ::error:: line is already written.
var errReported = errors.New("devbox-parity: reported")

// ErrReported is for callers that must not print the error again.
func IsReported(err error) bool { return errors.Is(err, errReported) }

func trailingSpace(s string) string {
	// `git status --porcelain | tr '\n' ' '` leaves a space after the last
	// entry as well.
	if s != "" {
		return " "
	}
	return ""
}
