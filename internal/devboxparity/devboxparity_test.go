package devboxparity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const token = "ghs_never_leak_this"

// fake is a recording Exec. Responses are keyed by "name arg arg"; a missing
// key succeeds with no output.
type fake struct {
	calls   []string
	tokenIn map[string]bool // commands that saw GH_TOKEN
	out     map[string]string
	fail    map[string]bool
	hook    func(c Cmd, line string) (handled bool, err error)
	wd      string
}

func (f *fake) exec(_ context.Context, c Cmd) error {
	line := c.Name + " " + strings.Join(c.Args, " ")
	rel, _ := filepath.Rel(f.wd, c.Dir)
	f.calls = append(f.calls, rel+": "+line)
	if f.tokenIn == nil {
		f.tokenIn = map[string]bool{}
	}
	for _, kv := range c.Env {
		if strings.HasPrefix(kv, "GH_TOKEN=") || strings.HasPrefix(kv, "TOKEN=") {
			f.tokenIn[line] = true
		}
	}
	if f.hook != nil {
		if ok, err := f.hook(c, line); ok {
			return err
		}
	}
	if f.fail[line] {
		return errors.New("exit status 1")
	}
	if s, ok := f.out[line]; ok && c.Stdout != nil {
		c.Stdout.Write([]byte(s))
	}
	return nil
}

func (f *fake) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

type env struct {
	t   *testing.T
	dir string
	f   *fake
	o   Options
	out bytes.Buffer
	err bytes.Buffer
	gh  string // GITHUB_OUTPUT file
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir}
	e.f = &fake{wd: dir, out: map[string]string{}, fail: map[string]bool{}}
	e.gh = filepath.Join(dir, "..", filepath.Base(dir)+".out")
	e.o = Options{
		Token: token, WorkDir: dir, Base: "master", Label: "dependencies", Mode: "align",
		FullDay: "1", ModuleDirs: "[]", RunnerTemp: t.TempDir(), GithubOutput: e.gh,
		Environ: []string{"PATH=/usr/bin", "TOKEN=" + token, "GH_TOKEN=" + token, "HOME=/h"},
		Exec:    e.f.exec, Now: func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }, // a Monday
		SkipGHInstall: true,
	}
	e.o.Out, e.o.Err = &e.out, &e.err
	t.Cleanup(func() { os.Remove(e.gh) })
	return e
}

