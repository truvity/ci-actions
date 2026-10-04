package pklfleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/truvity/ci-actions/internal/runcmd"
)

const U = "package://github.com/o/lib/releases/download"

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(p string) string { b, _ := os.ReadFile(p); return string(b) }

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// depsJSON is what a REMOTE resolve writes: the major-only key, the
// projectpackage value, a checksum.
func depsJSON(v string, names ...string) string {
	var b strings.Builder
	b.WriteString("{\n  \"schemaVersion\": 1,\n  \"resolvedDependencies\": {\n")
	for i, n := range names {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    \"%s/v%s/contracts.%s@%s\": {\n      \"type\": \"remote\",\n      \"uri\": \"projectpackage://github.com/o/lib/releases/download/v%s/contracts.%s@%s\",\n      \"checksums\": {\n        \"sha256\": \"abc%s\"\n      }\n    }",
			U, v, n, strings.SplitN(v, ".", 2)[0], v, n, v, n)
	}
	b.WriteString("\n  }\n}\n")
	return b.String()
}

// consumer is a checkout: a root project and a nested one.
type consumer struct {
	t   *testing.T
	dir string
	tmp string
}

func mkRepo(t *testing.T, root, nested string) *consumer {
	t.Helper()
	d := t.TempDir()
	c := &consumer{t: t, dir: filepath.Join(d, "consumer"), tmp: d}
	write(t, c.dir+"/PklProject", fmt.Sprintf(`// consumer project
amends "pkl:Project"
dependencies {
  ["vocab"] { uri = "%[1]s/v%[2]s/contracts.vocab@%[2]s" }
  ["model"] { uri = "%[1]s/v%[2]s/contracts.model@%[2]s" }
  ["other"] { uri = "package://github.com/someone/else/releases/download/v1.0.0/else.pkg@1.0.0" }
}
`, U, root))
	write(t, c.dir+"/PklProject.deps.json", depsJSON(root, "vocab", "model"))
	write(t, c.dir+"/svc/PklProject", fmt.Sprintf("amends \"pkl:Project\"\ndependencies {\n  [\"vocab\"] { uri = \"%[1]s/v%[2]s/contracts.vocab@%[2]s\" }\n}\n// import \"%[1]s/v%[2]s/contracts.vocab@%[2]s#/Vocab.pkl\"\n", U, nested))
	write(t, c.dir+"/svc/PklProject.deps.json", depsJSON(nested, "vocab"))
	write(t, c.dir+"/README.md", "plain\n")
	git(t, c.dir, "init", "-q", ".")
	git(t, c.dir, "add", "-A")
	git(t, c.dir, "commit", "-q", "-m", "init")
	return c
}

func (c *consumer) f(p string) string { return read(filepath.Join(c.dir, p)) }
func (c *consumer) edit(p, from, to string) {
	b := c.f(p)
	if !strings.Contains(b, from) {
		c.t.Fatalf("%s has no %q", p, from)
	}
	write(c.t, filepath.Join(c.dir, p), strings.Replace(b, from, to, 1))
}

func (c *consumer) snapshot() [32]byte {
	var all string
	for _, p := range []string{"PklProject", "PklProject.deps.json", "svc/PklProject", "svc/PklProject.deps.json", "README.md"} {
		all += c.f(p)
	}
	return sha256.Sum256([]byte(all))
}

func (c *consumer) rewrite(target string) (log string, outputs map[string]string, dirs string, err error) {
	var b bytes.Buffer
	out := filepath.Join(c.tmp, "gh_out")
	os.Remove(out)
	df := filepath.Join(c.tmp, "dirs")
	err = Rewrite(context.Background(), RewriteOptions{Common: Common{Out: &b, Dir: c.dir, Output: out}, Target: target, Source: "o/lib", DirsFile: df})
	outputs = map[string]string{}
	for _, l := range strings.Split(read(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			outputs[k] = v
		}
	}
	return b.String(), outputs, read(df), err
}

