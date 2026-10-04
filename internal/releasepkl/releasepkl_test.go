package releasepkl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/truvity/ci-actions/internal/runcmd"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(path string) string { b, _ := os.ReadFile(path); return string(b) }

// ── checks ───────────────────────────────────────────────────────────────

const changelog = `# Changelog

## Unreleased

- Not yet.

## v0.2.0 — 2026-10-01

- Second.

## v0.1.0

- First.
- Another.

## v0.0.9

- Older.
`

func runChecks(t *testing.T, refType, tag, declared, cl string) (out, version, notes string, err error) {
	t.Helper()
	dir := t.TempDir()
	if cl == "" {
		cl = filepath.Join(dir, "CHANGELOG.md")
		write(t, cl, changelog)
	}
	var b bytes.Buffer
	o := ChecksOptions{Common: Common{Out: &b, Output: filepath.Join(dir, "out")}, RefType: refType, Tag: tag, Declared: declared, Changelog: cl, NotesFile: filepath.Join(dir, "notes.md")}
	err = Checks(context.Background(), o)
	return b.String(), read(o.Output), read(o.NotesFile), err
}

func TestChecks(t *testing.T) {
	for _, tc := range []struct{ name, tag, declared, version string }{
		{"tag equals declared", "v0.1.0", "0.1.0", "0.1.0"},
		{"heading with a date suffix", "v0.2.0", "0.2.0", "0.2.0"},
		{"nothing declared: the tag is the version", "v0.1.0", "", "0.1.0"},
	} {
		out, version, _, err := runChecks(t, "tag", tc.tag, tc.declared, "")
		if err != nil || version != "version="+tc.version+"\n" || out != fmt.Sprintf("%s is %s, with a changelog heading\n", tc.tag, tc.version) {
			t.Errorf("%s: err %v out %q version %q", tc.name, err, out, version)
		}
	}
	for _, tc := range []struct{ name, refType, tag, declared, want string }{
		{"tag differs from declared", "tag", "v0.1.0", "0.2.0", "::error::tag 'v0.1.0' is not v plus the declared version '0.2.0' — nothing released\n"},
		{"declared differs from tag", "tag", "v0.2.0", "0.1.0", "::error::tag 'v0.2.0' is not v plus the declared version '0.1.0' — nothing released\n"},
		{"tag without v", "tag", "0.1.0", "0.1.0", "::error::tag '0.1.0' does not start with v\n"},
		{"branch push", "branch", "v0.1.0", "0.1.0", "::error::release-pkl runs on a tag push, not on a branch\n"},
		{"declared is not a version", "tag", "v1", "1", "::error::'1' is not a version (X.Y.Z, optionally -prerelease)\n"},
		{"declared with a v", "tag", "vv0.1.0", "v0.1.0", "::error::'v0.1.0' is not a version (X.Y.Z, optionally -prerelease)\n"},
		{"nothing declared, tag not a version", "tag", "vnext", "", "::error::'next' is not a version (X.Y.Z, optionally -prerelease)\n"},
		{"no heading for the version", "tag", "v0.3.0", "0.3.0", "no '## v0.3.0' heading — nothing released\n"},
		{"a prefix of a heading is not a heading", "tag", "v0.0.1", "0.0.1", "no '## v0.0.1' heading — nothing released\n"},
	} {
		out, version, _, err := runChecks(t, tc.refType, tc.tag, tc.declared, "")
		if !errors.Is(err, ErrFailed) || !strings.HasSuffix(out, tc.want) || version != "" {
			t.Errorf("%s: err %v out %q want %q, output %q", tc.name, err, out, tc.want, version)
		}
	}
	missing := filepath.Join(t.TempDir(), "none.md")
	if out, _, _, err := runChecks(t, "tag", "v0.1.0", "0.1.0", missing); !errors.Is(err, ErrFailed) || out != "::error::"+missing+" not found\n" {
		t.Errorf("missing changelog: err %v out %q", err, out)
	}
	if _, _, notes, _ := runChecks(t, "tag", "v0.1.0", "0.1.0", ""); notes != "\n- First.\n- Another.\n\n" {
		t.Errorf("notes are %q: the heading's section only", notes)
	}
	pre := filepath.Join(t.TempDir(), "pre.md")
	write(t, pre, "## Unreleased\n\n## v1.0.0-rc.1\n\n- rc.\n")
	if _, v, _, err := runChecks(t, "tag", "v1.0.0-rc.1", "1.0.0-rc.1", pre); err != nil || v != "version=1.0.0-rc.1\n" {
		t.Errorf("prerelease: err %v %q", err, v)
	}
	// A sub-heading is part of the section; only a `## ` heading ends it.
	sub := filepath.Join(t.TempDir(), "sub.md")
	write(t, sub, "## v1.0.0\n\n### Added\n\n- x\n\n## v0.9.0\n\n- y\n")
	if _, _, notes, err := runChecks(t, "tag", "v1.0.0", "", sub); err != nil || notes != "\n### Added\n\n- x\n\n" {
		t.Errorf("sub-heading: err %v notes %q", err, notes)
	}
}

