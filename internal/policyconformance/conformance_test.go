package policyconformance

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The harness starts from a repository that meets every rule, proves each rule
// says PASS there, then breaks one rule at a time and proves THAT rule, and
// only that rule, says FAIL. A conformance check that reports PASS for
// everything is indistinguishable from one that looks at nothing.
//
// No network and no token: `origin` is a bare repository beside the fixture,
// and the tag C5 looks for is pushed there. The estate facts the C13 cases
// plant are assembled from pieces, since this file is itself tracked by a
// public repository whose own canary and ticket rule would match them.
var (
	org = "truv" + "ity"
	key = "IN" + "F-4242"
)

type fx struct {
	t   *testing.T
	dir string
}

func (f *fx) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=case", "GIT_AUTHOR_EMAIL=case@example.invalid",
		"GIT_COMMITTER_NAME=case", "GIT_COMMITTER_EMAIL=case@example.invalid", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (f *fx) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) appendTo(rel, content string) {
	f.t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, rel))
	f.write(rel, string(b)+content)
}

func (f *fx) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, rel))
	return string(b)
}

// sub is `sed -i 's/re/to/'` on every line (first match per line, like sed).
func (f *fx) sub(rel, re, to string) {
	f.t.Helper()
	rx := regexp.MustCompile(re)
	ls := strings.Split(f.read(rel), "\n")
	for i, l := range ls {
		if loc := rx.FindStringSubmatchIndex(l); loc != nil {
			ls[i] = l[:loc[0]] + string(rx.ExpandString(nil, to, l, loc)) + l[loc[1]:]
		}
	}
	f.write(rel, strings.Join(ls, "\n"))
}

func (f *fx) remove(rel string) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.dir, rel)); err != nil {
		f.t.Fatal(err)
	}
}

// tag commits on master, pushes it and a new annotated tag, and fetches.
func (f *fx) tag(name string) {
	f.t.Helper()
	f.git("commit", "-q", "--allow-empty", "-m", "next")
	f.git("push", "-q", "origin", "master")
	f.tagOnly(name)
}

func (f *fx) tagOnly(name string) {
	f.t.Helper()
	f.git("tag", "-a", name, "-m", name)
	f.git("push", "-q", "origin", name)
	f.git("fetch", "-q", "origin")
}

var headings = []string{"Who it is for", "The model", "Install and a worked example", "Consumers",
	"Neighbours", "Documentation", "The rule that makes this repository public", "Status", "Development", "Releasing", "Licence"}

// conformant builds a repository that meets C1-C13.
func conformant(t *testing.T) *fx {
	t.Helper()
	root := t.TempDir()
	f := &fx{t: t, dir: filepath.Join(root, "widget")}
	f.write("charts/widget/Chart.yaml", "apiVersion: v2\nname: widget\nversion: 0.0.0\nappVersion: \"0.0.0\"\n")
	f.write("charts/widget/values.schema.json", "{}\n")
	f.write("charts/widget/values.yaml", "image:\n  repository: ghcr.io/example/widget/server\n")
	f.write("tests/golden/widget/default.yaml", "kind: Deployment\n")
	f.write("tests/invalid/widget/negative.yaml", "replicas: -1\n")
	f.write("hack/leak-canary.sh", "#!/usr/bin/env bash\n")
	f.write("Justfile", "check:\n    ./hack/leak-canary.sh\n")
	f.write("CHANGELOG.md", "# Changelog\n\n## Unreleased\n\n## v1.1.0\n\n## v1.0.0\n")
	f.write("devbox.json", `{"packages": ["go@1.25.1", "just@1.40.0"]}`+"\n")
	f.write("renovate.json", `{"$schema": "https://docs.renovatebot.com/renovate-schema.json", "extends": ["github>truvity/ci-workflows"]}`+"\n")
	var readme strings.Builder
	readme.WriteString("# widget\n")
	for _, h := range headings {
		fmt.Fprintf(&readme, "\n## %s\n\ntext\n", h)
	}
	readme.WriteString("\n```sh\ngo install example.invalid/widget@v1.1.0\n```\n")
	f.write("README.md", readme.String())
	f.write("LICENSE", "MIT License\n\nPermission is hereby granted, free of charge, ...\n")
	f.write("go.mod", "module example.invalid/widget\n")
	f.write(".github/workflows/security.yaml", "jobs:\n  govulncheck:\n    uses: truvity/ci-workflows/.github/workflows/check.yaml@x\n    with:\n      recipes: '[\"vuln\"]'\n")
	f.write(".github/workflows/ci.yaml", "jobs:\n  check:\n    with:\n      recipes: '[\"build\", \"test\"]'\n")
	f.write(".github/workflows/release.yaml", "jobs:\n  release:\n    with:\n      ko-docker-repo: ghcr.io/example/widget\n")
	f.write(".goreleaser.yaml", "builds:\n  - id: server\n    main: ./cmd/server\n")
	f.git("init", "-q", "-b", "master", ".")
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "init")
	f.git("tag", "-a", "v1.1.0", "-m", "v1.1.0")
	f.git("clone", "-q", "--bare", ".", f.dir+".git")
	f.git("remote", "add", "origin", f.dir+".git")
	f.git("fetch", "-q", "origin")
	return f
}