func TestRewrite(t *testing.T) {
	c := mkRepo(t, "0.2.0", "0.2.0")
	log, out, dirs, err := c.rewrite("0.2.1")
	if err != nil {
		t.Fatal(err, log)
	}
	for _, p := range []string{"PklProject", "PklProject.deps.json", "svc/PklProject", "svc/PklProject.deps.json"} {
		if strings.Contains(c.f(p), "0.2.0") {
			t.Errorf("%s still names 0.2.0:\n%s", p, c.f(p))
		}
	}
	for _, want := range []struct{ file, text string }{
		{"PklProject", `["vocab"] { uri = "` + U + `/v0.2.1/contracts.vocab@0.2.1" }`},
		{"PklProject.deps.json", `"` + U + `/v0.2.1/contracts.vocab@0": {`}, // the major-only key keeps its @0
		{"PklProject.deps.json", `"uri": "projectpackage://github.com/o/lib/releases/download/v0.2.1/contracts.vocab@0.2.1"`},
		{"PklProject.deps.json", `"sha256": "abcvocab"`}, // checksums are left for resolve
		{"svc/PklProject", `import "` + U + `/v0.2.1/contracts.vocab@0.2.1#/Vocab.pkl"`},
		{"PklProject", `someone/else/releases/download/v1.0.0/else.pkg@1.0.0`}, // another library is untouched
		{"README.md", "plain\n"},
	} {
		if !strings.Contains(c.f(want.file), want.text) {
			t.Errorf("%s lacks %q:\n%s", want.file, want.text, c.f(want.file))
		}
	}
	if dirs != ".\nsvc\n" || out["changed"] != "true" || out["breaking"] != "false" || out["from"] != "0.2.0" {
		t.Errorf("dirs %q outputs %v", dirs, out)
	}
	if log != "moved 2 project(s) from v0.2.0 to v0.2.1 (breaking: false)\n" {
		t.Errorf("log %q", log)
	}

	// Already current: byte-identical, changed=false, no directories.
	before := c.snapshot()
	log, out, dirs, err = c.rewrite("0.2.1")
	if err != nil || c.snapshot() != before || out["changed"] != "false" || dirs != "" || log != "already at v0.2.1 — nothing to rewrite\n" {
		t.Errorf("no-op: err %v out %v dirs %q log %q", err, out, dirs, log)
	}
}

func TestRewriteClassification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		root, nested string
		target       string
		dirs, from   string
		breaking     string
		keys         string // a deps.json key that must be present
	}{
		{"only the stale project is rewritten", "0.2.0", "0.2.1", "0.2.1", ".\n", "0.2.0", "false", ""},
		{"projects on two versions, both moved", "0.2.0", "0.2.1", "0.2.2", ".\nsvc\n", "0.2.0,0.2.1", "false", ""},
		{"a 0.x minor is breaking", "0.2.0", "0.2.0", "0.3.0", ".\nsvc\n", "0.2.0", "true", `contracts.vocab@0": {`},
		{"a new major is breaking, the @ keys move", "0.2.0", "0.2.0", "1.0.0", ".\nsvc\n", "0.2.0", "true", `contracts.vocab@1": {`},
		{"a 1.x patch is not breaking", "1.0.0", "1.0.0", "1.0.1", ".\nsvc\n", "1.0.0", "false", `contracts.vocab@1": {`},
		{"a 1.x minor is not breaking", "1.0.0", "1.0.0", "1.1.0", ".\nsvc\n", "1.0.0", "false", ""},
		{"a prerelease target is breaking", "0.2.0", "0.2.0", "0.2.1-rc.1", ".\nsvc\n", "0.2.0", "true", ""},
		{"a prerelease replaced by its release", "0.2.1-rc.1", "0.2.1-rc.1", "0.2.1", ".\nsvc\n", "0.2.1-rc.1", "false", ""},
		{"versions sort numerically", "0.10.0", "0.9.0", "0.10.1", ".\nsvc\n", "0.9.0,0.10.0", "true", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mkRepo(t, tc.root, tc.nested)
			log, out, dirs, err := c.rewrite(tc.target)
			if err != nil {
				t.Fatal(err, log)
			}
			if dirs != tc.dirs || out["from"] != tc.from || out["breaking"] != tc.breaking {
				t.Errorf("dirs %q from %q breaking %q (%s)", dirs, out["from"], out["breaking"], log)
			}
			if tc.keys != "" && !strings.Contains(c.f("PklProject.deps.json"), tc.keys) {
				t.Errorf("missing %s in\n%s", tc.keys, c.f("PklProject.deps.json"))
			}
			if tc.target == "1.0.0" && strings.Contains(c.f("PklProject.deps.json"), `@0"`) {
				t.Errorf("a major-only key stayed @0:\n%s", c.f("PklProject.deps.json"))
			}
			if tc.target == "0.2.1-rc.1" && !strings.Contains(c.f("PklProject"), "v0.2.1-rc.1/contracts.vocab@0.2.1-rc.1") {
				t.Errorf("rc not written:\n%s", c.f("PklProject"))
			}
		})
	}
}

func TestRewriteRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(c *consumer)
		target string
		want   string
	}{
		{"tag and package version disagree", func(c *consumer) {
			c.edit("PklProject", "v0.2.0/contracts.model@0.2.0", "v0.2.0/contracts.model@0.1.0")
		}, "0.2.1",
			"::error file=PklProject,line=5::'" + U + "/v0.2.0/contracts.model@0.1.0' names release v0.2.0 but package version 0.1.0\n"},
		{"a floating version", func(c *consumer) { c.edit("PklProject", "contracts.model@0.2.0", "contracts.model@latest") }, "0.2.1",
			"::error file=PklProject,line=5::'" + U + "/v0.2.0/contracts.model@latest' is a dependency into o/lib but not of the shape v<version>/<name>@<version> — refusing to guess\n"},
		{"a tag without its v", func(c *consumer) {
			c.edit("PklProject", "download/v0.2.0/contracts.model", "download/0.2.0/contracts.model")
		}, "0.2.1", "not of the shape"},
		{"a malformed URI in a second project stops the first from being written", func(c *consumer) {
			c.edit("svc/PklProject", `download/v0.2.0/contracts.vocab@0.2.0"`, `download/v0.2/contracts.vocab@0.2"`)
		}, "0.2.1", "::error file=svc/PklProject,line=3::"},
		{"a major-only key that is not the release's major", func(c *consumer) { c.edit("PklProject.deps.json", `contracts.vocab@0"`, `contracts.vocab@7"`) }, "0.2.1",
			"has major 7 but release v0.2.0\n"},
		{"a downgrade", func(c *consumer) {
			c.edit("PklProject", "v0.2.0/contracts.vocab@0.2.0", "v0.3.0/contracts.vocab@0.3.0")
		}, "0.2.1", "::error file=PklProject,line=4::v0.3.0 is newer than the target v0.2.1 — refusing to downgrade\n"},
		{"a target that is not a version", func(c *consumer) {}, "latest", "::error::target 'latest' is not a version (X.Y.Z, optionally -prerelease)\n"},
		{"a target with a shell in it", func(c *consumer) {}, "0.2.1;touch x", "::error::target '0.2.1;touch x' is not a version (X.Y.Z, optionally -prerelease)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mkRepo(t, "0.2.0", "0.2.0")
			tc.mutate(c)
			before := c.snapshot()
			log, _, _, err := c.rewrite(tc.target)
			if !errors.Is(err, ErrFailed) || !strings.Contains(log, tc.want) {
				t.Errorf("err %v log %q, want %q", err, log, tc.want)
			}
			if c.snapshot() != before {
				t.Error("refused but wrote files")
			}
		})
	}
	// Every refusal is reported, not the first.
	c := mkRepo(t, "0.2.0", "0.2.0")
	c.edit("PklProject", "contracts.model@0.2.0", "contracts.model@latest")
	c.edit("svc/PklProject", `contracts.vocab@0.2.0" }`, `contracts.vocab@0.1.0" }`)
	if log, _, _, _ := c.rewrite("0.2.1"); strings.Count(log, "::error file=") != 2 {
		t.Errorf("both faults are reported: %s", log)
	}

	c = mkRepo(t, "0.2.0", "0.2.0")
	git(t, c.dir, "rm", "-q", "PklProject", "svc/PklProject")
	if log, _, _, err := c.rewrite("0.2.1"); !errors.Is(err, ErrFailed) || log != "::error::no PklProject in this repository\n" {
		t.Errorf("no PklProject: %v %q", err, log)
	}
	if log, _, _, err := mkRepo(t, "0.2.0", "0.2.0").rewriteSource("o/lib; rm"); !errors.Is(err, ErrFailed) || log != "::error::source 'o/lib; rm' is not owner/name\n" {
		t.Errorf("source: %v %q", err, log)
	}
}