func (e *env) write(name, content string) {
	e.t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(name string) string {
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) run() error {
	e.t.Helper()
	err := Run(context.Background(), e.o)
	if strings.Contains(e.out.String()+e.err.String(), token) {
		e.t.Error("the token was printed")
	}
	return err
}

func (e *env) outputs() string {
	b, _ := os.ReadFile(e.gh)
	return string(b)
}

func goDL(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		fmt.Fprint(w, `[{"version":"go1.26rc1"},{"version":"go1.25.7"},{"version":"go1.25.6"},{"version":"go1.24.13"},{"version":"go1.24.2"}]`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *env) built(v string) {
	e.f.out["devbox run -- golangci-lint version"] = "golangci-lint has version 1.64.8 built with go" + v + " from abc on 2026\n"
}

// A go.mod edit, as `go mod edit -toolchain=` makes it.
func (e *env) goModEdit() {
	e.f.hook = func(c Cmd, line string) (bool, error) {
		if !strings.HasPrefix(line, "devbox run -- go mod edit -toolchain=") {
			return false, nil
		}
		val := strings.TrimPrefix(line, "devbox run -- go mod edit -toolchain=")
		p := filepath.Join(c.Dir, "go.mod")
		b, _ := os.ReadFile(p)
		lines := strings.Split(string(b), "\n")
		var out []string
		for _, l := range lines {
			if strings.HasPrefix(l, "toolchain ") {
				continue
			}
			out = append(out, l)
			if strings.HasPrefix(l, "go ") && val != "none" {
				out = append(out, "toolchain "+val)
			}
		}
		return true, os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o644)
	}
}

func TestVersionCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.25", "1.25", 0}, {"1.25.4", "1.25.10", -1}, {"1.9", "1.10", -1}, {"1.25", "1.25.1", -1},
		{"1.26", "1.25.9", 1}, {"1.25.7", "1.25.7", 0}, {"1.26rc1", "1.26", 1}, {"1.026", "1.26", 0},
	} {
		if got := versionCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("versionCompare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := versionCompare(tc.b, tc.a); got != -tc.want {
			t.Errorf("versionCompare(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
	if firstTwo("1.25.4") != "1.25" || firstTwo("1.25") != "1.25" || firstTwo("1") != "1" {
		t.Error("firstTwo")
	}
}

func TestMode(t *testing.T) {
	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	sunday := time.Date(2026, 10, 4, 23, 59, 0, 0, time.UTC)
	for _, tc := range []struct {
		mode, day string
		now       time.Time
		line      string
		update    bool
	}{
		{"auto", "1", monday, "mode=auto (weekday 1, full-update-day 1) -> full=true", true},
		{"auto", "1", sunday, "mode=auto (weekday 7, full-update-day 1) -> full=false", false},
		{"auto", "7", sunday, "mode=auto (weekday 7, full-update-day 7) -> full=true", true},
		{"full", "3", monday, "mode=full (weekday 1, full-update-day 3) -> full=true", true},
		{"align", "1", monday, "mode=align (weekday 1, full-update-day 1) -> full=false", false},
	} {
		e := newEnv(t)
		e.o.Mode, e.o.FullDay, e.o.Now = tc.mode, tc.day, func() time.Time { return tc.now }
		if err := e.run(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.out.String(), tc.line+"\n") {
			t.Errorf("%v: want %q in\n%s", tc, tc.line, e.out.String())
		}
		if e.f.called(": devbox update") != tc.update {
			t.Errorf("%v: devbox update ran = %v", tc, !tc.update)
		}
	}
}

func TestGoToolchain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gomod     string
		built     string // golangci-lint's Go; "" makes golangci-lint fail and go env answer 1.25.4
		want      string // lines expected in the output
		edit      string // the -toolchain flag expected, "" for none
		wantMod   string // go.mod afterwards, "" to skip
		unreachDL bool
	}{
		{"older toolchain is raised", "module x\n\ngo 1.25\n\ntoolchain go1.25.1\n", "1.25.4",
			"aligning toolchain 1.25.1 -> 1.25.7 (language stays 1.25)", "-toolchain=go1.25.7", "toolchain go1.25.7", false},
		{"no toolchain line starts from the language", "module x\n\ngo 1.25.0\n", "1.25.4",
			".: language 1.25.0, toolchain 1.25.0; cap: go 1.25 line (golangci built with 1.25.4)", "-toolchain=go1.25.7", "", false},
		{"already newer is never lowered", "module x\n\ngo 1.25\n\ntoolchain go1.25.9\n", "1.25.4",
			"toolchain 1.25.9 already at or above 1.25.7 — no change", "", "", false},
		{"already equal", "module x\n\ngo 1.25\n\ntoolchain go1.25.7\n", "1.25.4",
			"toolchain 1.25.7 already at or above 1.25.7 — no change", "", "", false},
		{"language above the cap is a human's", "module x\n\ngo 1.26\n", "1.25.4",
			"::warning::.: language directive 1.26 is above the golangci-lint cap 1.25 — a human set this; not touching go.mod", "", "", false},
		{"target equal to the language drops the toolchain", "module x\n\ngo 1.25.7\n\ntoolchain go1.25.1\n", "1.25.4",
			"aligning toolchain 1.25.1 -> 1.25.7 (language stays 1.25.7)", "-toolchain=none", "go 1.25.7", false},
		{"no golangci-lint falls back to the devbox go", "module x\n\ngo 1.25\n\ntoolchain go1.25.1\n", "",
			"golangci built with 1.25.4)", "-toolchain=go1.25.7", "", false},
		{"go.dev unreachable leaves it alone", "module x\n\ngo 1.25\n\ntoolchain go1.25.1\n", "1.25.4",
			".: go.dev unreachable — toolchain stays 1.25.1 this run", "", "", true},
		{"a line go.dev has no release of", "module x\n\ngo 1.23\n\ntoolchain go1.23.1\n", "1.23.4",
			": go.dev unreachable — toolchain stays 1.23.1 this run", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.goModEdit()
			e.write("go.mod", tc.gomod)
			if tc.built != "" {
				e.built(tc.built)
			} else {
				e.f.fail["devbox run -- golangci-lint version"] = true
				e.f.out["devbox run -- go env GOVERSION"] = "go1.25.4\n"
			}
			srv := goDL(t, nil)
			e.o.GoDLURL = srv.URL
			if tc.unreachDL {
				e.o.GoDLURL = "http://127.0.0.1:1/"
			}
			if err := e.run(); err != nil {
				t.Fatal(err, e.out.String())
			}
			if !strings.Contains(e.out.String(), tc.want) {
				t.Errorf("want %q in\n%s", tc.want, e.out.String())
			}
			edited := ""
			for _, c := range e.f.calls {
				if i := strings.Index(c, "go mod edit "); i >= 0 {
					edited = c[i+len("go mod edit "):]
				}
			}
			if edited != tc.edit {
				t.Errorf("go mod edit %q, want %q", edited, tc.edit)
			}
			if tc.edit != "" && !e.f.called(": git diff -- ./go.mod") {
				t.Error("the change is shown with git diff")
			}
			if tc.wantMod != "" && !strings.Contains(e.read("go.mod"), tc.wantMod) {
				t.Errorf("go.mod:\n%s", e.read("go.mod"))
			}
		})
	}
}