// run: the rules read tracked files, so what an edit created is staged first,
// as it would be by the commit a real change ends in.
func (f *fx) run(o Options) (string, error, string) {
	f.t.Helper()
	f.git("add", "-A")
	var out bytes.Buffer
	o.Out = &out
	o.Dir = f.dir
	if o.GithubRepository == "" {
		o.GithubRepository = "example/widget"
	}
	err := Run(context.Background(), o)
	return out.String(), err, ""
}

func line(log, id string) string {
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, id+" ") {
			return l
		}
	}
	return ""
}

func (f *fx) expect(log, id, want string) {
	f.t.Helper()
	if l := line(log, id); !strings.HasPrefix(l, id+" "+want+":") {
		f.t.Errorf("%s: want %s, got %q", id, want, l)
	}
}

func TestBaseline(t *testing.T) {
	f := conformant(t)
	log, err, _ := f.run(Options{})
	if err != nil {
		t.Fatal(err, log)
	}
	for _, id := range Rules {
		f.expect(log, id, "PASS")
	}
	if !strings.Contains(log, "heading for the latest tag v1.1.0 present") {
		t.Errorf("C5 found the latest tag through origin: %s", line(log, "C5"))
	}
	if !strings.HasSuffix(log, "policy-conformance: every evaluated rule passed\n") {
		t.Errorf("tail: %q", log[len(log)-80:])
	}
}

type edit func(f *fx)

type rcase struct {
	id, what string
	edit     edit
	want     string // PASS (default), FAIL or EXEMPT
	needle   string // text the rule's line must carry
}

func writePair(f *fx, name string) {
	f.write("charts/"+name+"/values.schema.json", "{}\n")
	f.write("tests/golden/"+name+"/default.yaml", "kind: Deployment\n")
	f.write("tests/invalid/"+name+"/negative.yaml", "replicas: -1\n")
}

var mirrorHdr = "annotations:\n  truvity.io/mirror: \"acme/widget@1.2.3\"\n"

var pairFix = func(f *fx) {
	f.write("internal/s3test/r.go", "package s3test\n\nconst region = \"eu-west-1\"\n")
	f.write("catalogue/schemaid.go", "package catalogue\n\nconst host = \"a."+org+".com\"\n")
}

const pairYAML = "exempt:\n  C13:\n    - checks: [region]\n      paths: [internal/s3test/**]\n      reason: a worked example\n    - checks: [domain]\n      paths: [catalogue/schemaid.go]\n      reason: a schema identifier\n"

