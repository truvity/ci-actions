package taggedpins

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The cases of hack/tagged-pins-cases.sh, as table tests.
//
// NO NETWORK, and no test-only seam in the code under test: `git
// ls-remote` resolves the https URL the package builds through a
// url.<base>.insteadOf rule in a throwaway GIT_CONFIG_GLOBAL, so the thing
// under test is the unmodified command line a job would run.

type library struct{ tagged, loose string }

type env struct {
	work string
	libs map[string]library
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newEnv(t *testing.T) *env {
	t.Helper()
	work := t.TempDir()
	cfg := filepath.Join(work, "gitconfig")
	gitconfig := fmt.Sprintf(`[url "%s/remotes/"]
	insteadOf = https://github.com/
[user]
	email = ci@example.invalid
	name = ci
[init]
	defaultBranch = master
[safe]
	directory = *
`, work)
	if err := os.WriteFile(cfg, []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	return &env{work: work, libs: map[string]library{}}
}

// makeLibrary: a remote holding one ANNOTATED tag and one commit after it.
// Annotated on purpose: a release here is an annotated tag, and the tag
// OBJECT sha is the plausible-looking wrong answer that the `^{}`
// dereference exists to reject.
func (e *env) makeLibrary(t *testing.T, slug string) library {
	t.Helper()
	bare := filepath.Join(e.work, "remotes", slug)
	src := filepath.Join(e.work, "src", slug)
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, bare, "init", "-q", "--bare")
	git(t, src, "init", "-q")
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(src, "a"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "add", "-A")
		git(t, src, "commit", "-qm", s)
	}
	write("one")
	git(t, src, "tag", "-a", "v1.0.0", "-m", "v1.0.0")
	write("two")
	git(t, src, "push", "-q", bare, "HEAD:master", "--tags")
	l := library{
		tagged: git(t, src, "rev-parse", "v1.0.0^{}"),
		loose:  git(t, src, "rev-parse", "HEAD"),
	}
	e.libs[slug] = l
	return l
}