func TestDeclared(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, stdout, want string
		fail                    bool
		wantOut                 string
	}{
		{"no command declares nothing", "", "", "version=\n", false, ""},
		{"the last line, whitespace removed", "echo", "Info: x\n 1.2.3 \r\n", "version=1.2.3\n", false, ""},
		{"no trailing newline", "echo", "1.2.3", "version=1.2.3\n", false, ""},
		{"a blank last line is nothing", "echo", "1.2.3\n\n", "", true, "::error::version-command printed nothing\n"},
		{"no output is nothing", "echo", "", "", true, "::error::version-command printed nothing\n"},
	} {
		dir := t.TempDir()
		var b bytes.Buffer
		var got []string
		o := DeclaredOptions{Common: Common{Out: &b, Output: filepath.Join(dir, "out"), Exec: func(_ context.Context, c runcmd.Cmd) error {
			got = c.Args
			c.Stdout.Write([]byte(tc.stdout))
			return nil
		}}, VersionCommand: tc.cmd}
		err := Declared(context.Background(), o)
		if tc.fail != errors.Is(err, ErrFailed) || read(o.Output) != tc.want || b.String() != tc.wantOut {
			t.Errorf("%s: err %v output %q log %q", tc.name, err, read(o.Output), b.String())
		}
		if tc.cmd != "" && strings.Join(got, " ") != "run -- bash -c "+tc.cmd {
			t.Errorf("%s: ran %v", tc.name, got)
		}
	}
	err := Declared(context.Background(), DeclaredOptions{Common: Common{Exec: func(context.Context, runcmd.Cmd) error { return errors.New("boom") }}, VersionCommand: "x"})
	if err == nil || errors.Is(err, ErrFailed) {
		t.Errorf("a failing version command fails the step: %v", err)
	}
}

// ── assets ───────────────────────────────────────────────────────────────

// mkOut writes two packages at 0.1.0 in the layout `pkl project package` writes.
func mkOut(t *testing.T, d string) {
	t.Helper()
	os.RemoveAll(d)
	for _, p := range []string{"a.one", "a.two"} {
		for _, f := range []string{p + "@0.1.0", p + "@0.1.0.zip"} {
			path := filepath.Join(d, p+"@0.1.0", f)
			body := "meta " + p
			if strings.HasSuffix(f, ".zip") {
				body = "zip " + p
			}
			write(t, path, body)
			s, _ := sum(path)
			write(t, path+".sha256", s)
		}
	}
}

func runAssets(t *testing.T, dir, version string) (string, string, error) {
	t.Helper()
	m := filepath.Join(t.TempDir(), "manifest")
	var b bytes.Buffer
	if version == "" {
		version = "0.1.0"
	}
	err := Assets(context.Background(), AssetsOptions{Common: Common{Out: &b}, OutputDir: dir, Version: version, Manifest: m})
	return b.String(), read(m), err
}