func TestGoModuleDirs(t *testing.T) {
	var hits atomic.Int32
	srv := goDL(t, &hits)
	e := newEnv(t)
	e.goModEdit()
	e.o.GoDLURL = srv.URL
	e.o.ModuleDirs = `["tools","svc/a","tools","gone","."]`
	e.write("go.mod", "module r\n\ngo 1.25\n\ntoolchain go1.25.1\n")
	e.write("tools/go.mod", "module t\n\ngo 1.25\n\ntoolchain go1.25.6\n")
	e.write("svc/a/go.mod", "module a\n\ngo 1.24\n\ntoolchain go1.24.1\n")
	e.built("1.25.4")
	// svc/a is on another line: golangci-lint is the same binary, so it is
	// capped at 1.25 and a 1.24 module is below the cap: its toolchain moves
	// to the newest 1.25 patch only if that is not downward (it is not).
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	o := e.out.String()
	for _, want := range []string{
		"gone: no go.mod — nothing to align\n",
		"tools: language 1.25, toolchain 1.25.6; cap: go 1.25 line",
		"tools: aligning toolchain 1.25.6 -> 1.25.7",
		"svc/a: aligning toolchain 1.24.1 -> 1.25.7 (language stays 1.24)",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("want %q in\n%s", want, o)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("go.dev is asked once per cap line, got %d", hits.Load())
	}
	// directories in jq's `unique` order, each once
	var order []string
	for _, c := range e.f.calls {
		if strings.Contains(c, "golangci-lint version") {
			order = append(order, strings.SplitN(c, ":", 2)[0])
		}
	}
	if strings.Join(order, ",") != ".,svc/a,tools" {
		t.Errorf("order = %v", order)
	}
	if err := func() error { e2 := newEnv(t); e2.o.ModuleDirs = "nope"; return e2.run() }(); err == nil {
		t.Error("module-dirs that is not a JSON array fails")
	}
}

const lockWith = `{"lockfile_version":"1","packages":{"go@1.25":{"version":"1.25.4"},"playwright-driver@latest":{"version":"1.49.1"},"playwright-test@latest":{"version":"1.50.0"}}}`

