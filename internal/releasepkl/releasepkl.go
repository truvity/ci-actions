// Package releasepkl is the port of the shell steps of the shared
// release-pkl workflow: reading the declared version, refusing a tag that is
// not that version or has no changelog heading, checking the built assets,
// publishing the release (safe to re-run), and smoke-testing the published
// packages. gh, devbox and pkl stay the executed programs.
package releasepkl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// ErrFailed marks a failure whose ::error:: line is already written.
var ErrFailed = errors.New("release-pkl: failed")

// Common is what every step shares.
type Common struct {
	Out    io.Writer
	Err    io.Writer
	Exec   runcmd.Exec
	Output string // GITHUB_OUTPUT
	Dir    string // working directory for executed commands; "" is the current one
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

func (c Common) setOutput(k, v string) error {
	if c.Output == "" {
		return nil
	}
	f, err := os.OpenFile(c.Output, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "%s=%s\n", k, v)
	return f.Close()
}

// lines is what awk sees: the records of text, no empty record after a final
// newline.
func lines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func sum(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func base(path string) string { return path[strings.LastIndex(path, "/")+1:] }

// ── declared ─────────────────────────────────────────────────────────────

// DeclaredOptions are the "Read the declared version" step's environment.
type DeclaredOptions struct {
	Common
	VersionCommand string
}

// Declared runs the caller's version command in devbox and keeps its last
// line, whitespace removed, as `version`. No command declares nothing.
func Declared(ctx context.Context, o DeclaredOptions) error {
	o.init()
	declared := ""
	if o.VersionCommand != "" {
		var b bytes.Buffer
		if err := o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: "devbox", Args: []string{"run", "--", "bash", "-c", o.VersionCommand}, Stdout: &b, Stderr: o.Err}); err != nil {
			return fmt.Errorf("version-command: %w", err)
		}
		out := strings.TrimSuffix(b.String(), "\n")
		last := out[strings.LastIndex(out, "\n")+1:]
		declared = strings.Map(func(r rune) rune {
			if strings.ContainsRune(" \t\n\v\f\r", r) {
				return -1
			}
			return r
		}, last)
		if declared == "" {
			return o.fail("version-command printed nothing")
		}
	}
	return o.setOutput("version", declared)
}

// ── checks ───────────────────────────────────────────────────────────────

// ChecksOptions are the "Refuse a tag ..." step's environment.
type ChecksOptions struct {
	Common
	RefType   string
	Tag       string
	Declared  string
	Changelog string
	NotesFile string
}

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// Checks refuses a tag that is not v plus the declared version, or whose
// changelog has no `## <tag>` heading, and writes that section as the notes.
func Checks(ctx context.Context, o ChecksOptions) error {
	o.init()
	if o.RefType != "tag" {
		return o.fail("release-pkl runs on a tag push, not on a %s", o.RefType)
	}
	if !strings.HasPrefix(o.Tag, "v") {
		return o.fail("tag '%s' does not start with v", o.Tag)
	}
	version := o.Declared
	if version == "" {
		version = strings.TrimPrefix(o.Tag, "v")
	}
	if !versionRE.MatchString(version) {
		return o.fail("'%s' is not a version (X.Y.Z, optionally -prerelease)", version)
	}
	if o.Tag != "v"+version {
		return o.fail("tag '%s' is not v plus the declared version '%s' — nothing released", o.Tag, version)
	}
	b, err := os.ReadFile(o.Changelog)
	if err != nil {
		if os.IsNotExist(err) {
			return o.fail("%s not found", o.Changelog)
		}
		return err
	}
	// The heading convention auto-release uses: `## vX.Y.Z`, then anything.
	found := false
	var notes strings.Builder
	on := false
	for _, l := range lines(string(b)) {
		f := strings.Fields(l)
		if len(f) > 0 && f[0] == "##" {
			on = len(f) > 1 && f[1] == o.Tag
			found = found || on
			continue
		}
		if on {
			notes.WriteString(l + "\n")
		}
	}
	if !found {
		return o.fail("%s has no '## %s' heading — nothing released", o.Changelog, o.Tag)
	}
	// The release's notes are that section, up to the next `## `.
	if err := os.WriteFile(o.NotesFile, []byte(notes.String()), 0o644); err != nil {
		return err
	}
	if err := o.setOutput("version", version); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "%s is %s, with a changelog heading\n", o.Tag, version)
	return nil
}