// run puts the given `uses:` lines in a workspace at rel and runs the
// package over it, as a job would.
func (e *env) run(t *testing.T, rel, libraries string, uses ...string) (string, error) {
	t.Helper()
	ws := filepath.Join(e.work, "ws")
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, filepath.Dir(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("jobs:\n  x:\n    steps:\n")
	for _, u := range uses {
		b.WriteString("      - uses: " + u + "\n")
	}
	if err := os.WriteFile(filepath.Join(ws, rel), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Run(Options{Root: ws, Libraries: libraries, Out: &out})
	return out.String(), err
}

func TestTaggedPins(t *testing.T) {
	e := newEnv(t)
	wf := e.makeLibrary(t, "truvity/ci-workflows")
	ac := e.makeLibrary(t, "truvity/ci-actions")
	cc := e.makeLibrary(t, "truvity/ci-cache")

	const wfPath = "truvity/ci-workflows/.github/workflows/check.yaml@"
	const w = ".github/workflows/w.yaml"

	cases := []struct {
		name      string
		rel       string
		libraries string
		uses      []string
		wantErr   bool
		contains  []string
	}{
		{
			name: "a pin naming a release is accepted", rel: w,
			libraries: "truvity/ci-workflows",
			uses:      []string{wfPath + wf.tagged},
			contains:  []string{"ok        truvity/ci-workflows/.github/workflows/check.yaml", "  v1.0.0\n", "tagged-pins: 1 of 1 libraries had pins in this checkout, all naming releases"},
		},
		{
			name: "a pin naming no release is refused and named", rel: w,
			libraries: "truvity/ci-workflows",
			uses:      []string{wfPath + wf.loose},
			wantErr:   true,
			contains:  []string{"UNTAGGED", "names no release; newest is v1.0.0", "::error::A pinned commit is not a release."},
		},
		{
			name: "an untagged pin in the SECOND library is refused", rel: w,
			libraries: "truvity/ci-workflows truvity/ci-actions",
			uses:      []string{wfPath + wf.tagged, "truvity/ci-actions/setup-devbox@" + ac.loose},
			wantErr:   true,
			contains:  []string{"truvity/ci-actions/setup-devbox"},
		},
		{
			name: "commas separate libraries as well as spaces", rel: w,
			libraries: "truvity/ci-workflows,truvity/ci-actions",
			uses:      []string{wfPath + wf.tagged, "truvity/ci-actions/setup-devbox@" + ac.tagged},
			contains:  []string{"2 of 2 libraries"},
		},
		{
			// ci-cache's pin lives in setup-devbox/action.yaml, not under
			// .github. A search scoped to .github would default ci-cache
			// into the list and report "nothing to check" forever.
			name: "an untagged ci-cache pin OUTSIDE .github is refused", rel: "setup-devbox/action.yaml",
			libraries: "truvity/ci-workflows truvity/ci-actions truvity/ci-cache",
			uses:      []string{wfPath + wf.tagged, "truvity/ci-cache/setup@" + cc.loose},
			wantErr:   true,
			contains:  []string{"truvity/ci-cache/setup"},
		},
		{
			name: "a tagged ci-cache pin outside .github is accepted", rel: "setup-devbox/action.yaml",
			libraries: "truvity/ci-workflows truvity/ci-actions truvity/ci-cache",
			uses:      []string{wfPath + wf.tagged, "truvity/ci-cache/setup@" + cc.tagged},
			contains:  []string{"2 of 3 libraries"},
		},
		{
			name: "a repository pinning none passes and says how many it found", rel: w,
			libraries: "truvity/ci-workflows truvity/ci-actions",
			uses:      []string{"actions/checkout@0000000000000000000000000000000000000000"},
			contains: []string{
				"no truvity/ci-workflows pins in this checkout — nothing to check",
				"no truvity/ci-actions pins in this checkout — nothing to check",
				"0 of 2 libraries had pins",
			},
		},
		{
			// An action input is always set and usually empty: empty must
			// fall back to the default, not check nothing.
			name: "a blank libraries input falls back to the default", rel: w,
			libraries: "",
			uses:      []string{wfPath + wf.loose},
			wantErr:   true,
			contains:  []string{"UNTAGGED"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.run(t, tc.rel, tc.libraries, tc.uses...)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tc.wantErr, out)
			}
			if tc.wantErr && !errors.Is(err, ErrUntagged) {
				t.Fatalf("err = %v, want ErrUntagged", err)
			}
			for _, c := range tc.contains {
				if !strings.Contains(out, c) {
					t.Errorf("output lacks %q:\n%s", c, out)
				}
			}
		})
	}
}

func TestOutputLines(t *testing.T) {
	// The exact line shapes the shell script printed.
	e := newEnv(t)
	wf := e.makeLibrary(t, "truvity/ci-workflows")
	out, err := e.run(t, ".github/workflows/w.yaml", "truvity/ci-workflows",
		"truvity/ci-workflows/.github/workflows/check.yaml@"+wf.tagged,
		"truvity/ci-workflows/.github/workflows/other.yaml@"+wf.loose)
	if err == nil {
		t.Fatal("want error")
	}
	want := fmt.Sprintf("ok        %-46s %.12s  %s\nUNTAGGED  %-46s %.12s  names no release; newest is %s\n",
		"truvity/ci-workflows/.github/workflows/check.yaml", wf.tagged, "v1.0.0",
		"truvity/ci-workflows/.github/workflows/other.yaml", wf.loose, "v1.0.0")
	if !strings.HasPrefix(out, want) {
		t.Errorf("got:\n%s\nwant prefix:\n%s", out, want)
	}
}

func TestParseLibraries(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", DefaultLibraries},
		{"  ", DefaultLibraries},
		{"a/b", "a/b"},
		{"a/b,c/d", "a/b c/d"},
		{"a/b, c/d  e/f", "a/b c/d e/f"},
	} {
		if got := strings.Join(ParseLibraries(tc.in), " "); got != tc.want {
			t.Errorf("ParseLibraries(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseLsRemote(t *testing.T) {
	sha := strings.Repeat("a", 40)
	obj := strings.Repeat("b", 40)
	in := obj + "\trefs/tags/v1.0.0\n" + sha + "\trefs/tags/v1.0.0^{}\n" + sha + "\trefs/heads/master\n"
	got := parseLsRemote([]byte(in))
	if len(got) != 1 || got[0].Commit != sha || got[0].Name != "v1.0.0" {
		t.Errorf("got %+v: only the dereferenced commit of an annotated tag counts", got)
	}
}