func TestPlaywright(t *testing.T) {
	pkg := "{\n  \"name\": \"x\",\n  \"packageManager\": \"yarn@4.5.0\",\n  \"dependencies\": {\n    \"zod\": \"^3\"\n  },\n  \"devDependencies\": {\n    \"@playwright/test\": \"1.40.0\",\n    \"playwright\": \"^1.40.0\",\n    \"a-lib\": \"1.0.0\"\n  }\n}\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		out   string
		args  string // the lockfile command expected, "" for none
		after string // package.json afterwards, "" to skip
	}{
		{"no pair", map[string]string{"package.json": "{}"}, "no devbox.lock + package.json pair — nothing to align\n", "", ""},
		{"devbox does not pin it", map[string]string{"devbox.lock": `{"packages":{"go@1":{"version":"1"}}}`, "package.json": pkg}, "devbox does not pin playwright — nothing to align\n", "", ""},
		{"package.json does not depend on it", map[string]string{"devbox.lock": lockWith, "package.json": `{"dependencies":{"a":"1"}}`}, "package.json does not depend on playwright — nothing to align\n", "", ""},
		{"already aligned", map[string]string{"devbox.lock": lockWith, "package.json": `{"devDependencies":{"@playwright/test":"1.49.1"}}`}, "playwright 1.49.1 already aligned\n", "", ""},
		{"yarn berry", map[string]string{"devbox.lock": lockWith, "package.json": pkg, "yarn.lock": ""},
			"aligning playwright 1.40.0 -> 1.49.1 (devbox leads)\n", "yarn install --mode=update-lockfile",
			"{\n  \"name\": \"x\",\n  \"packageManager\": \"yarn@4.5.0\",\n  \"dependencies\": {\n    \"zod\": \"^3\"\n  },\n  \"devDependencies\": {\n    \"@playwright/test\": \"1.49.1\",\n    \"playwright\": \"1.49.1\",\n    \"a-lib\": \"1.0.0\"\n  }\n}\n"},
		{"yarn classic", map[string]string{"devbox.lock": lockWith, "package.json": strings.Replace(pkg, "yarn@4.5.0", "yarn@1.22.0", 1), "yarn.lock": ""}, "aligning playwright", "yarn install --ignore-scripts", ""},
		{"yarn without packageManager", map[string]string{"devbox.lock": lockWith, "package.json": strings.Replace(pkg, "  \"packageManager\": \"yarn@4.5.0\",\n", "", 1), "yarn.lock": ""}, "aligning playwright", "yarn install --ignore-scripts", ""},
		{"pnpm", map[string]string{"devbox.lock": lockWith, "package.json": pkg, "pnpm-lock.yaml": ""}, "aligning playwright", "pnpm install --lockfile-only", ""},
		{"npm", map[string]string{"devbox.lock": lockWith, "package.json": pkg, "package-lock.json": "{}"}, "aligning playwright", "npm install --package-lock-only --ignore-scripts", ""},
		{"no lockfile", map[string]string{"devbox.lock": lockWith, "package.json": pkg}, "::warning::no lockfile found — package.json moved, nothing else\n", "", ""},
		// only devDependencies: no `"dependencies": null` is invented
		{"dev only, formatting kept", map[string]string{"devbox.lock": lockWith, "package.json": "{\n\t\"devDependencies\": {\"playwright\": \"1.0.0\"}\n}", "package-lock.json": "{}"},
			"aligning playwright 1.0.0 -> 1.49.1", "npm install --package-lock-only --ignore-scripts", "{\n\t\"devDependencies\": {\"playwright\": \"1.49.1\"}\n}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			for n, c := range tc.files {
				e.write(n, c)
			}
			e.o.GoDLURL = "http://127.0.0.1:1/"
			if err := e.run(); err != nil {
				t.Fatal(err, e.out.String())
			}
			if !strings.Contains(e.out.String(), tc.out) {
				t.Errorf("want %q in\n%s", tc.out, e.out.String())
			}
			ran := ""
			for _, c := range e.f.calls {
				for _, pm := range []string{"yarn", "pnpm", "npm"} {
					if strings.Contains(c, "devbox run -- "+pm+" ") {
						ran = c[strings.Index(c, "-- ")+3:]
					}
				}
			}
			if ran != tc.args {
				t.Errorf("lockfile command %q, want %q", ran, tc.args)
			}
			if tc.after != "" && e.read("package.json") != tc.after {
				t.Errorf("package.json:\n%s\nwant:\n%s", e.read("package.json"), tc.after)
			}
			if strings.Contains(e.read("package.json"), "null") && !strings.Contains(tc.files["package.json"], "null") {
				t.Error("a null key was invented")
			}
		})
	}
}