// ── assets ───────────────────────────────────────────────────────────────

// AssetsOptions are the "Check the assets" step's environment.
type AssetsOptions struct {
	Common
	OutputDir string
	Version   string
	Manifest  string
}

// Assets lists every file under the output directory in the manifest and
// checks that each is named for the version and carries a matching checksum.
func Assets(ctx context.Context, o AssetsOptions) error {
	o.init()
	if st, err := os.Stat(o.OutputDir); err != nil || !st.IsDir() {
		return o.fail("%s does not exist — did the recipe write elsewhere?", o.OutputDir)
	}
	var files []string
	err := filepath.WalkDir(o.OutputDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files) // LC_ALL=C sort
	manifest := strings.Join(files, "\n")
	if len(files) > 0 {
		manifest += "\n"
	}
	if err := os.WriteFile(o.Manifest, []byte(manifest), 0o644); err != nil {
		return err
	}
	if len(files) == 0 {
		return o.fail("%s holds no files", o.OutputDir)
	}

	failed := false
	bad := func(format string, a ...any) {
		fmt.Fprintf(o.Out, "::error::"+format+"\n", a...)
		failed = true
	}
	count := map[string]int{}
	for _, f := range files {
		count[base(f)]++
	}
	var dups []string
	for n, c := range count {
		if c > 1 {
			dups = append(dups, n)
		}
	}
	sort.Strings(dups)
	if len(dups) > 0 {
		bad("two files would be uploaded under one name: %s", strings.Join(dups, " ")+" ")
	}
	for _, f := range files {
		name := base(f)
		if !strings.Contains(name, "@"+o.Version) {
			bad("%s does not carry @%s", name, o.Version)
		}
		if strings.HasSuffix(name, ".sha256") {
			if st, err := os.Stat(strings.TrimSuffix(f, ".sha256")); err != nil || !st.Mode().IsRegular() {
				bad("%s has no asset beside it to be the checksum of", name)
			}
			continue
		}
		cb, err := os.ReadFile(f + ".sha256")
		if err != nil {
			bad("%s has no %s.sha256", name, name)
			continue
		}
		var firsts []string
		for _, l := range lines(string(cb)) {
			if fl := strings.Fields(l); len(fl) > 0 {
				firsts = append(firsts, fl[0])
			} else {
				firsts = append(firsts, "")
			}
		}
		want, err := sum(f)
		if err != nil {
			return err
		}
		if strings.TrimRight(strings.Join(firsts, "\n"), "\n") != want {
			bad("%s.sha256 is not the checksum of %s", name, name)
		}
	}
	if failed {
		return ErrFailed
	}
	fmt.Fprintf(o.Out, "%d assets, each checksummed, each named for @%s\n", len(files), o.Version)
	return nil
}

// ── publish ──────────────────────────────────────────────────────────────

// PublishOptions are the "Publish the release" step's environment.
type PublishOptions struct {
	Common
	Repo      string
	Tag       string
	Version   string
	Manifest  string
	NotesFile string
	Work      string
}

