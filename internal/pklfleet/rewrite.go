// Package pklfleet is the port of the shell steps of the shared pkl-fleet
// workflow that move one consumer repository to a new release of a Pkl
// contracts library: rewriting the dependency URIs, resolving and regenerating,
// and opening or updating the pull request. git, gh-less API calls, devbox,
// just and pkl: the API is read with net/http, the programs stay executed.
package pklfleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// ErrFailed marks a failure whose message is already written.
var ErrFailed = errors.New("pkl-fleet: failed")

// Common is what every step shares.
type Common struct {
	Out  io.Writer
	Err  io.Writer
	Exec runcmd.Exec
	Dir  string // the checkout; "" is the working directory

	Output  string // GITHUB_OUTPUT
	Summary string // GITHUB_STEP_SUMMARY
}

func (c *Common) init() {
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.Err == nil {
		c.Err = io.Discard
	}
	if c.Exec == nil {
		c.Exec = runcmd.OS
	}
}

func (c Common) fail(format string, a ...any) error {
	fmt.Fprintf(c.Out, "::error::"+format+"\n", a...)
	return ErrFailed
}

func appendFile(path, text string) error {
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

func (c Common) at(p string) string {
	if c.Dir == "" {
		return p
	}
	return path.Join(c.Dir, p)
}

// out runs a command and returns its stdout.
func (c Common) out(ctx context.Context, env []string, name string, args ...string) (string, error) {
	var b bytes.Buffer
	err := c.Exec(ctx, runcmd.Cmd{Dir: c.Dir, Env: env, Name: name, Args: args, Stdout: &b, Stderr: c.Err})
	return b.String(), err
}

// ── version arithmetic ───────────────────────────────────────────────────

const verPattern = `[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?`

var (
	verOnly    = regexp.MustCompile(`^` + verPattern + `$`)
	sourceOnly = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// order is dpkg's character order, which GNU `sort -V` follows for the
// shapes versions take here (digits, letters, `.`, `-`, `~`): the end of the
// string and digits weigh nothing, `~` sorts before everything, letters
// before other characters.
func order(s string, i int) int {
	if i >= len(s) || isDigit(s[i]) {
		return 0
	}
	c := s[i]
	switch {
	case c == '~':
		return -1
	case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		return int(c)
	}
	return int(c) + 256
}

// versionCmp is `sort -V` between two version strings.
func versionCmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			if ac, bc := order(a, i), order(b, j); ac != bc {
				return ac - bc
			}
			i++
			j++
		}
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		diff := 0
		for i < len(a) && isDigit(a[i]) && j < len(b) && isDigit(b[j]) {
			if diff == 0 {
				diff = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isDigit(a[i]) {
			return 1
		}
		if j < len(b) && isDigit(b[j]) {
			return -1
		}
		if diff != 0 {
			return diff
		}
	}
	return 0
}

func before(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

func major(v string) string { return before(v, ".") }

func minor(v string) string {
	_, rest, _ := strings.Cut(v, ".")
	return before(rest, ".")
}

// ── rewrite ──────────────────────────────────────────────────────────────

// RewriteOptions are the "Rewrite the dependency URIs" step's environment.
type RewriteOptions struct {
	Common
	Target   string // TARGET
	Source   string // SOURCE: owner/name of the library
	DirsFile string // DIRS_FILE
}

// Rewrite moves every dependency on Source in every tracked PklProject and
// PklProject.deps.json to Target. Pass one reads everything and refuses on any
// URI it cannot say what to do with, before a single byte is written.
func Rewrite(ctx context.Context, o RewriteOptions) error {
	o.init()
	if !verOnly.MatchString(o.Target) {
		return o.fail("target '%s' is not a version (X.Y.Z, optionally -prerelease)", o.Target)
	}
	if !sourceOnly.MatchString(o.Source) {
		return o.fail("source '%s' is not owner/name", o.Source)
	}
	src := regexp.QuoteMeta(o.Source)
	find := regexp.MustCompile(`(?:project)?package://github\.com/` + src + `/releases/download/[^"' #]*`)
	uriRE := regexp.MustCompile(`^(?:project)?package://github\.com/` + src + `/releases/download/v(?P<tag>` + verPattern + `)/(?P<name>[A-Za-z0-9._-]+)@(?P<at>` + verPattern + `|[0-9]+)$`)
	tcore := before(o.Target, "-")
	tmaj, tmin := major(o.Target), minor(o.Target)

	listing, err := o.out(ctx, nil, "git", "ls-files")
	if err != nil {
		return fmt.Errorf("git ls-files: %w", err)
	}
	var projects []string
	for _, p := range strings.Split(strings.TrimSuffix(listing, "\n"), "\n") {
		if p != "" && path.Base(p) == "PklProject" {
			projects = append(projects, p)
		}
	}
	if len(projects) == 0 {
		return o.fail("no PklProject in this repository")
	}

	bad := false
	stale := map[string]bool{}
	olds := map[string]bool{}
	dirs := map[string]bool{}
	for _, p := range projects {
		d := path.Dir(p)
		for _, f := range []string{p, d + "/PklProject.deps.json"} {
			b, err := os.ReadFile(o.at(f))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			for n, line := range strings.Split(string(b), "\n") {
				for _, uri := range find.FindAllString(line, -1) {
					m := uriRE.FindStringSubmatch(uri)
					if m == nil {
						fmt.Fprintf(o.Out, "::error file=%s,line=%d::'%s' is a dependency into %s but not of the shape v<version>/<name>@<version> — refusing to guess\n", f, n+1, uri, o.Source)
						bad = true
						continue
					}
					tag, at := m[uriRE.SubexpIndex("tag")], m[uriRE.SubexpIndex("at")]
					if strings.Contains(at, ".") {
						if at != tag {
							fmt.Fprintf(o.Out, "::error file=%s,line=%d::'%s' names release v%s but package version %s\n", f, n+1, uri, tag, at)
							bad = true
							continue
						}
					} else if at != major(tag) {
						fmt.Fprintf(o.Out, "::error file=%s,line=%d::'%s' has major %s but release v%s\n", f, n+1, uri, at, tag)
						bad = true
						continue
					}
					core := before(tag, "-")
					if core != tcore && versionCmp(core, tcore) > 0 {
						fmt.Fprintf(o.Out, "::error file=%s,line=%d::v%s is newer than the target v%s — refusing to downgrade\n", f, n+1, tag, o.Target)
						bad = true
						continue
					}
					if tag != o.Target {
						stale[f], olds[tag], dirs[d] = true, true, true
					}
				}
			}
		}
	}
	if bad {
		return ErrFailed
	}

	if len(stale) == 0 {
		fmt.Fprintf(o.Out, "already at v%s — nothing to rewrite\n", o.Target)
		if err := appendFile(o.Output, "changed=false\n"); err != nil {
			return err
		}
		return os.WriteFile(o.DirsFile, nil, 0o644)
	}

	// The full `@<version>` form first (PklProject, and the `projectpackage://`
	// values of deps.json), then the major-only `@<major>` keys of deps.json,
	// which only a different major moves.
	pre := `(package://github\.com/` + src + `/releases/download/)`
	full := regexp.MustCompile(pre + `v` + verPattern + `/([A-Za-z0-9._-]+)@` + verPattern)
	keys := regexp.MustCompile(pre + `v` + verPattern + `/([A-Za-z0-9._-]+)@[0-9]+(["' #]|$)`)
	for f := range stale {
		st, err := os.Stat(o.at(f))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(o.at(f))
		if err != nil {
			return err
		}
		ls := strings.Split(string(b), "\n")
		for i, l := range ls {
			l = full.ReplaceAllString(l, "${1}v"+o.Target+"/${2}@"+o.Target)
			ls[i] = keys.ReplaceAllString(l, "${1}v"+o.Target+"/${2}@"+tmaj+"${3}")
		}
		if err := os.WriteFile(o.at(f), []byte(strings.Join(ls, "\n")), st.Mode().Perm()); err != nil {
			return err
		}
	}
	var dl []string
	for d := range dirs {
		dl = append(dl, d)
	}
	sort.Strings(dl) // LC_ALL=C sort
	if err := os.WriteFile(o.DirsFile, []byte(strings.Join(dl, "\n")+"\n"), 0o644); err != nil {
		return err
	}

	breaking := strings.Contains(o.Target, "-")
	var oldList []string
	for old := range olds {
		oldList = append(oldList, old)
		if major(old) != tmaj {
			breaking = true
		} else if major(old) == "0" && minor(old) != tmin {
			breaking = true
		}
	}
	sort.Slice(oldList, func(i, j int) bool { return versionCmp(oldList[i], oldList[j]) < 0 })
	from := strings.Join(oldList, ",")
	if err := appendFile(o.Output, fmt.Sprintf("changed=true\nbreaking=%t\nfrom=%s\n", breaking, from)); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "moved %d project(s) from v%s to v%s (breaking: %t)\n", len(dl), strings.ReplaceAll(from, ",", ", v"), o.Target, breaking)
	return nil
}

// ── resolve ──────────────────────────────────────────────────────────────

// ResolveOptions are the "Resolve and regenerate" step's environment.
type ResolveOptions struct {
	Common
	DirsFile   string
	PklCommand string
}

// Resolve resolves the rewritten projects with the repository's own `resolve`
// recipe when it has one, else `pkl project resolve` for each, then runs its
// `generate` recipe when it has one, all inside devbox.
func Resolve(ctx context.Context, o ResolveOptions) error {
	o.init()
	inbox := func(args ...string) error {
		return o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: "devbox", Args: append([]string{"run", "--"}, args...), Stdout: o.Out, Stderr: o.Err})
	}
	var b bytes.Buffer
	_ = o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: "devbox", Args: []string{"run", "--", "just", "--summary"}, Stdout: &b, Stderr: io.Discard})
	recipes := " " + strings.TrimSuffix(b.String(), "\n") + " "
	has := func(name string) bool { return strings.Contains(recipes, " "+name+" ") }
	if has("resolve") {
		fmt.Fprintln(o.Out, "resolving with the repository's resolve recipe")
		if err := inbox("just", "resolve"); err != nil {
			return fmt.Errorf("just resolve: %w", err)
		}
	} else {
		pkl := strings.Fields(o.PklCommand)
		dirs, err := os.ReadFile(o.DirsFile)
		if err != nil {
			return err
		}
		for _, d := range strings.Split(string(dirs), "\n") {
			if d == "" {
				continue
			}
			fmt.Fprintf(o.Out, "resolving %s with %s\n", d, strings.Join(pkl, " "))
			if err := inbox(append(append(append([]string{}, pkl...), "project", "resolve"), d)...); err != nil {
				return fmt.Errorf("pkl project resolve %s: %w", d, err)
			}
		}
	}
	if has("generate") {
		fmt.Fprintln(o.Out, "regenerating with the repository's generate recipe")
		if err := inbox("just", "generate"); err != nil {
			return fmt.Errorf("just generate: %w", err)
		}
	}
	return nil
}