func TestPlaywrightFirstPackageWins(t *testing.T) {
	e := newEnv(t)
	e.write("devbox.lock", `{"packages":{"playwright-driver@x":{"version":""},"playwright-test@x":{"version":"1.2.3"},"playwright@y":{"version":"9.9.9"}}}`)
	e.write("package.json", `{"dependencies":{"playwright":"1.0.0"},"devDependencies":{"@playwright/test":"1.1.0"}}`)
	e.write("package-lock.json", "{}")
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "aligning playwright 1.0.0 -> 1.2.3") {
		t.Errorf("%s", e.out.String())
	}
	if got := e.read("package.json"); got != `{"dependencies":{"playwright":"1.2.3"},"devDependencies":{"@playwright/test":"1.2.3"}}` {
		t.Errorf("both sections follow: %s", got)
	}
}

// gh answers for the pull-request phase.
func (e *env) ghAnswers(existing string) {
	e.f.out["gh repo view --json nameWithOwner --jq .nameWithOwner"] = "acme/app\n"
	e.f.out["gh pr list --head chore/devbox-update --state open --json url --jq .[0].url"] = existing
	e.f.out["gh pr create --title chore(deps): update devbox packages and align the followers --body "+prBody+" --base master --label dependencies"] = "https://github.com/acme/app/pull/7\n"
}

const prBody = "Automated parity: `devbox update` on the configured weekday, and every follower aligned to its devbox pin — the go toolchain directive (language minor capped by golangci-lint's build Go, patch = newest of that line, never downward) and the playwright npm package (exactly the nix-pinned driver). Nix packages are outside Renovate's reach and the followers are owned here, so the pairs can never disagree."