// Publish creates the release, or checks and completes the one a failed run
// left: assets already there must hold the same bytes, and nothing else may be
// there. Safe to re-run.
func Publish(ctx context.Context, o PublishOptions) error {
	o.init()
	mb, err := os.ReadFile(o.Manifest)
	if err != nil {
		return err
	}
	files := lines(string(mb))
	local := map[string]string{}
	for _, f := range files {
		local[base(f)] = f
	}
	gh := func(stdout io.Writer, args ...string) error {
		return o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: "gh", Args: args, Stdout: stdout, Stderr: o.Err})
	}
	names := func() ([]string, error) {
		var b bytes.Buffer
		if err := gh(&b, "release", "view", o.Tag, "--repo", o.Repo, "--json", "assets", "--jq", ".assets[].name"); err != nil {
			return nil, fmt.Errorf("gh release view: %w", err)
		}
		return lines(b.String()), nil
	}

	var remote []string
	if gh(io.Discard, "release", "view", o.Tag, "--repo", o.Repo) == nil {
		fmt.Fprintf(o.Out, "release %s exists — checking what it holds\n", o.Tag)
		if remote, err = names(); err != nil {
			return err
		}
		if err := os.RemoveAll(o.Work); err != nil {
			return err
		}
		if err := os.MkdirAll(o.Work, 0o755); err != nil {
			return err
		}
		for _, name := range remote {
			lp, ok := local[name]
			if !ok {
				return o.fail("release %s holds '%s', which this build does not produce — refusing; delete the release to start over, or cut the next version", o.Tag, name)
			}
			if err := gh(o.Out, "release", "download", o.Tag, "--repo", o.Repo, "--pattern", name, "--dir", o.Work); err != nil {
				return fmt.Errorf("gh release download: %w", err)
			}
			a, err := sum(filepath.Join(o.Work, name))
			if err != nil {
				return err
			}
			b, err := sum(lp)
			if err != nil {
				return err
			}
			if a != b {
				return o.fail("release %s holds '%s' with different bytes than this build — refusing to replace a published asset; delete the release to start over, or cut the next version", o.Tag, name)
			}
		}
	} else {
		args := []string{"release", "create", o.Tag, "--repo", o.Repo, "--verify-tag", "--title", o.Tag, "--notes-file", o.NotesFile, "--generate-notes"}
		if strings.Contains(o.Version, "-") {
			args = append(args, "--prerelease")
		}
		if err := gh(o.Out, args...); err != nil {
			return fmt.Errorf("gh release create: %w", err)
		}
	}

	have := map[string]bool{}
	for _, n := range remote {
		have[n] = true
	}
	var missing []string
	for _, f := range files {
		if !have[base(f)] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(o.Out, "uploading %d of %d assets\n", len(missing), len(files))
		if err := gh(o.Out, append([]string{"release", "upload", o.Tag, "--repo", o.Repo}, missing...)...); err != nil {
			return fmt.Errorf("gh release upload: %w", err)
		}
	} else {
		fmt.Fprintf(o.Out, "all %d assets are already there\n", len(files))
	}

	// The release must now hold exactly the build's assets.
	got, err := names()
	if err != nil {
		return err
	}
	sort.Strings(got)
	var want []string
	for n := range local {
		want = append(want, n)
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return o.fail("release %s does not hold exactly the build's assets after upload", o.Tag)
	}
	return nil
}

// ── smoke ────────────────────────────────────────────────────────────────

// DefaultSleeps is the backoff between attempts: 8 attempts, about five
// minutes in all, for a CDN that has not caught up with a release just
// published.
var DefaultSleeps = []time.Duration{10 * time.Second, 15 * time.Second, 20 * time.Second, 30 * time.Second, 45 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second}

// SmokeOptions are the "Smoke test the published packages" step's environment.
type SmokeOptions struct {
	Common
	PklCommand  string // PKL_CMD, e.g. `devbox run -- pkl`
	Repo        string
	Tag         string
	Version     string
	Manifest    string
	SmokeImport string
	SmokeDir    string
	Attempts    int
	Sleeps      []time.Duration
	Sleep       func(time.Duration)
}