func TestAssets(t *testing.T) {
	o := filepath.Join(t.TempDir(), "o")
	mkOut(t, o)
	out, manifest, err := runAssets(t, o, "")
	if err != nil || out != "8 assets, each checksummed, each named for @0.1.0\n" || len(strings.Split(strings.TrimSpace(manifest), "\n")) != 8 {
		t.Errorf("ok: err %v out %q", err, out)
	}
	if lines := strings.Split(strings.TrimSpace(manifest), "\n"); !sort.StringsAreSorted(lines) {
		t.Errorf("manifest is sorted: %v", lines)
	}
	for _, tc := range []struct {
		name    string
		mutate  func()
		version string
		want    string
	}{
		{"asset without a checksum", func() { os.Remove(filepath.Join(o, "a.two@0.1.0/a.two@0.1.0.zip.sha256")) }, "", "::error::a.two@0.1.0.zip has no a.two@0.1.0.zip.sha256\n"},
		{"checksum without an asset", func() { os.Remove(filepath.Join(o, "a.one@0.1.0/a.one@0.1.0.zip")) }, "", "::error::a.one@0.1.0.zip.sha256 has no asset beside it to be the checksum of\n"},
		{"checksum of other bytes", func() { write(t, filepath.Join(o, "a.one@0.1.0/a.one@0.1.0.sha256"), "beef") }, "", "::error::a.one@0.1.0.sha256 is not the checksum of a.one@0.1.0\n"},
		{"names carry another version", func() {}, "0.2.0", "::error::a.one@0.1.0 does not carry @0.2.0\n"},
		{"a file that carries no version", func() { write(t, filepath.Join(o, "a.one@0.1.0/README"), "") }, "", "::error::README does not carry @0.1.0\n::error::README has no README.sha256\n"},
		{"one name twice", func() {
			write(t, filepath.Join(o, "again/a.one@0.1.0"), "meta a.one")
		}, "", "::error::two files would be uploaded under one name: a.one@0.1.0 \n"},
	} {
		mkOut(t, o)
		tc.mutate()
		out, _, err := runAssets(t, o, tc.version)
		if !errors.Is(err, ErrFailed) || !strings.Contains(out, tc.want) {
			t.Errorf("%s: err %v out %q want it to contain %q", tc.name, err, out, tc.want)
		}
	}
	// Every fault is reported, not the first.
	mkOut(t, o)
	os.Remove(filepath.Join(o, "a.two@0.1.0/a.two@0.1.0.zip.sha256"))
	write(t, filepath.Join(o, "a.one@0.1.0/a.one@0.1.0.sha256"), "beef")
	if out, _, _ := runAssets(t, o, ""); strings.Count(out, "::error::") != 2 {
		t.Errorf("both faults are reported: %q", out)
	}
	empty := t.TempDir()
	if out, _, err := runAssets(t, empty, ""); !errors.Is(err, ErrFailed) || out != "::error::"+empty+" holds no files\n" {
		t.Errorf("empty: %v %q", err, out)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if out, _, err := runAssets(t, missing, ""); !errors.Is(err, ErrFailed) || out != "::error::"+missing+" does not exist — did the recipe write elsewhere?\n" {
		t.Errorf("missing: %v %q", err, out)
	}
	// A flat directory is fine too.
	mkOut(t, o)
	flat := filepath.Join(o, "flat")
	os.MkdirAll(flat, 0o755)
	for _, p := range []string{"a.one", "a.two"} {
		ents, _ := os.ReadDir(filepath.Join(o, p+"@0.1.0"))
		for _, e := range ents {
			os.Rename(filepath.Join(o, p+"@0.1.0", e.Name()), filepath.Join(flat, e.Name()))
		}
		os.Remove(filepath.Join(o, p+"@0.1.0"))
	}
	if _, _, err := runAssets(t, o, ""); err != nil {
		t.Errorf("flat: %v", err)
	}
}

// ── publish ──────────────────────────────────────────────────────────────

// fakeGH is a release store: `exists`, the assets it holds (name to bytes),
// and a log of the calls.
type fakeGH struct {
	exists bool
	assets map[string]string
	calls  []string
	t      *testing.T
}

func (g *fakeGH) exec(_ context.Context, c runcmd.Cmd) error {
	if c.Name != "gh" || c.Args[0] != "release" {
		g.t.Fatalf("unexpected command %s %v", c.Name, c.Args)
	}
	cmd, args := c.Args[1], c.Args[2:]
	g.calls = append(g.calls, cmd+" "+strings.Join(args, " "))
	switch cmd {
	case "view":
		if !g.exists {
			return errors.New("release not found")
		}
		if strings.Contains(strings.Join(args, " "), "--json") {
			var n []string
			for k := range g.assets {
				n = append(n, k)
			}
			sort.Strings(n)
			for _, k := range n {
				fmt.Fprintln(c.Stdout, k)
			}
		}
	case "create":
		g.exists = true
		g.assets = map[string]string{}
	case "upload":
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "/") {
				g.assets[filepath.Base(a)] = read(a)
			}
		}
	case "download":
		var pat, dir string
		for i, a := range args {
			if a == "--pattern" {
				pat = args[i+1]
			}
			if a == "--dir" {
				dir = args[i+1]
			}
		}
		write(g.t, filepath.Join(dir, pat), g.assets[pat])
	default:
		g.t.Fatalf("unexpected release %s", cmd)
	}
	return nil
}

