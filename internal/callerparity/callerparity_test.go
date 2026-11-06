package callerparity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const kitsDir = "../../caller-parity/kits"

func kit(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(kitsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// theirConfig is the depguard block as a repository carries it: under
// `linters.settings`, beside that repository's own lint settings.
func theirConfig(fragment string) string {
	var b strings.Builder
	b.WriteString("version: \"2\"\n\nlinters:\n  enable:\n    - depguard\n\n  settings:\n")
	for _, l := range strings.Split(fragment, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}

func stripComments(s string) string {
	re := regexp.MustCompile(`[ \t]*#.*$`)
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = re.ReplaceAllString(l, "")
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

type files map[string]string // path -> content

// stub is an in-memory GitHub: one directory of files per repository.
type stub struct {
	repos map[string]files
	srv   *httptest.Server
}

func newStub(t *testing.T, repos map[string]files) *stub {
	t.Helper()
	s := &stub{repos: repos}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(code int, v any) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		parts := strings.SplitN(strings.Trim(r.URL.Path, "/"), "/", 5)
		if parts[0] != "repos" || len(parts) < 3 {
			send(404, map[string]string{"message": "Not Found"})
			return
		}
		name := parts[2]
		if name == "repo-gone" {
			send(404, map[string]string{"message": "Not Found"})
			return
		}
		if len(parts) == 3 {
			send(200, map[string]string{"default_branch": "master"})
			return
		}
		if name == "forbidden" {
			send(403, map[string]string{"message": "Resource not accessible by integration"})
			return
		}
		content, ok := repos[name][parts[4]]
		if !ok {
			// a directory: GitHub answers 200 with a listing (an array)
			var listing []map[string]string
			for p := range repos[name] {
				if strings.HasPrefix(p, strings.TrimSuffix(parts[4], "/")+"/") {
					listing = append(listing, map[string]string{"path": p, "type": "file"})
				}
			}
			if len(listing) > 0 {
				send(200, listing)
				return
			}
		}
		if !ok {
			send(404, map[string]string{"message": "Not Found"})
			return
		}
		enc := base64.StdEncoding.EncodeToString([]byte(content))
		var wrapped strings.Builder // GitHub wraps the payload at 60 characters
		for i := 0; i < len(enc); i += 60 {
			end := i + 60
			if end > len(enc) {
				end = len(enc)
			}
			wrapped.WriteString(enc[i:end] + "\n")
		}
		send(200, map[string]string{"encoding": "base64", "content": wrapped.String()})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

type result struct {
	log, summary, output string
	err                  error
}

func (s *stub) run(t *testing.T, kits string, repos []string, failOnDiff bool) result {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	list := make([]string, len(repos))
	for i, r := range repos {
		list[i] = "stub/" + r
	}
	rj, _ := json.Marshal(list)
	o := Options{Token: "stub-token", Repositories: string(rj), Kits: kits, API: s.srv.URL, Out: &b,
		Summary: filepath.Join(dir, "summary"), Output: filepath.Join(dir, "output")}
	if failOnDiff {
		o.FailOnDiff = "true"
	}
	err := Run(context.Background(), o)
	sum, _ := os.ReadFile(o.Summary)
	out, _ := os.ReadFile(o.Output)
	if strings.Contains(b.String()+string(sum)+string(out), "stub-token") {
		t.Error("the token must never be printed")
	}
	return result{b.String(), string(sum), string(out), err}
}

func (r result) row(repo string) string {
	for _, l := range strings.Split(r.summary, "\n") {
		if strings.HasPrefix(l, "| stub/"+repo+" |") {
			return l
		}
	}
	return ""
}

// diffFor is that repository's diff block of the summary.
func (r result) diffFor(repo string) string {
	var in bool
	var out []string
	for _, l := range strings.Split(r.summary, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ stub/"+repo+" "):
			in = true
		case in && l == "```":
			return strings.Join(out, "\n")
		case in:
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func count(s, sub string) int { return len(regexp.MustCompile(sub).FindAllString(s, -1)) }

func TestCallerKits(t *testing.T) {
	sec, auto, lint := kit(t, "security.yaml"), kit(t, "auto-release.yaml"), kit(t, "golangci-depguard.yaml")
	pin := "0123456789abcdef0123456789abcdef01234567"
	pinRe := regexp.MustCompile(`@[0-9a-f]{40}`)
	cronRe := regexp.MustCompile(`cron: "[0-9]+ [0-9]+ `)

	// Every readable case also carries the import-ban block, verbatim, and a
	// go.mod: the kit's applies_if means a repository with no Go in it is
	// never even asked, and without one every case here would read n/a. The
	// cases that are ABOUT that kit are their own test below.
	base := func(f files) files {
		f[".golangci.yaml"] = theirConfig(lint)
		f["go.mod"] = "module example.com/stub\n"
		return f
	}
	wf := func(s, a string) files {
		f := files{}
		if s != "" {
			f[".github/workflows/security.yaml"] = s
		}
		if a != "" {
			f[".github/workflows/auto-release.yaml"] = a
		}
		return base(f)
	}
	dropTrigger := regexp.MustCompile(`(?m)^  push:\n.*\n`).ReplaceAllString(auto, "")
	extraInputs := sec + "      runner: ${{ vars.CI_RUNNER_LABEL_LARGE }}\n      goproxy: ${{ vars.CI_GOPROXY }}\n    secrets:\n      module-app-private-key: ${{ secrets.MODULE_APP_PRIVATE_KEY }}\n"
	extraBlock := strings.Replace(strings.Replace(sec, "\n  govulncheck:\n", "\n  vuln:\n", 1),
		"\npermissions:\n", "\nconcurrency:\n  group: security-${{ github.ref }}\n  cancel-in-progress: true\npermissions:\n", 1)

	s := newStub(t, map[string]files{
		"identical":       wf(sec, auto),
		"comments-differ": wf("# Our own words about what this does, and why.\n\n"+stripComments(sec), "# Ditto, at length.\n#\n# Several lines of it.\n"+stripComments(auto)),
		"cron-differs":    wf(cronRe.ReplaceAllString(sec, `cron: "37 2 `), cronRe.ReplaceAllString(auto, `cron: "59 23 `)),
		"pin-differs":     wf(pinRe.ReplaceAllString(sec, "@"+pin), pinRe.ReplaceAllString(auto, "@"+pin)),
		"trigger-missing": wf(sec, dropTrigger),
		"extra-inputs":    wf(extraInputs, auto),
		"extra-block":     wf(extraBlock, auto),
		"absent":          wf(sec, ""),
		"forbidden":       {},
	})
	repos := []string{"identical", "comments-differ", "cron-differs", "pin-differs", "trigger-missing", "extra-inputs", "extra-block", "absent", "forbidden", "repo-gone"}
	res := s.run(t, kitsDir, repos, false)
	if res.err != nil {
		t.Fatal(res.err, res.log)
	}

	// `| repository | auto-release.yaml | golangci-depguard.yaml | security.yaml |`
	for repo, want := range map[string]string{
		"identical":       "| same | same | same |",
		"comments-differ": "| same | same | same |",    // its own prose is not a difference
		"cron-differs":    "| same | same | same |",    // a staggered cron is not a difference
		"pin-differs":     "| same | same | same |",    // a library pin renovate has not moved is not a difference
		"trigger-missing": "| differs | same | same |", // THE DEFECT: a dropped trigger
		"extra-inputs":    "| same | same | differs |", // an ADDED input IS a difference
		"extra-block":     "| same | same | differs |", // an ADDED block and a renamed job ARE
		"absent":          "| absent | same | same |",  // not carried is absent, not differing
		"forbidden":       "| unreadable | unreadable | unreadable |",
		"repo-gone":       "| unreadable | unreadable | unreadable |",
	} {
		if got := res.row(repo); got != "| stub/"+repo+" "+want {
			t.Errorf("%s: %q, want %q", repo, got, "| stub/"+repo+" "+want)
		}
	}
	if !strings.Contains(res.output, "differences=3\n") || !strings.Contains(res.output, "absent=1\n") {
		t.Errorf("output %q", res.output)
	}
	for sub, want := range map[string]int{
		"::warning::stub/trigger-missing: .github/workflows/auto-release.yaml differs from the canonical kit":         1,
		"::warning::stub/forbidden: could not read .github/workflows/auto-release.yaml \\(HTTP 403\\) — not compared": 1,
		"::warning::stub/repo-gone: could not read the repository \\(HTTP 404\\) — not compared":                      1,
		"::warning::stub/absent": 0, // the absent file is silent on the run
	} {
		if got := count(res.log, sub); got != want {
			t.Errorf("%q appears %d times, want %d\n%s", sub, got, want, res.log)
		}
	}
	// The diff is what makes the report actionable, and it is the NORMALISED
	// diff: the missing trigger, not the prose around it, and an addition
	// shows up as an addition on the repository's side.
	for sub, want := range map[string]int{
		"(?m)^-  push:$": 1, "(?m)^-name: Auto Release$": 0,
		"(?m)^\\+      goproxy: ": 1, "(?m)^\\+concurrency:$": 1,
	} {
		if got := count(res.summary, sub); got != want {
			t.Errorf("%q appears %d times in the summary, want %d", sub, got, want)
		}
	}
	if !strings.Contains(res.summary, "--- kits/auto-release.yaml (canonical)\n+++ stub/trigger-missing .github/workflows/auto-release.yaml\n") {
		t.Errorf("diff labels:\n%s", res.summary)
	}

	// Report by default; a gate when the estate asks for one.
	gate := s.run(t, kitsDir, repos, true)
	if gate.err == nil || !strings.Contains(gate.log, "::error::3 caller file(s) differ from the canonical kit\n") {
		t.Errorf("fail-on-diff: %v\n%s", gate.err, gate.log)
	}

	// A kit CAN be turned off in the manifest; the report names it and
	// leaves it out of the table entirely.
	off := t.TempDir()
	for _, n := range []string{"security.yaml", "auto-release.yaml", "golangci-depguard.yaml"} {
		_ = os.WriteFile(filepath.Join(off, n), []byte(kit(t, n)), 0o644)
	}
	_ = os.WriteFile(filepath.Join(off, "kits.yaml"), []byte(strings.Replace(kit(t, "kits.yaml"), "\n  enabled: true\n", "\n  enabled: false\n", 1)), 0o644)
	r := s.run(t, off, []string{"identical"}, false)
	if r.err != nil || strings.Contains(r.summary, "golangci-depguard.yaml |") ||
		!strings.Contains(r.summary, "Kits turned off in `kits/kits.yaml` and NOT compared: golangci-depguard.yaml.\n") {
		t.Errorf("a kit turned off: %v\n%s", r.err, r.summary)
	}
}

func TestBlockKit(t *testing.T) {
	lint := kit(t, "golangci-depguard.yaml")
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "golangci-depguard.yaml"), []byte(lint), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "kits.yaml"), []byte(kit(t, "kits.yaml")), 0o644)

	var frag map[string]any
	if err := yaml.Unmarshal([]byte(lint), &frag); err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(deny []any) []any) string {
		var doc map[string]any
		_ = yaml.Unmarshal([]byte(lint), &doc)
		main := doc["depguard"].(map[string]any)["rules"].(map[string]any)["main"].(map[string]any)
		main["deny"] = f(main["deny"].([]any))
		out, _ := yaml.Marshal(doc)
		return string(out)
	}
	reversed := mutate(func(d []any) []any {
		for i, j := 0, len(d)-1; i < j; i, j = i+1, j-1 {
			d[i], d[j] = d[j], d[i]
		}
		return d
	})
	extra := mutate(func(d []any) []any {
		return append(d, map[string]any{"pkg": "github.com/example/locally-banned", "desc": "banned by this repository, not the estate"})
	})
	missing := regexp.MustCompile(`(?m)^.*pkg: github.com/pkg/errors\n.*\n`).ReplaceAllString(lint, "")
	if missing == lint {
		t.Fatal("the fixture did not drop a ban")
	}
	goMod := "module example.com/stub\n"
	minimal := "version: \"2\"\n\nlinters:\n  enable:\n    - errcheck\n"

	s := newStub(t, map[string]files{
		"lint-identical":                  {"go.mod": goMod, ".golangci.yaml": theirConfig(lint)},
		"lint-reindented":                 {"go.mod": goMod, ".golangci.yaml": "# Our lint configuration. The import bans below are copied from\n# the canonical block and kept in step by hand.\n" + theirConfig(lint)},
		"lint-reordered":                  {"go.mod": goMod, ".golangci.yaml": theirConfig(reversed)},
		"lint-entry-missing":              {"go.mod": goMod, ".golangci.yaml": theirConfig(missing)},
		"lint-extra-rule":                 {"go.mod": goMod, ".golangci.yaml": theirConfig(extra)},
		"lint-block-absent":               {"go.mod": goMod, ".golangci.yaml": minimal},
		"lint-file-absent":                {"go.mod": goMod},
		"lint-not-go":                     {},
		"lint-yml-ext":                    {"go.mod": goMod, ".golangci.yml": theirConfig(lint)},
		"amazon-eks-pod-identity-webhook": {"go.mod": goMod, ".golangci.yaml": minimal},
	})
	repos := []string{"lint-identical", "lint-reindented", "lint-reordered", "lint-entry-missing", "lint-extra-rule", "lint-block-absent", "lint-file-absent", "lint-not-go", "lint-yml-ext", "amazon-eks-pod-identity-webhook"}
	res := s.run(t, dir, repos, false)
	if res.err != nil {
		t.Fatal(res.err, res.log)
	}
	for repo, want := range map[string]string{
		"lint-identical":                  "same", // a verbatim block is at parity
		"lint-reindented":                 "same", // its own comments and indentation are not a difference
		"lint-reordered":                  "same", // its own order is not a difference
		"lint-entry-missing":              "differs",
		"lint-extra-rule":                 "differs", // an ADDED ban IS a difference too
		"lint-block-absent":               "differs", // a file with no block differs; it is not absent
		"lint-file-absent":                "absent",
		"lint-not-go":                     "n/a",  // no go.mod: not absent, not applicable
		"lint-yml-ext":                    "same", // .golangci.yml is found and compared
		"amazon-eks-pod-identity-webhook": "exempt (fork, minimal lint to ease upstream merges)",
	} {
		if got := res.row(repo); got != "| stub/"+repo+" | "+want+" |" {
			t.Errorf("%s: %q, want state %q", repo, got, want)
		}
	}
	if !strings.Contains(res.output, "differences=3\n") || !strings.Contains(res.output, "absent=1\n") {
		t.Errorf("output %q", res.output)
	}
	// The warning names the path the manifest gave, not the default one.
	if count(res.log, "::warning::stub/lint-entry-missing: .golangci.yaml differs from the canonical kit") != 1 {
		t.Errorf("log:\n%s", res.log)
	}
	// The diff is over the DATA, so it names the ban rather than a line of YAML.
	d := res.diffFor("lint-entry-missing")
	if count(d, `(?m)^-.*"pkg"`) != 1 || count(d, `(?m)^-.*github.com/pkg/errors`) != 1 || count(d, `(?m)^\+.*"pkg"`) != 0 {
		t.Errorf("missing ban:\n%s", d)
	}
	if n := count(res.diffFor("lint-block-absent"), `(?m)^-.*"pkg"`); n <= 5 {
		t.Errorf("a repository with no block loses every ban, lost %d", n)
	}
	d = res.diffFor("lint-extra-rule")
	if count(d, `(?m)^\+.*"pkg"`) != 1 || count(d, `(?m)^\+.*github.com/example/locally-banned`) != 1 || count(d, `(?m)^-.*"pkg"`) != 0 {
		t.Errorf("extra ban:\n%s", d)
	}
}

func TestSubstanceAndSubtree(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"comments and blanks", "a: 1 # c\n\n  # whole line\nb: 2\n", "a: 1\nb: 2\n"},
		{"cron", "  - cron: \"1 2 * * *\"\n  -cron: x\non: y\n", "on: y\n"},
		{"library pin", "    uses: o/r/.github/workflows/w.yaml@0123456789abcdef0123456789abcdef01234567 # v1\n", "    uses: o/r/.github/workflows/w.yaml@<pinned>\n"},
		{"third-party pin is compared", "    uses: actions/checkout@0123456789abcdef0123456789abcdef01234567\n", "    uses: actions/checkout@0123456789abcdef0123456789abcdef01234567\n"},
		{"no final newline is kept", "a: 1\nb: 2", "a: 1\nb: 2"},
		{"empty", "", ""},
	} {
		if got := string(substance([]byte(tc.in))); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	doc := []byte("x:\n  y:\n    - {b: 2, a: [3, 1]}\n    - null\n    - z\n    - 1\n")
	want := "[\n  null,\n  1,\n  \"z\",\n  {\n    \"a\": [\n      1,\n      3\n    ],\n    \"b\": 2\n  }\n]\n"
	if got := string(subtree(doc, ".x.y")); got != want {
		t.Errorf("subtree:\n%s\nwant:\n%s", got, want)
	}
	for _, p := range []string{".x.nope", ".x.y.z.w", ".q"} {
		if got := string(subtree(doc, p)); got != "null\n" {
			t.Errorf("%s: %q", p, got)
		}
	}
	if got := string(subtree([]byte("a: [unclosed"), ".a")); got != "null\n" {
		t.Errorf("invalid yaml: %q", got)
	}
	if !strings.HasPrefix(string(subtree(doc, ".")), "{\n  \"x\"") {
		t.Error("root path")
	}
}

func TestEdges(t *testing.T) {
	s := newStub(t, nil)
	// nothing to check
	for _, repos := range []string{"", "[]", "{}", "nope"} {
		var b strings.Builder
		dir := t.TempDir()
		err := Run(context.Background(), Options{Repositories: repos, Kits: kitsDir, API: s.srv.URL, Out: &b,
			Summary: filepath.Join(dir, "s"), Output: filepath.Join(dir, "o")})
		out, _ := os.ReadFile(filepath.Join(dir, "o"))
		if err != nil || b.String() != "no repositories to check\n" || string(out) != "differences=0\nabsent=0\n" {
			t.Errorf("%q: %v %q %q", repos, err, b.String(), out)
		}
	}
	// no kits
	var b strings.Builder
	err := Run(context.Background(), Options{Repositories: `["a/b"]`, Kits: t.TempDir(), API: s.srv.URL, Out: &b})
	if err == nil || b.String() == "" || !strings.HasPrefix(b.String(), "::error::no kit files in ") {
		t.Errorf("%v %q", err, b.String())
	}
	// every file absent says the token may not be able to read contents
	r := newStub(t, map[string]files{"bare": {"go.mod": "module x\n"}})
	res := r.run(t, kitsDir, []string{"bare"}, false)
	if !strings.Contains(res.log, "::warning::every file is reported absent — check that the token carries contents: read\n") {
		t.Errorf("%s", res.log)
	}
}

// A kit that is for one kind of repository says so by naming a DIRECTORY the
// repository must carry: `applies_if: pins/`. With the trailing slash the
// answer must be a directory, so a file that happens to be called `pins` is
// not mistaken for one. Without it the old rule stands: any file or directory.
func TestKitAppliesIfADirectory(t *testing.T) {
	const caller = "name: check\non:\n  pull_request:\n    branches: [master]\njobs:\n  check:\n    uses: o/r/.github/workflows/check.yaml@0123456789abcdef0123456789abcdef01234567 # v1\n    with:\n      company-kind: delivery\n"
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "company-delivery.yaml"), []byte(caller), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "kits.yaml"), []byte("company-delivery.yaml:\n  path: .github/workflows/check.yaml\n  applies_if: pins/\n"), 0o644)
	pin := ".github/workflows/check.yaml"
	s := newStub(t, map[string]files{
		"delivery-same":     {"pins/acme/devel.yaml": "pins: {}\n", pin: caller},
		"delivery-differs":  {"pins/acme/devel.yaml": "pins: {}\n", pin: strings.Replace(caller, "delivery", "config", 1)},
		"delivery-no-check": {"pins/acme/devel.yaml": "pins: {}\n"},
		"other-kind":        {"tenant/project.yaml": "x: 1\n", pin: caller},
		"file-named-pins":   {"pins": "not a directory\n", pin: caller},
		"empty":             {},
	})
	repos := []string{"delivery-same", "delivery-differs", "delivery-no-check", "other-kind", "file-named-pins", "empty"}
	res := s.run(t, dir, repos, false)
	if res.err != nil {
		t.Fatal(res.err, res.log)
	}
	for repo, want := range map[string]string{
		"delivery-same":     "same",
		"delivery-differs":  "differs",
		"delivery-no-check": "absent",
		"other-kind":        "n/a", // no pins/: the kit has no opinion
		"file-named-pins":   "n/a", // a file is not a directory
		"empty":             "n/a",
	} {
		if got := res.row(repo); got != "| stub/"+repo+" | "+want+" |" {
			t.Errorf("%s: %q, want state %q", repo, got, want)
		}
	}

	// Without the slash, a file satisfies applies_if, as it always did.
	_ = os.WriteFile(filepath.Join(dir, "kits.yaml"), []byte("company-delivery.yaml:\n  path: .github/workflows/check.yaml\n  applies_if: pins\n"), 0o644)
	res = s.run(t, dir, []string{"file-named-pins", "delivery-same", "other-kind"}, false)
	for repo, want := range map[string]string{"file-named-pins": "same", "delivery-same": "same", "other-kind": "n/a"} {
		if got := res.row(repo); got != "| stub/"+repo+" | "+want+" |" {
			t.Errorf("plain applies_if, %s: %q, want state %q", repo, got, want)
		}
	}
}