func (c *consumer) rewriteSource(source string) (string, struct{}, struct{}, error) {
	var b bytes.Buffer
	err := Rewrite(context.Background(), RewriteOptions{Common: Common{Out: &b, Dir: c.dir}, Target: "0.2.1", Source: source, DirsFile: filepath.Join(c.tmp, "dirs")})
	return b.String(), struct{}{}, struct{}{}, err
}

// versionCmp is what GNU `sort -V` does to the versions this step sorts.
func TestVersionCmpMatchesSortV(t *testing.T) {
	if _, err := exec.LookPath("sort"); err != nil {
		t.Skip("no sort")
	}
	vs := []string{"0.2.0", "0.10.0", "0.9.0", "1.0.0", "0.2.1-rc.1", "0.2.1", "0.2.1-rc.10", "0.2.1-rc.2", "1.0.0-beta", "1.0.0-alpha.1", "10.0.0", "2.0.0", "0.0.9", "0.2.0-rc.1"}
	cmd := exec.Command("sort", "-V")
	cmd.Stdin = strings.NewReader(strings.Join(vs, "\n") + "\n")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	want, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), vs...)
	sort.Slice(got, func(i, j int) bool { return versionCmp(got[i], got[j]) < 0 })
	if strings.Join(got, "\n")+"\n" != string(want) {
		t.Errorf("got\n%s\nsort -V\n%s", strings.Join(got, "\n"), want)
	}
}

// ── resolve ──────────────────────────────────────────────────────────────

func runResolve(t *testing.T, recipes string, justMissing bool, failOn string, pklCmd string) (calls []string, err error) {
	t.Helper()
	d := t.TempDir()
	write(t, d+"/dirs", ".\nsvc\n")
	var b bytes.Buffer
	exec := func(_ context.Context, c runcmd.Cmd) error {
		if c.Name != "devbox" || c.Args[0] != "run" || c.Args[1] != "--" {
			t.Fatalf("unexpected %s %v", c.Name, c.Args)
		}
		a := c.Args[2:]
		if a[0] == "just" && a[1] == "--summary" {
			if justMissing {
				return errors.New("just: not found")
			}
			fmt.Fprintln(c.Stdout, recipes)
			return nil
		}
		calls = append(calls, strings.Join(a, " "))
		if failOn != "" && strings.HasPrefix(strings.Join(a, " "), failOn) {
			return errors.New("fail")
		}
		return nil
	}
	err = Resolve(context.Background(), ResolveOptions{Common: Common{Out: &b, Exec: exec}, DirsFile: d + "/dirs", PklCommand: pklCmd})
	return
}

func TestResolve(t *testing.T) {
	j := func(s []string) string { return strings.Join(s, ";") }
	for _, tc := range []struct {
		name, recipes, pkl, failOn, want string
		justMissing, fails               bool
	}{
		{"resolve and generate recipes", "test resolve generate lint", "pkl", "", "just resolve;just generate", false, false},
		{"no recipes: direct resolve of each project, no generate", "test lint", "pkl", "", "pkl project resolve .;pkl project resolve svc", false, false},
		{"pkl-command is what runs", "", "bin/pkl", "", "bin/pkl project resolve .;bin/pkl project resolve svc", false, false},
		{"generate without resolve: direct resolve, then generate", "generate", "pkl", "", "pkl project resolve .;pkl project resolve svc;just generate", false, false},
		{"no just in the devbox: direct resolve", "resolve generate", "pkl", "", "pkl project resolve .;pkl project resolve svc", true, false},
		{"recipe names are matched whole", "resolvex regenerate", "pkl", "", "pkl project resolve .;pkl project resolve svc", false, false},
		{"a failing resolve fails the job before generate", "resolve generate", "pkl", "just resolve", "just resolve", false, true},
		{"a failing generate fails the job", "resolve generate", "pkl", "just generate", "just resolve;just generate", false, true},
		{"a failing direct resolve fails the job", "", "pkl", "pkl", "pkl project resolve .", false, true},
	} {
		calls, err := runResolve(t, tc.recipes, tc.justMissing, tc.failOn, tc.pkl)
		if (err != nil) != tc.fails || j(calls) != tc.want {
			t.Errorf("%s: err %v calls %s want %s", tc.name, err, j(calls), tc.want)
		}
	}
}