func (g *fakeGH) count(prefix string) int {
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type pub struct {
	t        *testing.T
	gh       *fakeGH
	manifest string
	notes    string
	work     string
	files    []string
}

func newPub(t *testing.T) *pub {
	d := t.TempDir()
	o := filepath.Join(d, "o")
	mkOut(t, o)
	m := filepath.Join(d, "manifest")
	if _, _, err := func() (string, string, error) {
		var b bytes.Buffer
		err := Assets(context.Background(), AssetsOptions{Common: Common{Out: &b}, OutputDir: o, Version: "0.1.0", Manifest: m})
		return "", "", err
	}(); err != nil {
		t.Fatal(err)
	}
	p := &pub{t: t, manifest: m, notes: filepath.Join(d, "notes.md"), work: filepath.Join(d, "remote"), gh: &fakeGH{assets: map[string]string{}, t: t}}
	write(t, p.notes, "- notes\n")
	p.files = strings.Split(strings.TrimSpace(read(m)), "\n")
	return p
}

func (p *pub) run(version string) (string, error) {
	var b bytes.Buffer
	if version == "" {
		version = "0.1.0"
	}
	err := Publish(context.Background(), PublishOptions{Common: Common{Out: &b, Exec: p.gh.exec}, Repo: "o/r", Tag: "v0.1.0", Version: version, Manifest: p.manifest, NotesFile: p.notes, Work: p.work})
	return b.String(), err
}

func TestPublishFresh(t *testing.T) {
	p := newPub(t)
	out, err := p.run("")
	if err != nil || p.gh.count("create") != 1 || len(p.gh.assets) != 8 || !strings.Contains(out, "uploading 8 of 8 assets\n") {
		t.Fatalf("err %v create %d held %d out %q", err, p.gh.count("create"), len(p.gh.assets), out)
	}
	create := ""
	for _, c := range p.gh.calls {
		if strings.HasPrefix(c, "create") {
			create = c
		}
	}
	if create != "create v0.1.0 --repo o/r --verify-tag --title v0.1.0 --notes-file "+p.notes+" --generate-notes" {
		t.Errorf("create: %s", create)
	}
	p = newPub(t)
	if _, err := p.run("1.0.0-rc.1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range p.gh.calls {
		if strings.HasPrefix(c, "create") && !strings.HasSuffix(c, "--generate-notes --prerelease") {
			t.Errorf("a prerelease version is marked: %s", c)
		}
	}
}

func TestPublishResumes(t *testing.T) {
	p := newPub(t)
	p.gh.exists = true
	for _, f := range p.files[:3] {
		p.gh.assets[filepath.Base(f)] = read(f)
	}
	out, err := p.run("")
	if err != nil || p.gh.count("create") != 0 || len(p.gh.assets) != 8 || !strings.Contains(out, "release v0.1.0 exists — checking what it holds\n") || !strings.Contains(out, "uploading 5 of 8 assets\n") {
		t.Fatalf("err %v create %d held %d out %q", err, p.gh.count("create"), len(p.gh.assets), out)
	}
	for _, c := range p.gh.calls {
		if strings.HasPrefix(c, "upload") && strings.Count(c, " /") != 5 {
			t.Errorf("only the five missing are uploaded: %s", c)
		}
	}
	out, err = p.run("")
	if err != nil || p.gh.count("upload") != 1 || !strings.Contains(out, "all 8 assets are already there\n") {
		t.Errorf("a complete release uploads nothing: err %v uploads %d out %q", err, p.gh.count("upload"), out)
	}
}

func TestPublishRefusals(t *testing.T) {
	p := newPub(t)
	p.gh.exists = true
	p.gh.assets[filepath.Base(p.files[0])] = "other"
	out, err := p.run("")
	if !errors.Is(err, ErrFailed) || !strings.Contains(out, "::error::release v0.1.0 holds '"+filepath.Base(p.files[0])+"' with different bytes than this build — refusing to replace a published asset; delete the release to start over, or cut the next version\n") || p.gh.count("upload") != 0 {
		t.Errorf("different bytes: err %v out %q uploads %d", err, out, p.gh.count("upload"))
	}
	p = newPub(t)
	p.gh.exists = true
	p.gh.assets["stray@0.1.0"] = "x"
	out, err = p.run("")
	if !errors.Is(err, ErrFailed) || !strings.Contains(out, "::error::release v0.1.0 holds 'stray@0.1.0', which this build does not produce — refusing; delete the release to start over, or cut the next version\n") || p.gh.count("upload") != 0 {
		t.Errorf("stray: err %v out %q", err, out)
	}
}

// ── smoke ────────────────────────────────────────────────────────────────

type fakePkl struct {
	calls []string
	fail  string // "project resolve", "eval"
	flaky int    // the first N resolve attempts fail
	slept []time.Duration
}

func (p *fakePkl) exec(_ context.Context, c runcmd.Cmd) error {
	line := strings.Join(c.Args, " ")
	p.calls = append(p.calls, line)
	if p.fail != "" && strings.HasPrefix(line, p.fail) {
		return errors.New("fail")
	}
	if strings.HasPrefix(line, "project resolve") && len(p.calls) <= p.flaky {
		return errors.New("flaky")
	}
	return nil
}

func smoke(t *testing.T, p *fakePkl, imp string, sleeps []time.Duration) (string, string, error) {
	t.Helper()
	pb := newPub(t)
	dir := filepath.Join(t.TempDir(), "smoke")
	var b bytes.Buffer
	err := Smoke(context.Background(), SmokeOptions{Common: Common{Out: &b, Exec: p.exec}, PklCommand: "pkl", Repo: "o/r", Tag: "v0.1.0", Version: "0.1.0",
		Manifest: pb.manifest, SmokeImport: imp, SmokeDir: dir, Attempts: 3, Sleeps: sleeps, Sleep: func(d time.Duration) { p.slept = append(p.slept, d) }})
	return b.String(), dir, err
}

func TestSmoke(t *testing.T) {
	const base = "package://github.com/o/r/releases/download/v0.1.0"
	p := &fakePkl{}
	out, dir, err := smoke(t, p, "", nil)
	if err != nil || len(p.calls) != 1 || !strings.HasPrefix(p.calls[0], "project resolve --cache-dir ") || !strings.HasSuffix(p.calls[0], "/project") || out != "resolved 2 packages from "+base+"\n" {
		t.Fatalf("resolve only: err %v calls %v out %q", err, p.calls, out)
	}
	want := "amends \"pkl:Project\"\n\ndependencies {\n  [\"p0\"] { uri = \"" + base + "/a.one@0.1.0\" }\n  [\"p1\"] { uri = \"" + base + "/a.two@0.1.0\" }\n}\n"
	if got := read(filepath.Join(dir, "project", "PklProject")); got != want {
		t.Errorf("PklProject:\n%s\nwant:\n%s", got, want)
	}

	p = &fakePkl{}
	out, dir, err = smoke(t, p, "a.one#/One.pkl\na.two#/sub/Two.pkl\n\n", nil)
	wantMod := "local m0 = import(\"" + base + "/a.one@0.1.0#/One.pkl\")\nlocal m1 = import(\"" + base + "/a.two@0.1.0#/sub/Two.pkl\")\nimported = List(m0,m1).length\n"
	if err != nil || read(filepath.Join(dir, "smoke.pkl")) != wantMod || len(p.calls) != 2 || !strings.HasPrefix(p.calls[1], "eval ") || !strings.HasSuffix(out, "evaluated the smoke module\n") {
		t.Errorf("imports: err %v calls %v\n%s", err, p.calls, read(filepath.Join(dir, "smoke.pkl")))
	}

	for _, tc := range []struct{ imp, want string }{
		{"a.nine#/X.pkl", "::error::smoke-import entry 'a.nine#/X.pkl' names no package of this release\n"},
		{"a.one", "::error::smoke-import entry 'a.one' is not <package name>#/<module path>\n"},
		{"\n\n", "::error::smoke-import holds no entries\n"},
	} {
		p = &fakePkl{}
		out, dir, err = smoke(t, p, tc.imp, nil)
		if tc.imp == "\n\n" {
			// An import of only blank lines is a non-empty value with no entries.
		}
		if !errors.Is(err, ErrFailed) || !strings.HasSuffix(out, tc.want) || len(p.calls) != 1 {
			t.Errorf("%q: err %v out %q", tc.imp, err, out)
		}
		if _, e := os.Stat(filepath.Join(dir, "smoke.pkl")); e == nil {
			t.Errorf("%q: a refused import must not write the module", tc.imp)
		}
	}

	p = &fakePkl{fail: "project resolve"}
	out, _, err = smoke(t, p, "", nil)
	if !errors.Is(err, ErrFailed) || !strings.Contains(out, "::error::the 2 published packages do not resolve from github.com — the release is up but the CDN may not have fully propagated; re-running the failed job is safe\n") {
		t.Errorf("failed resolve: %v %q", err, out)
	}
	p = &fakePkl{fail: "eval"}
	out, _, err = smoke(t, p, "a.one#/One.pkl", nil)
	if !errors.Is(err, ErrFailed) || !strings.Contains(out, "::error::the smoke module does not evaluate against the published packages") {
		t.Errorf("failed eval: %v %q", err, out)
	}

	// 3 attempts: a resolve that fails twice passes, forever failing is bounded,
	// and the backoff is the configured one.
	p = &fakePkl{flaky: 2}
	if _, _, err = smoke(t, p, "", []time.Duration{time.Second, 2 * time.Second}); err != nil || len(p.calls) != 3 || len(p.slept) != 2 || p.slept[0] != time.Second || p.slept[1] != 2*time.Second {
		t.Errorf("retry: err %v calls %d slept %v", err, len(p.calls), p.slept)
	}
	p = &fakePkl{flaky: 9}
	if _, _, err = smoke(t, p, "", []time.Duration{time.Second}); !errors.Is(err, ErrFailed) || len(p.calls) != 3 || len(p.slept) != 1 {
		t.Errorf("bounded: err %v calls %d slept %v", err, len(p.calls), p.slept)
	}
}

func TestSmokeNeedsMetadata(t *testing.T) {
	pb := newPub(t)
	// A manifest of only zips and checksums has no <name>@<version> file.
	var keep []string
	for _, f := range pb.files {
		if strings.HasSuffix(f, ".zip") {
			keep = append(keep, f)
		}
	}
	write(t, pb.manifest, strings.Join(keep, "\n")+"\n")
	var b bytes.Buffer
	err := Smoke(context.Background(), SmokeOptions{Common: Common{Out: &b, Exec: (&fakePkl{}).exec}, PklCommand: "pkl", Repo: "o/r", Tag: "v0.1.0", Version: "0.1.0", Manifest: pb.manifest, SmokeDir: filepath.Join(t.TempDir(), "s")})
	if !errors.Is(err, ErrFailed) || b.String() != "::error::no package metadata (<name>@0.1.0) among the assets to resolve\n" {
		t.Errorf("%v %q", err, b.String())
	}
}

// The assets the check lists are the ones the publish uploads, byte for byte,
// as the shell did with `sha256sum`.
func TestSumMatchesSha256sum(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	write(t, f, "hello\n")
	want, err := exec.Command("sha256sum", f).Output()
	if err != nil {
		t.Skip("no sha256sum")
	}
	if got, _ := sum(f); got != strings.Fields(string(want))[0] {
		t.Errorf("sum %s, sha256sum %s", got, want)
	}
}