// The company kits ship switched off, so nothing else reads them. This does:
// they are what a company copies verbatim, so they must be the shape the rest
// of the estate expects, carry nothing of one company, and sort a repository
// into the right kind.
func TestCompanyKits(t *testing.T) {
	manifestBytes := []byte(kit(t, "kits.yaml"))
	var man map[string]map[string]any
	if err := yaml.Unmarshal(manifestBytes, &man); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ file, kind, dir string }{
		{"company-delivery-check.yaml", "delivery", "pins/"},
		{"company-config-check.yaml", "config", "tenant/"},
	} {
		t.Run(c.kind, func(t *testing.T) {
			text := kit(t, c.file)
			var k struct {
				Name        string            `yaml:"name"`
				On          map[string]any    `yaml:"on"`
				Permissions map[string]string `yaml:"permissions"`
				Jobs        map[string]struct {
					Uses    string            `yaml:"uses"`
					With    map[string]string `yaml:"with"`
					Needs   any               `yaml:"needs"`
					If      string            `yaml:"if"`
					Secrets any               `yaml:"secrets"`
					Steps   []map[string]any  `yaml:"steps"`
				} `yaml:"jobs"`
			}
			if err := yaml.Unmarshal([]byte(text), &k); err != nil {
				t.Fatal(err)
			}
			if k.Name != "check" {
				t.Errorf("name %q: the fan-in context the ruleset requires is `check`", k.Name)
			}
			if _, ok := k.On["pull_request"]; !ok || len(k.On) != 1 {
				t.Errorf("on: %v, want pull_request alone (never pull_request_target)", k.On)
			}
			if k.Permissions["contents"] != "read" || k.Permissions["id-token"] != "write" || len(k.Permissions) != 2 {
				t.Errorf("permissions: %v", k.Permissions)
			}
			pin := regexp.MustCompile(`^truvity/ci-workflows/\.github/workflows/check\.yaml@[0-9a-f]{40}$`)
			call := k.Jobs["company"]
			if !pin.MatchString(call.Uses) || call.Secrets != nil {
				t.Errorf("company job: uses %q secrets %v", call.Uses, call.Secrets)
			}
			if call.With["company-kind"] != c.kind {
				t.Errorf("company-kind %q, want %q", call.With["company-kind"], c.kind)
			}
			for in, v := range call.With {
				if in != "company-kind" && !regexp.MustCompile(`^\$\{\{ vars\.[A-Z_]+ \}\}$`).MatchString(v) {
					t.Errorf("input %s is %q: a company's own setting belongs in a repository variable", in, v)
				}
			}
			fan := k.Jobs["check"]
			if fan.Needs != "company" || fan.If != "always()" || len(fan.Steps) != 1 || !strings.Contains(fan.Steps[0]["run"].(string), `= "success"`) {
				t.Errorf("fan-in job: %+v", fan)
			}
			if len(k.Jobs) != 2 {
				t.Errorf("jobs: %d, want the call and its fan-in", len(k.Jobs))
			}
			if m := man[c.file]; m["path"] != ".github/workflows/check.yaml" || m["applies_if"] != c.dir || m["enabled"] != false {
				t.Errorf("manifest entry: %v", m)
			}
		})
	}
	for name := range man {
		if _, err := os.Stat(filepath.Join(kitsDir, name)); err != nil {
			t.Errorf("kits.yaml names %s, which is not a kit file", name)
		}
	}

	// Switched on, a verbatim copy is at parity in the right kind of
	// repository, and the other kind and a plain repository are n/a.
	dir := t.TempDir()
	for _, n := range []string{"company-delivery-check.yaml", "company-config-check.yaml"} {
		_ = os.WriteFile(filepath.Join(dir, n), []byte(kit(t, n)), 0o644)
	}
	on := strings.ReplaceAll(string(manifestBytes), "enabled: false", "enabled: true")
	_ = os.WriteFile(filepath.Join(dir, "kits.yaml"), []byte(on), 0o644)
	wf := ".github/workflows/check.yaml"
	s := newStub(t, map[string]files{
		"delivery":       {"pins/acme/devel.yaml": "pins: {}\n", wf: kit(t, "company-delivery-check.yaml")},
		"config":         {"tenant/project.yaml": "x: 1\n", wf: kit(t, "company-config-check.yaml")},
		"delivery-stale": {"pins/acme/devel.yaml": "pins: {}\n", wf: kit(t, "company-config-check.yaml")},
		"plain":          {"go.mod": "module x\n"},
	})
	res := s.run(t, dir, []string{"delivery", "config", "delivery-stale", "plain"}, false)
	if res.err != nil {
		t.Fatal(res.err, res.log)
	}
	for repo, want := range map[string]string{
		"delivery":       "same | n/a",
		"config":         "n/a | same",
		"delivery-stale": "differs | n/a",
		"plain":          "n/a | n/a",
	} {
		// columns are the kit files in name order: config, then delivery
		parts := strings.Split(want, " | ")
		if got := res.row(repo); got != "| stub/"+repo+" | "+parts[1]+" | "+parts[0]+" |" {
			t.Errorf("%s: %q, want config %s, delivery %s", repo, got, parts[1], parts[0])
		}
	}
}