var rulecases = []rcase{
	{"C1", "a chart committing a real version", func(f *fx) { f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 0.1.0") }, "FAIL", "version is 0.1.0"},
	{"C1", "the retired 0.0.0-dev placeholder", func(f *fx) { f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 0.0.0-dev") }, "FAIL", "0.0.0-dev"},
	{"C1", "an appVersion that is not the placeholder", func(f *fx) { f.sub("charts/widget/Chart.yaml", `^appVersion: .*`, "appVersion: 1.2.3") }, "FAIL", "appVersion is 1.2.3"},
	{"C2", "a chart with no values schema", func(f *fx) { f.remove("charts/widget/values.schema.json") }, "FAIL", "values.schema.json"},
	{"C3", "a chart with no negative fixture", func(f *fx) { f.remove("tests/invalid") }, "FAIL", "tests/invalid/widget"},
	{"C4", "no Justfile", func(f *fx) { f.remove("Justfile") }, "FAIL", "no Justfile"},
	{"C4", "a Justfile that never runs the canary", func(f *fx) { f.write("Justfile", "check:\n") }, "FAIL", "never mentions"},
	{"C5", "no heading for the latest tag", func(f *fx) { f.tag("v1.2.0") }, "FAIL", "no heading for v1.2.0"},
	{"C5", "no heading for the higher of two tags on one commit", func(f *fx) { f.tagOnly("v1.2.0") }, "FAIL", "no heading for v1.2.0"},
	{"C5", "headings oldest first", func(f *fx) { f.write("CHANGELOG.md", "## v1.0.0\n\n## v1.1.0\n") }, "FAIL", "newest first"},
	{"C5", "a Keep-a-Changelog style heading", func(f *fx) { f.write("CHANGELOG.md", "## [1.1.0] - 2026-09-29\n") }, "FAIL", "not in the form"},
	{"C5", "two Unreleased headings", func(f *fx) { f.write("CHANGELOG.md", "## Unreleased\n\n## Unreleased\n\n## v1.1.0\n") }, "FAIL", "at most one"},
	{"C5", "a patch of a line no heading carries is hand-cut and needs its heading", func(f *fx) { f.tag("v1.2.1") }, "FAIL", "not an automatic patch"},
	{"C6", "a package on latest", func(f *fx) { f.write("devbox.json", `{"packages": {"go": "latest", "just": "1.40.0"}}`+"\n") }, "FAIL", "pinned to latest: go"},
	{"C6", "a package with no version", func(f *fx) { f.write("devbox.json", `{"packages": ["go"]}`+"\n") }, "FAIL", "no version: go"},
	{"C7", "a renovate.json that restates the preset", func(f *fx) { f.write("renovate.json", `{"$schema": "x", "extends": ["config:recommended"]}`+"\n") }, "FAIL", "extends is"},
	{"C7", "an undocumented override", func(f *fx) {
		f.write("renovate.json", `{"$schema": "x", "extends": ["github>truvity/ci-workflows"], "automerge": false}`+"\n")
	}, "FAIL", "no top-level description"},
	{"C8", "a missing heading", func(f *fx) { f.sub("README.md", `^## Neighbours$`, "") }, "FAIL", "'Neighbours'"},
	{"C8", "two headings swapped", func(f *fx) {
		f.sub("README.md", `^## Status$`, "## TMP")
		f.sub("README.md", `^## Development$`, "## Status")
		f.sub("README.md", `^## TMP$`, "## Development")
	}, "FAIL", "comes before"},
	{"C9", "a licence that is not MIT", func(f *fx) { f.write("LICENSE", "Apache License\n") }, "FAIL", "not the MIT"},
	{"C10", "a Go repository with no security.yaml", func(f *fx) { f.remove(".github/workflows/security.yaml") }, "FAIL", "security.yaml is missing"},
	{"C10", "the check recipe depending on vuln", func(f *fx) { f.write("Justfile", "vuln:\n    true\ncheck: vuln\n    ./hack/leak-canary.sh\n") }, "FAIL", "the check recipe depends on vuln"},
	{"C10", "check reaching vuln through another recipe", func(f *fx) {
		f.write("Justfile", "vuln:\n    true\nci: vuln\n    true\ncheck: ci\n    ./hack/leak-canary.sh\n")
	}, "FAIL", "recipe ci depends on vuln"},
	{"C10", "check calling just vuln from its body", func(f *fx) {
		f.write("Justfile", "vuln:\n    true\ncheck:\n    ./hack/leak-canary.sh\n    just vuln\n")
	}, "FAIL", "runs `just vuln`"},
	{"C10", "vuln in the merge gate", func(f *fx) { f.sub(".github/workflows/ci.yaml", `"test"`, `"test", "vuln"`) }, "FAIL", "ci.yaml runs vuln"},
	{"C11", "an image named after the repository twice", func(f *fx) {
		f.sub(".goreleaser.yaml", `cmd/server`, "cmd/widget")
		f.sub("charts/widget/values.yaml", `widget/server`, "widget/widget")
	}, "FAIL", "ghcr.io/example/widget/widget"},
	{"C12", "an @latest install", func(f *fx) {
		f.appendTo("README.md", "\n```sh\ngo install example.invalid/widget@latest\n```\n")
	}, "FAIL", "@latest"},

	{"C12", "@latest named only in prose, outside a fenced block, is not an install", func(f *fx) {
		f.appendTo("README.md", "\nPin a real version -- an OCI reference has no `@latest` tag to fall back to.\n")
	}, "PASS", ""},

	// C1 · a mirror chart carries the upstream's version
	{"C1", "a declared mirror chart carries the upstream version", func(f *fx) {
		f.appendTo("charts/widget/Chart.yaml", mirrorHdr)
		f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 1.2.3")
		f.sub("charts/widget/Chart.yaml", `^appVersion: .*`, `appVersion: "1.2.3"`)
	}, "PASS", "mirror chart(s) at the upstream version"},
	{"C1", "a chart with a real version and no mirror declaration", func(f *fx) { f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 1.2.3") }, "FAIL", "version is 1.2.3"},
	{"C1", "a mirror chart whose version differs from the mirrored one", func(f *fx) {
		f.appendTo("charts/widget/Chart.yaml", mirrorHdr)
		f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 1.2.4")
	}, "FAIL", "not the mirrored 1.2.3"},
	{"C1", "a mirror chart whose appVersion differs from the mirrored version", func(f *fx) {
		f.appendTo("charts/widget/Chart.yaml", mirrorHdr)
		f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 1.2.3")
		f.sub("charts/widget/Chart.yaml", `^appVersion: .*`, "appVersion: 0.0.0")
	}, "FAIL", "appVersion is 0.0.0, not the mirrored 1.2.3"},
	{"C1", "a mirror annotation that is not owner/repo@version", func(f *fx) {
		f.appendTo("charts/widget/Chart.yaml", "annotations:\n  truvity.io/mirror: \"1.2.3\"\n")
		f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 1.2.3")
	}, "FAIL", "not <owner>/<repo>@<version>"},

	// C1 · a chart-only repository judges no appVersion
	{"C1", "appVersion is not judged when every goreleaser build is skipped", func(f *fx) {
		f.write(".goreleaser.yaml", "builds:\n  - skip: true\n")
		f.sub("charts/widget/Chart.yaml", `^appVersion: .*`, `appVersion: "9.9.9"`)
	}, "PASS", ""},

	// exemptions: .github/policy-conformance.yaml
	{"C1", "an exempted chart's version is not judged", func(f *fx) {
		f.sub("charts/widget/Chart.yaml", `^version: 0.0.0`, "version: 1.2.3-upstream")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C1:\n    reason: CRD chart, stamped from upstream's pin\n    charts: [widget]\n")
	}, "PASS", "(exempt: widget: CRD chart, stamped from upstream's pin)"},
	{"C1", "an exemption named for one chart does not cover a second", func(f *fx) {
		f.write("charts/other/Chart.yaml", f.read("charts/widget/Chart.yaml"))
		writePair(f, "other")
		f.sub("charts/other/Chart.yaml", `^version: 0.0.0`, "version: 1.2.3")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C1:\n    reason: only widget is exempt\n    charts: [widget]\n")
	}, "FAIL", "charts/other version is 1.2.3"},
	{"C2", "an exempted chart's missing schema is not judged", func(f *fx) {
		f.remove("charts/widget/values.schema.json")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C2:\n    reason: library chart takes no values\n    charts: [widget]\n")
	}, "PASS", ""},
	{"C5", "an exemption suppresses only the missing-latest-heading failure", func(f *fx) {
		f.write(".github/policy-conformance.yaml", "exempt:\n  C5:\n    reason: dependency-only patches carry no heading\n")
		f.tag("v1.2.0")
	}, "PASS", "exempt: dependency-only patches carry no heading"},
	{"C5", "an exemption does not excuse a genuinely unordered CHANGELOG", func(f *fx) {
		f.write(".github/policy-conformance.yaml", "exempt:\n  C5:\n    reason: dependency-only patches carry no heading\n")
		f.write("CHANGELOG.md", "## v1.0.0\n\n## v1.1.0\n")
	}, "FAIL", "newest first"},
	{"C9", "an exempted fork's non-MIT licence is EXEMPT, not FAIL", func(f *fx) {
		f.write("LICENSE", "Apache License\n")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C9:\n    reason: fork of an Apache-2.0 upstream\n")
	}, "EXEMPT", "fork of an Apache-2.0 upstream"},

	// C5 · an automatic patch needs no heading of its own
	{"C5", "vX.Y.Z with Z > 0 and the newest heading's X.Y is an automatic patch", func(f *fx) { f.tag("v1.1.1") }, "PASS", "automatic patch of v1.1.0"},
	{"C5", "a patch whose own heading exists passes as a hand-cut tag", func(f *fx) {
		f.write("CHANGELOG.md", "## Unreleased\n\n## v1.1.1\n\n## v1.1.0\n\n## v1.0.0\n")
		f.tag("v1.1.1")
	}, "PASS", "heading for the latest tag v1.1.1 present"},
	{"C5", "vX.Y.0 is always hand-cut, even one minor past the newest heading", func(f *fx) { f.tag("v1.2.0") }, "FAIL", "no heading for v1.2.0"},
	{"C5", "a major bump patch (v2.0.1, newest heading v1.1.0) is not automatic", func(f *fx) { f.tag("v2.0.1") }, "FAIL", "not an automatic patch"},

	// C10 · look-alikes that are not check reaching vuln
	{"C10", "a vuln recipe that check does not reach, and a comment naming it", func(f *fx) {
		f.write("Justfile", "# check: vuln would be wrong\nvuln:\n    govulncheck ./...\ncheck: build\n    ./hack/leak-canary.sh\nbuild:\n    just build-all\n")
	}, "PASS", ""},
	{"C10", "just vuln-report in a body is another recipe, not vuln", func(f *fx) {
		f.write("Justfile", "check:\n    ./hack/leak-canary.sh\n    just vuln-report\n")
	}, "PASS", ""},

	// C13 · estate facts are inputs, never defaults
	{"C13", "an organisation domain as a chart default", func(f *fx) { f.appendTo("charts/widget/values.yaml", "host: internal."+org+".com\n") }, "FAIL", "values.yaml:3 (domain)"},
	{"C13", "the tenancy API group as a chart default", func(f *fx) { f.appendTo("charts/widget/values.yaml", "group: tenancy."+org+".io\n") }, "FAIL", "values.yaml:3 (tenancy)"},
	{"C13", "a real environment name as the default of an env key", func(f *fx) { f.appendTo("charts/widget/values.yaml", "env: prod\n") }, "FAIL", "values.yaml:3 (env)"},
	{"C13", "a real cluster name, quoted, as the default of a cluster key", func(f *fx) { f.appendTo("charts/widget/values.yaml", "cluster: \"kernel\"\n") }, "FAIL", "values.yaml:3 (env)"},
	{"C13", "a real region as a chart default", func(f *fx) { f.appendTo("charts/widget/values.yaml", "bucketRegion: eu-west-1\n") }, "FAIL", "values.yaml:3 (region)"},
	{"C13", "a real region inside a longer chart default", func(f *fx) {
		f.appendTo("charts/widget/values.yaml", "endpoint: s3.us-east-2.amazonaws.example\n")
	}, "FAIL", "values.yaml:3 (region)"},
	{"C13", "an environment name as a schema default", func(f *fx) {
		f.write("charts/widget/values.schema.json", "{\n  \"properties\": {\n    \"env\": {\n      \"type\": \"string\",\n      \"default\": \"devel\"\n    }\n  }\n}\n")
	}, "FAIL", "values.schema.json:5 (env)"},
	{"C13", "a Go constant naming a real cluster", func(f *fx) { f.write("cmd/server/main.go", "package main\n\nconst defaultCluster = \"kernel\"\n") }, "FAIL", "main.go:3 (env)"},
	{"C13", "a Go flag defaulting to a real region", func(f *fx) {
		f.write("cmd/server/main.go", "package main\n\nvar region = fs.String(\"region\", \"us-east-1\", \"\")\n")
	}, "FAIL", "main.go:3 (region)"},
	{"C13", "a TypeScript constant holding an organisation domain", func(f *fx) {
		f.write("src/config.ts", "export const HOST = \"api."+org+".xyz\";\n")
	}, "FAIL", "config.ts:1 (domain)"},
	{"C13", "a ticket key in the README", func(f *fx) { f.appendTo("README.md", "\nSee "+key+".\n") }, "FAIL", "README.md:"},
	{"C13", "a ticket key in the CHANGELOG is no exception", func(f *fx) { f.appendTo("CHANGELOG.md", "- fixes "+key+"\n") }, "FAIL", "CHANGELOG.md:"},
	{"C13", "a ticket key in code", func(f *fx) { f.write("cmd/server/main.go", "package main\n\n// see "+key+"\n") }, "FAIL", "main.go:3 (ticket)"},

	// What is not an estate fact must stay quiet.
	{"C13", "neutral placeholders in chart defaults", func(f *fx) {
		f.appendTo("charts/widget/values.yaml", "host: app.example.com\nregion: eu-example-1\nenv: \"\"\ncluster: \"\"\n")
	}, "PASS", ""},
	{"C13", "a real region or environment named only in a comment", func(f *fx) {
		f.appendTo("charts/widget/values.yaml", "# e.g. eu-west-1, or prod for "+org+".com\nregion: \"\" # eu-west-1 in our case\n")
	}, "PASS", ""},
	{"C13", "a Go comparison, case label and list of names are tests of a name, not defaults", func(f *fx) {
		f.write("cmd/server/main.go", "package main\n\nfunc f(env string) bool {\n\tswitch env {\n\tcase \"prod\":\n\t\treturn true\n\t}\n\tenvs := []string{\"devel\", \"prod\"}\n\t_ = envs\n\treturn env == \"kernel\"\n}\n")
	}, "PASS", ""},
	{"C13", "Go test files may name anything", func(f *fx) { f.write("cmd/server/main_test.go", "package main\n\nconst defaultCluster = \"kernel\"\n") }, "PASS", ""},
	{"C13", "a values.yaml that is not a chart's is not read", func(f *fx) { f.write("docs/values.yaml", "env: prod\n") }, "PASS", ""},
	{"C13", "a URL that merely contains the organisation's name as a path is not a domain", func(f *fx) {
		f.write("cmd/server/main.go", "package main\n\nconst repo = \"https://github.com/"+org+"/widget\"\n")
	}, "PASS", ""},

	// C13 · exemptions
	{"C13", "an exemption for one check suppresses that check", func(f *fx) {
		f.appendTo("charts/widget/values.yaml", "bucketRegion: eu-west-1\n")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    reason: this chart documents one region as a worked example\n    checks: [region]\n")
	}, "PASS", "(1 finding(s) exempt: this chart documents one region as a worked example)"},
	{"C13", "an exemption for one check does not cover another", func(f *fx) {
		f.appendTo("charts/widget/values.yaml", "bucketRegion: eu-west-1\nhost: internal."+org+".com\n")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    reason: this chart documents one region as a worked example\n    checks: [region]\n")
	}, "FAIL", "values.yaml:4 (domain)"},
	{"C13", "an exemption scoped to a path covers that path", func(f *fx) {
		f.write("HISTORY.md", "See "+key+".\n")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    reason: a history file that quotes tickets\n    checks: [ticket]\n    paths: [HISTORY.md]\n")
	}, "PASS", ""},
	{"C13", "an exemption scoped to a path does not cover another", func(f *fx) {
		f.write("HISTORY.md", "See "+key+".\n")
		f.appendTo("README.md", "\nSee "+key+".\n")
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    reason: a history file that quotes tickets\n    checks: [ticket]\n    paths: [HISTORY.md]\n")
	}, "FAIL", "README.md:"},
	{"C13", "paired entries each cover their own check and path", func(f *fx) { pairFix(f); f.write(".github/policy-conformance.yaml", pairYAML) }, "PASS", ""},
	{"C13", "paired entries do not widen: a check outside its pair still fails", func(f *fx) {
		pairFix(f)
		f.appendTo("catalogue/schemaid.go", "const regionB = \"eu-west-1\"\n")
		f.write(".github/policy-conformance.yaml", pairYAML)
	}, "FAIL", "catalogue/schemaid.go:4 (region)"},
	{"C13", "paired entries do not widen: the other path still fails", func(f *fx) {
		pairFix(f)
		f.appendTo("internal/s3test/r.go", "const host2 = \"b."+org+".com\"\n")
		f.write(".github/policy-conformance.yaml", pairYAML)
	}, "FAIL", "internal/s3test/r.go:4 (domain)"},
	{"C13", "the older single block still works", func(f *fx) {
		pairFix(f)
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    reason: both are deliberate\n    checks: [region, domain]\n    paths: [internal/s3test/**, catalogue/schemaid.go]\n")
	}, "PASS", ""},
	{"C13", "double-quoted paths and checks are matched", func(f *fx) {
		pairFix(f)
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    - checks: [\"region\"]\n      paths: [\"internal/s3test/**\"]\n      reason: a worked example\n    - checks: [\"domain\"]\n      paths: [\"catalogue/schemaid.go\"]\n      reason: a schema identifier\n")
	}, "PASS", ""},
	{"C13", "single-quoted paths are matched", func(f *fx) {
		pairFix(f)
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    - checks: ['region']\n      paths: ['internal/s3test/**']\n      reason: a worked example\n    - checks: ['domain']\n      paths: ['catalogue/schemaid.go']\n      reason: a schema identifier\n")
	}, "PASS", ""},
	{"C13", "a list entry with no reason is refused, and exempts nothing", func(f *fx) {
		pairFix(f)
		f.write(".github/policy-conformance.yaml", "exempt:\n  C13:\n    - checks: [region]\n      paths: [internal/s3test/**]\n    - checks: [domain]\n      paths: [catalogue/schemaid.go]\n      reason: a schema identifier\n")
	}, "FAIL", "has no reason"},

	// C11 · ko's base_import_paths: false, and sibling components
	{"C11", "base_import_paths: false publishes the bare repository, no comp appended", func(f *fx) {
		f.appendTo(".goreleaser.yaml", "kos:\n  - id: server\n    build: server\n    repositories: [ghcr.io/example/widget/server]\n    base_import_paths: false\n")
	}, "PASS", ""},
	{"C11", "a component named after the repo is not flagged when a sibling exists", func(f *fx) {
		f.write("charts/other/Chart.yaml", "apiVersion: v2\nname: other\nversion: 0.0.0\n")
		writePair(f, "other")
		f.sub(".goreleaser.yaml", `cmd/server`, "cmd/widget")
		f.sub("charts/widget/values.yaml", `widget/server`, "widget/widget")
		f.write("charts/other/values.yaml", "image:\n  repository: ghcr.io/example/widget/second\n")
	}, "PASS", ""},
	{"C11", "a ghcr.io reference inside a comment is not a reference to check", func(f *fx) {
		f.write("charts/widget/values.yaml", "# was ghcr.io/example/widget/widget up to an earlier version.\n"+f.read("charts/widget/values.yaml"))
	}, "PASS", "(2 checked)"},
}

func TestRules(t *testing.T) {
	for _, tc := range rulecases {
		want := tc.want
		if want == "" {
			want = "PASS"
		}
		t.Run(tc.id+" "+tc.what, func(t *testing.T) {
			t.Parallel()
			f := conformant(t)
			tc.edit(f)
			log, _, _ := f.run(Options{})
			f.expect(log, tc.id, want)
			l := line(log, tc.id)
			if tc.needle != "" && !strings.Contains(l, tc.needle) && !strings.Contains(log, tc.needle) {
				t.Errorf("%s names %q; got %q", tc.id, tc.needle, l)
			}
			// Only the broken rule moved.
			for _, other := range strings.Split(log, "\n") {
				if regexp.MustCompile(`^C[0-9]+ FAIL`).MatchString(other) && !strings.HasPrefix(other, tc.id+" ") {
					t.Errorf("%s: touching it failed another rule: %s", tc.id, other)
				}
			}
		})
	}
}

func TestInputs(t *testing.T) {
	f := conformant(t)
	f.remove("Justfile")

	log, err, _ := f.run(Options{Strict: "false"})
	if err != nil {
		t.Errorf("strict: false exits 0 on a failure: %v", err)
	}
	if !strings.Contains(log, "::warning title=policy-conformance C4::") {
		t.Errorf("strict: false annotates a warning:\n%s", log)
	}
	if !strings.HasSuffix(log, "policy-conformance: 1 rule(s) failed; reported as warnings (strict: false)\n") {
		t.Errorf("tail %q", log)
	}

	log, err, _ = f.run(Options{Strict: "true"})
	var ee *ExitError
	if !asExit(err, &ee) || ee.Code != 1 || !strings.Contains(log, "::error title=policy-conformance C4::") || !strings.HasSuffix(log, "::error::policy-conformance: 1 rule(s) failed\n") {
		t.Errorf("strict: true exits 1 on a failure: %v\n%s", err, log)
	}

	log, err, _ = f.run(Options{Strict: "true", Skip: "c4", Reason: "no Justfile here yet"})
	if err != nil {
		t.Errorf("a skipped rule does not fail strict: %v", err)
	}
	f.expect(log, "C4", "SKIP")
	if !strings.Contains(line(log, "C4"), "no Justfile here yet") {
		t.Errorf("skip prints the reason: %s", line(log, "C4"))
	}

	if log, err, _ = f.run(Options{Skip: "C4"}); !asExit(err, &ee) || ee.Code != 1 || !strings.HasPrefix(log, "::error::skip needs a reason: ") {
		t.Errorf("skip without a reason is refused: %v %q", err, log)
	}
	if log, err, _ = f.run(Options{Skip: "C99", Reason: "x"}); !asExit(err, &ee) || ee.Code != 1 ||
		log != "::error::skip names 'C99', which is not one of C1 C2 C3 C4 C5 C6 C7 C8 C9 C10 C11 C12 C13\n" {
		t.Errorf("an unknown rule id is refused: %v %q", err, log)
	}
	// commas and whitespace both separate ids
	if _, err, _ = f.run(Options{Skip: "C4 c10,C13", Reason: "x"}); err != nil {
		t.Errorf("%v", err)
	}

	summary := filepath.Join(t.TempDir(), "summary.md")
	f.run(Options{SummaryPath: summary})
	b, _ := os.ReadFile(summary)
	if !strings.Contains(string(b), "| C4 | FAIL |") || !strings.HasPrefix(string(b), "### policy-conformance\n\nRules from truvity/policy `docs/contracts/component.md`; strict: `false`.\n\n| rule | verdict | detail |\n| -- | -- | -- |\n") {
		t.Errorf("the job summary carries a row per rule:\n%s", b)
	}
	// a pipe in a detail is escaped
	f.write("CHANGELOG.md", "## Unreleased\n\n## v1.1.0\n\n## [1.0.0] | x\n")
	f.run(Options{SummaryPath: summary})
	b, _ = os.ReadFile(summary)
	if strings.Contains(string(b), "'[1.0.0] | x'") || !strings.Contains(string(b), `[1.0.0] \| x`) {
		t.Errorf("pipes are escaped in the summary:\n%s", b)
	}
}

func TestNothingToJudge(t *testing.T) {
	empty := t.TempDir()
	var out bytes.Buffer
	_ = Run(context.Background(), Options{Dir: empty, Out: &out})
	log := out.String()
	for _, id := range []string{"C1", "C2", "C3", "C6", "C10", "C11", "C13"} {
		if !strings.HasPrefix(line(log, id), id+" SKIP:") {
			t.Errorf("%s skips when there is nothing to judge: %q", id, line(log, id))
		}
	}
	for _, id := range []string{"C5", "C7", "C8", "C9", "C12"} {
		if !strings.HasPrefix(line(log, id), id+" FAIL:") {
			t.Errorf("%s fails when its file is missing: %q", id, line(log, id))
		}
	}
}

func asExit(err error, target **ExitError) bool {
	e, ok := err.(*ExitError)
	if ok {
		*target = e
	}
	return ok
}

// The real mirror charts of two repositories (CRD charts republished from
// upstream), as their Chart.yaml files read, judged intact and with each way
// C1 can fail them.
func TestMirrorCharts(t *testing.T) {
	chart := func(name, upstream, version string) string {
		return fmt.Sprintf(`apiVersion: v2
name: %s
description: >-
  The upstream CustomResourceDefinitions of %s at the version pinned in
  crdctl.yaml, mirrored verbatim. CRDs only; takes no values.
type: application
# A MIRROR chart (component contract C1): the version is the UPSTREAM
# version it mirrors, declared by the annotation below.
version: %s
appVersion: "%s"
annotations:
  truvity.io/mirror: "%s@%s"
home: https://example.invalid/%s
sources:
  - https://example.invalid/%s
`, name, upstream, version, version, upstream, version, name, name)
	}
	charts := map[string]string{
		"barman-cloud-crds":    chart("barman-cloud-crds", "cloudnative-pg/plugin-barman-cloud", "0.13.0"),
		"cilium-crds":          chart("cilium-crds", "cilium/cilium", "1.20.1"),
		"volume-snapshot-crds": chart("volume-snapshot-crds", "kubernetes-csi/external-snapshotter", "8.6.0"),
	}
	build := func(t *testing.T, names ...string) *fx {
		f := conformant(t)
		f.remove("charts/widget")
		f.remove("tests")
		for _, n := range names {
			f.write("charts/"+n+"/Chart.yaml", charts[n])
			writePair(f, n)
		}
		f.write(".goreleaser.yaml", "builds:\n  - skip: true\n")
		return f
	}
	t.Run("intact", func(t *testing.T) {
		f := build(t, "barman-cloud-crds", "cilium-crds", "volume-snapshot-crds")
		log, _, _ := f.run(Options{})
		if l := line(log, "C1"); l != "C1 PASS: 0 chart(s) commit version 0.0.0, 3 mirror chart(s) at the upstream version" {
			t.Errorf("%s", l)
		}
		f.expect(log, "C2", "PASS")
		f.expect(log, "C3", "PASS")
	})
	for name, tc := range map[string]struct {
		edit func(f *fx)
		want string
	}{
		"version drifted":    {func(f *fx) { f.sub("charts/cilium-crds/Chart.yaml", `^version: 1.20.1`, "version: 1.20.2") }, "charts/cilium-crds version is 1.20.2, not the mirrored 1.20.1"},
		"appVersion drifted": {func(f *fx) { f.sub("charts/cilium-crds/Chart.yaml", `^appVersion: .*`, `appVersion: "1.19.0"`) }, "charts/cilium-crds appVersion is 1.19.0, not the mirrored 1.20.1"},
		"annotation malformed": {func(f *fx) {
			f.sub("charts/cilium-crds/Chart.yaml", `mirror: "cilium/cilium@1.20.1"`, `mirror: "1.20.1"`)
		}, "charts/cilium-crds truvity.io/mirror is '1.20.1', not <owner>/<repo>@<version>"},
		"annotation dropped": {func(f *fx) {
			f.sub("charts/cilium-crds/Chart.yaml", `^  truvity.io/mirror:.*$`, "")
		}, "charts/cilium-crds version is 1.20.1, not 0.0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			f := build(t, "barman-cloud-crds", "cilium-crds")
			tc.edit(f)
			log, _, _ := f.run(Options{})
			if l := line(log, "C1"); !strings.HasPrefix(l, "C1 FAIL: ") || !strings.Contains(l, tc.want) {
				t.Errorf("%s: %q", name, l)
			}
		})
	}
}

func TestUnits(t *testing.T) {
	// the exemption file is read line by line: a column-0 comment ends the block
	f := conformant(t)
	f.write(".github/policy-conformance.yaml", "exempt:\n# a comment at column 0 ends the block\n  C9:\n    reason: ignored\n")
	f.write("LICENSE", "Apache License\n")
	log, _, _ := f.run(Options{})
	f.expect(log, "C9", "FAIL")

	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"internal/s3test/**", "internal/s3test/deep/r.go", true},
		{"internal/*/r.go", "internal/s3test/r.go", true},
		{"a?c", "abc", true}, {"a?c", "abbc", false},
		{"[ab]x", "bx", true}, {"[!ab]x", "bx", false},
		{"HISTORY.md", "docs/HISTORY.md", false},
	} {
		if shellGlob(tc.glob, tc.path) != tc.want {
			t.Errorf("shellGlob(%q, %q) != %v", tc.glob, tc.path, tc.want)
		}
	}
	if got := justfileReachesVuln("check: ci\nci: vuln\n    true\nvuln:\n    true\n"); len(got) != 1 || got[0] != "2: recipe ci depends on vuln, and check depends on ci" {
		t.Errorf("%v", got)
	}
	if got := justfileReachesVuln("vuln:\n    true\n"); got != nil {
		t.Errorf("no check recipe, nothing reached: %v", got)
	}
	if topKey("version: \"1.2.3\" # c\n", "version") != "1.2.3" || topKey("x: 1\n", "version") != "" {
		t.Error("topKey")
	}
	if !awkBlockHas("builds:\n  - id: a\nkos:\n  - x\n", "builds", "- id: a") || awkBlockHas("builds:\n  - id: a\nkos:\n  - x\n", "builds", "- x") {
		t.Error("awkBlock stops at the next top-level key")
	}
}

func awkBlockHas(content, head, want string) bool {
	for _, l := range awkBlock(content, head) {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