// Smoke resolves the published packages as a consumer would, and optionally
// evaluates a module importing some of them, retrying while the CDN catches up.
func Smoke(ctx context.Context, o SmokeOptions) error {
	o.init()
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Attempts == 0 {
		o.Attempts = 8
	}
	pkl := strings.Fields(o.PklCommand)
	if len(pkl) == 0 {
		return errors.New("PKL_CMD is empty")
	}
	baseURI := fmt.Sprintf("package://github.com/%s/releases/download/%s", o.Repo, o.Tag)
	if err := os.RemoveAll(o.SmokeDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(o.SmokeDir, "project"), 0o755); err != nil {
		return err
	}
	mb, err := os.ReadFile(o.Manifest)
	if err != nil {
		return err
	}
	uri := map[string]string{}
	var project strings.Builder
	project.WriteString("amends \"pkl:Project\"\n\ndependencies {\n")
	i := 0
	for _, f := range lines(string(mb)) {
		name := base(f)
		if !strings.HasSuffix(name, "@"+o.Version) {
			continue
		}
		uri[name[:strings.LastIndex(name, "@")]] = baseURI + "/" + name
		fmt.Fprintf(&project, "  [\"p%d\"] { uri = \"%s/%s\" }\n", i, baseURI, name)
		i++
	}
	project.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(o.SmokeDir, "project", "PklProject"), []byte(project.String()), 0o644); err != nil {
		return err
	}
	if i == 0 {
		return o.fail("no package metadata (<name>@%s) among the assets to resolve", o.Version)
	}

	retry := func(args ...string) bool {
		for attempt := 0; ; {
			if o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: pkl[0], Args: append(append([]string{}, pkl[1:]...), args...), Stdout: o.Out, Stderr: o.Err}) == nil {
				return true
			}
			attempt++
			if attempt >= o.Attempts {
				return false
			}
			if attempt <= len(o.Sleeps) {
				o.Sleep(o.Sleeps[attempt-1])
			}
		}
	}
	cache := []string{"--cache-dir", filepath.Join(o.SmokeDir, "cache")}
	if !retry(append(append([]string{"project", "resolve"}, cache...), filepath.Join(o.SmokeDir, "project"))...) {
		return o.fail("the %d published packages do not resolve from github.com — the release is up but the CDN may not have fully propagated; re-running the failed job is safe", i)
	}
	fmt.Fprintf(o.Out, "resolved %d packages from %s\n", i, baseURI)

	if o.SmokeImport != "" {
		// Validated first, written after: a refusal inside a redirected group
		// would put its message in the module instead of the log.
		n := 0
		var imports strings.Builder
		for _, entry := range strings.Split(o.SmokeImport, "\n") {
			if entry == "" {
				continue
			}
			name, _, _ := strings.Cut(entry, "#")
			if !strings.Contains(entry, "#/") {
				return o.fail("smoke-import entry '%s' is not <package name>#/<module path>", entry)
			}
			u, ok := uri[name]
			if !ok {
				return o.fail("smoke-import entry '%s' names no package of this release", entry)
			}
			_, path, _ := strings.Cut(entry, "#/")
			fmt.Fprintf(&imports, "local m%d = import(\"%s#/%s\")\n", n, u, path)
			n++
		}
		if n == 0 {
			return o.fail("smoke-import holds no entries")
		}
		var ms []string
		for k := 0; k < n; k++ {
			ms = append(ms, "m"+strconv.Itoa(k))
		}
		module := filepath.Join(o.SmokeDir, "smoke.pkl")
		if err := os.WriteFile(module, []byte(fmt.Sprintf("%simported = List(%s).length\n", imports.String(), strings.Join(ms, ","))), 0o644); err != nil {
			return err
		}
		if !retry(append(append([]string{"eval"}, cache...), module)...) {
			return o.fail("the smoke module does not evaluate against the published packages — the release is up but the CDN may not have fully propagated; re-running the failed job is safe")
		}
		fmt.Fprintln(o.Out, "evaluated the smoke module")
	}
	return nil
}