func apiStub(t *testing.T, rules, protection string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization %q", r.Header.Get("Authorization"))
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/acme/app/rules/branches/master"):
			fmt.Fprint(w, rules)
		case strings.HasSuffix(r.URL.Path, "/protection"):
			fmt.Fprint(w, protection)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const withRuleset = `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"check"}]}}]`

func TestPullRequest(t *testing.T) {
	changed := func(e *env) {
		e.f.fail["git diff --quiet -- devbox.json devbox.lock go.mod package.json yarn.lock pnpm-lock.yaml package-lock.json"] = true
		e.f.fail["git diff --cached --quiet"] = true // something is staged
		e.write("devbox.json", "{}")
		e.write("devbox.lock", "{}")
		e.write("yarn.lock", "")
	}
	t.Run("at parity", func(t *testing.T) {
		e := newEnv(t)
		if err := e.run(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.out.String(), "everything already at parity\n") || e.outputs() != "changed=false\n" {
			t.Errorf("%s / %s", e.out.String(), e.outputs())
		}
		if e.f.called("git checkout") || e.f.called("gh ") {
			t.Errorf("nothing is touched at parity: %v", e.f.calls)
		}
	})
	t.Run("opens a pull request and arms auto-merge", func(t *testing.T) {
		e := newEnv(t)
		changed(e)
		e.ghAnswers("")
		e.o.APIURL = apiStub(t, withRuleset, `{"message":"Branch not protected"}`).URL
		e.o.ModuleDirs = `["tools"]`
		e.f.fail["git diff --quiet -- devbox.json devbox.lock go.mod package.json yarn.lock pnpm-lock.yaml package-lock.json tools/go.mod"] = true
		if err := e.run(); err != nil {
			t.Fatal(err, e.out.String())
		}
		want := []string{
			": git config user.name github-actions[bot]",
			": git config user.email 41898282+github-actions[bot]@users.noreply.github.com",
			": git checkout -B chore/devbox-update",
			": git add -- devbox.json", ": git add -- devbox.lock", ": git add -- yarn.lock",
			": devbox run -- git commit -m chore(deps): update devbox packages and align the followers",
			": devbox run -- git push --force origin chore/devbox-update",
			": gh label create dependencies --force --color 0366d6 --description Dependency updates",
			": gh pr merge --auto --rebase chore/devbox-update",
		}
		for _, w := range want {
			if !e.f.called(w) {
				t.Errorf("missing call %q in %v", w, e.f.calls)
			}
		}
		for _, absent := range []string{"git add -- pnpm-lock.yaml", "git add -- package.json", "git add -- go.mod", "git add -- tools/go.mod"} {
			if e.f.called(absent) {
				t.Errorf("only existing followers are staged: %s", absent)
			}
		}
		if !strings.Contains(e.out.String(), "required checks on master: 1 from rulesets, 0 from classic protection — arming auto-merge\n") {
			t.Errorf("%s", e.out.String())
		}
		if e.outputs() != "changed=true\nurl=https://github.com/acme/app/pull/7\n" {
			t.Errorf("outputs %q", e.outputs())
		}
		for line, saw := range e.f.tokenIn {
			if !saw {
				continue
			}
			if !strings.HasPrefix(line, "gh ") && !strings.HasPrefix(line, "git ") && !strings.HasPrefix(line, "devbox run -- git ") {
				t.Errorf("the token reached %q", line)
			}
		}
	})
	t.Run("classic protection arms it too", func(t *testing.T) {
		e := newEnv(t)
		changed(e)
		e.ghAnswers("")
		e.o.APIURL = apiStub(t, `[]`, `{"required_status_checks":{"contexts":["a","b"]}}`).URL
		if err := e.run(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.out.String(), "required checks on master: 0 from rulesets, 2 from classic protection — arming auto-merge\n") || !e.f.called("gh pr merge --auto") {
			t.Errorf("%s", e.out.String())
		}
	})
	for name, api := range map[string][2]string{
		"nothing requires a check":               {`[]`, `{"message":"Branch not protected"}`},
		"unreadable sources fail toward a human": {`oops`, `{"message":"Not Found"}`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			changed(e)
			e.ghAnswers("")
			e.o.APIURL = apiStub(t, api[0], api[1]).URL
			if err := e.run(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(e.out.String(), "no required status check on master — neither a ruleset nor classic branch protection requires one; leaving the PR for a human to merge\n") || e.f.called("gh pr merge") {
				t.Errorf("%s / %v", e.out.String(), e.f.calls)
			}
			if !strings.Contains(e.outputs(), "changed=true") {
				t.Errorf("%q", e.outputs())
			}
		})
	}
	t.Run("an open pull request is updated in place", func(t *testing.T) {
		e := newEnv(t)
		changed(e)
		e.ghAnswers("https://github.com/acme/app/pull/3\n")
		if err := e.run(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.out.String(), "open PR for chore/devbox-update already exists — updated in place by the push\n") || e.f.called("gh pr create") || e.f.called("gh pr merge") {
			t.Errorf("%s / %v", e.out.String(), e.f.calls)
		}
		if e.outputs() != "changed=true\nurl=https://github.com/acme/app/pull/3\n" {
			t.Errorf("%q", e.outputs())
		}
	})
	t.Run("nothing staged is an error", func(t *testing.T) {
		e := newEnv(t)
		changed(e)
		e.f.hook = func(c Cmd, line string) (bool, error) {
			switch line {
			case "git diff --cached --quiet":
				return true, nil
			case "git status --porcelain":
				c.Stdout.Write([]byte(" M a\n?? b\n"))
				return true, nil
			}
			return false, nil
		}
		err := e.run()
		if !IsReported(err) || !strings.Contains(e.out.String(), "::error::followers changed but nothing was staged:  M a ?? b \n") {
			t.Errorf("%v / %q", err, e.out.String())
		}
	})
	t.Run("a failing command fails the run", func(t *testing.T) {
		e := newEnv(t)
		changed(e)
		e.f.fail["devbox run -- git push --force origin chore/devbox-update"] = true
		if err := e.run(); err == nil {
			t.Error("a failed push is an error")
		}
	})
}

func TestToolsDoNotSeeTheToken(t *testing.T) {
	e := newEnv(t)
	e.o.Mode = "full"
	e.write("go.mod", "module x\n\ngo 1.25\n")
	e.built("1.25.4")
	e.o.GoDLURL = goDL(t, nil).URL
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	for line, saw := range e.f.tokenIn {
		if saw && !strings.HasPrefix(line, "git diff --quiet") {
			t.Errorf("%q saw the token before the pull-request phase", line)
		}
	}
}

func TestEnsureGH(t *testing.T) {
	// a gh on PATH is used as is
	e := newEnv(t)
	e.o.SkipGHInstall = false
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n"), 0o755)
	e.o.Environ = []string{"PATH=" + bin}
	e.f.out[filepath.Join(bin, "gh")+" --version"] = "gh version 2.50.0 (2026)\nhttps://x\n"
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "gh present: gh version 2.50.0 (2026)\n") {
		t.Errorf("%s", e.out.String())
	}

	// otherwise it is installed, verified, and put on PATH
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho gh\n")
	tw.WriteHeader(&tar.Header{Name: "gh_2.97.0_linux_amd64/bin/gh", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(tgz.Bytes())
	good := hex.EncodeToString(sum[:]) + "  gh_2.97.0_linux_amd64.tar.gz\n"
	for name, sums := range map[string]string{"verified": good, "mismatch": strings.Repeat("0", 64) + "  gh_2.97.0_linux_amd64.tar.gz\n"} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, ".tar.gz"):
					w.Write(tgz.Bytes())
				case strings.HasSuffix(r.URL.Path, "_checksums.txt"):
					fmt.Fprint(w, sums)
				default:
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			e := newEnv(t)
			e.o.SkipGHInstall = false
			e.o.GHBaseURL = srv.URL
			e.o.ArchOverride = "x86_64"
			e.o.Environ = []string{"PATH=/nonexistent"}
			e.o.GithubPath = filepath.Join(t.TempDir(), "github_path")
			err := e.run()
			if name == "mismatch" {
				if !IsReported(err) || !strings.Contains(e.out.String(), "::error::checksum mismatch for gh_2.97.0_linux_amd64.tar.gz; refusing to run it\n") {
					t.Errorf("%v / %s", err, e.out.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err, e.out.String())
			}
			if !strings.Contains(e.out.String(), "gh installed: v2.97.0\n") {
				t.Errorf("%s", e.out.String())
			}
			if b, _ := os.ReadFile(e.o.GithubPath); !strings.HasSuffix(strings.TrimSpace(string(b)), "/gh/bin") {
				t.Errorf("GITHUB_PATH = %q", b)
			}
			if _, err := os.Stat(filepath.Join(e.o.RunnerTemp, "gh", "bin", "gh")); err != nil {
				t.Error(err)
			}
		})
	}
	e = newEnv(t)
	e.o.SkipGHInstall = false
	e.o.ArchOverride = "riscv64"
	e.o.Environ = []string{"PATH=/nonexistent"}
	if err := e.run(); !IsReported(err) || !strings.Contains(e.out.String(), "unsupported arch: riscv64\n") {
		t.Errorf("%v / %s", err, e.out.String())
	}
}

func TestGroupsAreTheOnlyAddedLines(t *testing.T) {
	e := newEnv(t)
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(e.out.String(), "\n") {
		if !regexp.MustCompile(`^::(end)?group::`).MatchString(l) && l != "" {
			kept = append(kept, l)
		}
	}
	want := "mode=align (weekday 1, full-update-day 1) -> full=false|.: no go.mod — nothing to align|no devbox.lock + package.json pair — nothing to align|everything already at parity"
	if strings.Join(kept, "|") != want {
		t.Errorf("%q", strings.Join(kept, "|"))
	}
}
