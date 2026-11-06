package companyboundary

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	application = "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: acme\n"
	appProject  = "apiVersion: argoproj.io/v1alpha1\nkind: AppProject\nmetadata:\n  name: acme\n"
	appSet      = "apiVersion: argoproj.io/v1alpha1\nkind: ApplicationSet\nmetadata:\n  name: acme\n"
	kargoStage  = "apiVersion: kargo.akuity.io/v1alpha1\nkind: Stage\nmetadata:\n  name: acme\n"
	namespace   = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: acme\n"
	secretData  = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: acme\ndata:\n  token: c2VjcmV0\n"
	secretStr   = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: acme\nstringData:\n  token: hunter2\n"
	secretEmpty = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: acme\ntype: Opaque\n"
	deployment  = "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: acme\n"
	goodPins    = "pins:\n  acme: 0.1.4\n  globex: \"\"\n"
	goodValues  = "values:\n  acme:\n    app:\n      replicas: 2\n"
	goodMod     = "module example.org/acme/config\n\ngo 1.26\n\nrequire example.org/other/lib v1.2.3\n"
)

// fixture is a tree of files and symlinks (link name -> target).
type fixture struct {
	files map[string]string
	links map[string]string
}

func (f fixture) write(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	for name, body := range f.files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range f.links {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func files(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

type badCase struct {
	name     string
	kind     string
	prefixes string
	fx       fixture
	path     string // the path the ::error:: line names
	rule     string
}

var badCases = []badCase{
	{"Application", "delivery", "", fixture{files: files("apps/acme.yaml", application)}, "apps/acme.yaml", "argo-object"},
	{"AppProject", "delivery", "", fixture{files: files("projects/acme.yaml", appProject)}, "projects/acme.yaml", "argo-object"},
	{"ApplicationSet in a config repo", "config", "", fixture{files: files("tenant/set.yaml", appSet)}, "tenant/set.yaml", "argo-object"},
	{"Application in the second document", "delivery", "", fixture{files: files("x/y.yml", "---\n"+deployment+"---\n"+application)}, "x/y.yml", "argo-object"},
	{"Application inside a List", "delivery", "", fixture{files: files("l.yaml", "apiVersion: v1\nkind: List\nitems:\n- "+strings.ReplaceAll(strings.TrimSuffix(application, "\n"), "\n", "\n  ")+"\n")}, "l.yaml", "argo-object"},
	{"Application as JSON", "delivery", "", fixture{files: files("a.json", `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application"}`)}, "a.json", "argo-object"},
	{"Application in a file that is not valid YAML", "delivery", "", fixture{files: files("t.yaml", "{{ .Values.x }}\n"+application)}, "t.yaml", "argo-object"},
	{"Application with an integer key", "delivery", "", fixture{files: files("a.yaml", application+"1: x\n")}, "a.yaml", "argo-object"},
	{"Application with a bool key", "delivery", "", fixture{files: files("a.yaml", application+"true: x\n")}, "a.yaml", "argo-object"},
	{"Application with a complex key", "delivery", "", fixture{files: files("a.yaml", application+"? [a, b]\n: x\n")}, "a.yaml", "argo-object"},
	{"Namespace with an integer key", "config", "", fixture{files: files("n.yaml", namespace+"1: x\n")}, "n.yaml", "namespace"},
	{"Secret with data and an integer key", "delivery", "", fixture{files: files("s.yaml", secretData+"1: x\n")}, "s.yaml", "secret-data"},
	{"Application inside a nested map with an integer key", "delivery", "", fixture{files: files("l.yaml", "apiVersion: v1\nkind: List\n2: x\nitems:\n- "+strings.ReplaceAll(strings.TrimSuffix(application, "\n"), "\n", "\n  ")+"\n  3: y\n")}, "l.yaml", "argo-object"},
	{"top-level YAML list of objects", "delivery", "", fixture{files: files("a.yaml", "- "+strings.ReplaceAll(strings.TrimSuffix(application, "\n"), "\n", "\n  ")+"\n")}, "a.yaml", "argo-object"},
	{"top-level JSON list of objects", "config", "", fixture{files: files("a.json", `[{"apiVersion":"v1","kind":"Namespace"}]`)}, "a.json", "namespace"},
	{"Kargo Stage", "delivery", "", fixture{files: files("kargo/stage.yaml", kargoStage)}, "kargo/stage.yaml", "kargo-object"},
	{"Namespace", "delivery", "", fixture{files: files("ns.yaml", namespace)}, "ns.yaml", "namespace"},
	{"Namespace in a config repo", "config", "", fixture{files: files("ns.yaml", namespace)}, "ns.yaml", "namespace"},
	{"Secret with data", "delivery", "", fixture{files: files("s.yaml", secretData)}, "s.yaml", "secret-data"},
	{"Secret with stringData", "config", "", fixture{files: files("deep/er/s.yaml", secretStr)}, "deep/er/s.yaml", "secret-data"},
	{"Pulumi.yaml in a delivery repo", "delivery", "", fixture{files: files("Pulumi.yaml", "name: acme\nruntime: go\n")}, "Pulumi.yaml", "pulumi-project"},
	{"stack file in a delivery repo", "delivery", "", fixture{files: files("infra/Pulumi.dev.yaml", "config: {}\n")}, "infra/Pulumi.dev.yaml", "pulumi-project"},
	{"tenant/ in a delivery repo", "delivery", "", fixture{files: files("tenant/readme.txt", "x")}, "tenant", "tenant-dir"},
	{"symlink out of the tree", "delivery", "", fixture{files: files("a.txt", "x"), links: map[string]string{"esc": "../../outside"}}, "esc", "symlink-escape"},
	{"absolute symlink", "config", "", fixture{files: files("a.txt", "x"), links: map[string]string{"docs/abs": "/etc/passwd"}}, "docs/abs", "symlink-escape"},
	{"symlink that climbs through a directory", "delivery", "", fixture{files: files("a/b/c.txt", "x"), links: map[string]string{"a/b/up": "../../../x"}}, "a/b/up", "symlink-escape"},
	{"pin that is a float", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "pins:\n  acme: 1.0\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"pin that is null", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "pins:\n  acme:\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"pin that is a map", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "pins:\n  acme:\n    version: 1.2.3\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"pin with whitespace", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "pins:\n  acme: \"1.2.3 && x\"\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"pins file with another top-level key", "delivery", "", fixture{files: files("pins/acme/devel.yaml", goodPins+"extra: 1\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"pins file without the pins key", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "acme: 0.1.4\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"pins file with a duplicate key", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "pins:\n  acme: 0.1.4\n  acme: 0.1.5\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"pins file with an alias", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "pins:\n  acme: &v 0.1.4\n  globex: *v\n")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"empty pins file", "delivery", "", fixture{files: files("pins/acme/devel.yaml", "")}, "pins/acme/devel.yaml", "pins-malformed"},
	{"values file with another top-level key", "delivery", "", fixture{files: files("values/acme/devel.yaml", goodValues+"extra: {}\n")}, "values/acme/devel.yaml", "values-malformed"},
	{"values file that is a list", "delivery", "", fixture{files: files("values/acme/devel.yaml", "- a\n- b\n")}, "values/acme/devel.yaml", "values-malformed"},
	{"values key that is a scalar", "delivery", "", fixture{files: files("values/acme/devel.yaml", "values: nope\n")}, "values/acme/devel.yaml", "values-malformed"},
	{"service name with a capital", "delivery", "", fixture{files: files("pins/Acme/devel.yaml", goodPins)}, "pins/Acme/devel.yaml", "name-unsafe"},
	{"cluster name with a space", "delivery", "", fixture{files: files("values/acme/de vel.yaml", goodValues)}, "values/acme/de vel.yaml", "name-unsafe"},
	{"service name with a dot", "delivery", "", fixture{files: files("pins/a.b/devel.yaml", goodPins)}, "pins/a.b/devel.yaml", "name-unsafe"},
	{"pin file directly under pins/", "delivery", "", fixture{files: files("pins/acme.yaml", goodPins)}, "pins/acme.yaml", "envelope-layout"},
	{"pin file one level too deep", "delivery", "", fixture{files: files("pins/acme/devel/x.yaml", goodPins)}, "pins/acme/devel/x.yaml", "envelope-layout"},
	{"non-yaml file in values/", "delivery", "", fixture{files: files("values/acme/devel.txt", "x")}, "values/acme/devel.txt", "envelope-layout"},
	{"pins is a symlink to a directory", "delivery", "", fixture{files: files("real/acme/devel.yaml", "acme: 1.0\n"), links: map[string]string{"pins": "real"}}, "pins", "envelope-layout"},
	{"values is a symlink to a directory", "delivery", "", fixture{files: files("real/acme/devel.yaml", goodValues), links: map[string]string{"values": "real"}}, "values", "envelope-layout"},
	{"pins is a regular file", "delivery", "", fixture{files: files("pins", goodPins)}, "pins", "envelope-layout"},
	{"symlink inside pins/", "delivery", "", fixture{files: files("pins/acme/devel.yaml", goodPins), links: map[string]string{"pins/acme/prod.yaml": "devel.yaml"}}, "pins/acme/prod.yaml", "envelope-layout"},
	{"go.mod replace", "config", "", fixture{files: files("go.mod", goodMod+"\nreplace example.org/other/lib => ../lib\n")}, "go.mod", "go-replace"},
	{"go.mod replace block", "config", "", fixture{files: files("sub/go.mod", goodMod+"\nreplace (\n\texample.org/other/lib => ../lib\n)\n")}, "sub/go.mod", "go-replace"},
	{"go.work replace", "config", "", fixture{files: files("go.work", "go 1.26\n\nuse .\n\nreplace example.org/other/lib => ../lib\n")}, "go.work", "go-replace"},
	{"import of a forbidden prefix", "config", "example.org/platform", fixture{files: files("main.go", "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.org/platform/internal/render\"\n)\n\nfunc main() { fmt.Println(render.X) }\n")}, "main.go", "foreign-import"},
	{"import of the prefix itself, second prefix of a list", "config", "example.org/one, example.org/platform", fixture{files: files("a/b.go", "package a\n\nimport _ \"example.org/platform\"\n")}, "a/b.go", "foreign-import"},
	{"import in a Go file that does not parse", "config", "example.org/platform", fixture{files: files("broken.go", "package main\nimport \"example.org/platform/internal/x\"\nfunc {{{\n")}, "broken.go", "foreign-import"},
	{"require of a forbidden prefix", "config", "example.org/platform", fixture{files: files("go.mod", goodMod+"require example.org/platform v0.3.0\n")}, "go.mod", "foreign-require"},
	{"require block of a forbidden prefix", "config", "example.org/platform", fixture{files: files("go.mod", "module example.org/acme/config\n\ngo 1.26\n\nrequire (\n\texample.org/other/lib v1.2.3\n\texample.org/platform/sub v0.3.0 // indirect\n)\n")}, "go.mod", "foreign-require"},
	{"vendored foreign package", "config", "example.org/platform", fixture{files: files("vendor/example.org/platform/internal/x/x.go", "package x\n")}, "vendor/example.org/platform", "foreign-vendor"},
	{"vendored foreign package outside vendor/", "config", "example.org/platform", fixture{files: files("third_party/example.org/platform/internal/x/x.go", "package x\n")}, "third_party/example.org/platform", "foreign-vendor"},
}

func run(t *testing.T, kind, prefixes string, fx fixture) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(Options{Kind: kind, Dir: fx.write(t), ForbiddenImportPrefixes: prefixes, Out: &out})
	return out.String(), err
}

func errorLine(kind, path, rule string) string {
	return "::error::company-boundary: " + kind + ": " + path + ": " + rule
}

func TestBadFixturesAreRefusedWithTheirNamedRule(t *testing.T) {
	for _, c := range badCases {
		t.Run(c.name, func(t *testing.T) {
			out, err := run(t, c.kind, c.prefixes, c.fx)
			if !errors.Is(err, ErrFailed) {
				t.Fatalf("want ErrFailed, got %v\n%s", err, out)
			}
			if want := errorLine(c.kind, c.path, c.rule); !strings.Contains(out, want) {
				t.Fatalf("missing %q in:\n%s", want, out)
			}
		})
	}
}

// Removing a rule must turn its case red: with the named rule switched off
// the same fixture is accepted, so the case depends on that rule alone.
func TestEveryRefusalDependsOnItsRule(t *testing.T) {
	for _, c := range badCases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Run(Options{Kind: c.kind, Dir: c.fx.write(t), ForbiddenImportPrefixes: c.prefixes, Out: &out, disabled: map[string]bool{c.rule: true}})
			if err != nil {
				t.Fatalf("with %s removed the fixture should pass, got %v\n%s", c.rule, err, out.String())
			}
		})
	}
}

var goodCases = []struct {
	name     string
	kind     string
	prefixes string
	fx       fixture
}{
	{"empty delivery repo", "delivery", "", fixture{files: files("README.md", "# acme\n")}},
	{"delivery with pins and values", "delivery", "", fixture{files: files("pins/acme/devel.yaml", goodPins, "values/acme/devel.yaml", goodValues, "pins/acme/.gitkeep", "")}},
	{"dormant and quoted pins", "delivery", "", fixture{files: files("pins/acme/prod.yaml", "pins:\n  acme: \"\"\n  globex: \"1.0\"\n  initech: sha256-abc\n")}},
	{"empty values", "delivery", "", fixture{files: files("values/acme/prod.yaml", "values: {}\n")}},
	{"runbook that shows an Application", "delivery", "", fixture{files: files("docs/runbook.md", "# Runbook\n\n```yaml\n"+application+"```\n\n"+namespace)}},
	{"Justfile", "delivery", "", fixture{files: files("Justfile", "check:\n    echo 'kind: Application argoproj.io'\n")}},
	{"validation tooling", "delivery", "", fixture{files: files("hack/validate.sh", "#!/usr/bin/env bash\ngrep -q 'kind: Namespace' x || true\n", "hack/validate_test.go", "package hack\n")}},
	{"workflow", "delivery", "", fixture{files: files(".github/workflows/check.yaml", "name: check\non: pull_request\njobs: {}\n")}},
	{"Secret without data", "delivery", "", fixture{files: files("s.yaml", secretEmpty)}},
	{"Deployment", "config", "", fixture{files: files("d.yaml", deployment)}},
	{"templated YAML that does not parse", "delivery", "", fixture{files: files("t.yaml", "{{ .Values.x }}\nkind: Deployment\n")}},
	{"in-tree symlink", "delivery", "", fixture{files: files("a/b.txt", "x"), links: map[string]string{"a/link": "b.txt", "up": "a/../a"}}},
	{"the .git directory is not scanned", "delivery", "", fixture{files: files(".git/info/x.yaml", application)}},
	{"config repo with Pulumi and tenant/", "config", "", fixture{files: files("Pulumi.yaml", "name: acme\nruntime: go\n", "Pulumi.dev.yaml", "config: {}\n", "tenant/project.yaml", "name: acme\n")}},
	{"config repo with a clean go.mod", "config", "example.org/platform", fixture{files: files("go.mod", goodMod, "main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println() }\n")}},
	{"a prefix is a path prefix, not a string prefix", "config", "example.org/platform", fixture{files: files("go.mod", goodMod+"require example.org/platformer v1.0.0\n", "a.go", "package a\n\nimport _ \"example.org/platformer/x\"\n")}},
	{"imports are not checked without prefixes", "config", "", fixture{files: files("a.go", "package a\n\nimport _ \"example.org/platform/internal/x\"\n")}},
	{"delivery repo: Go rules are not applied", "delivery", "example.org/platform", fixture{files: files("go.mod", goodMod+"require example.org/platform v0.3.0\nreplace example.org/other/lib => ../lib\n", "a.go", "package a\n\nimport _ \"example.org/platform/x\"\n", "vendor/example.org/platform/x.go", "package x\n")}},
	{"a comment in go.mod is not a replace", "config", "", fixture{files: files("go.mod", goodMod+"// replace example.org/x => ../x\n")}},
}

func TestGoodFixturesPass(t *testing.T) {
	for _, c := range goodCases {
		t.Run(c.name, func(t *testing.T) {
			out, err := run(t, c.kind, c.prefixes, c.fx)
			if err != nil {
				t.Fatalf("want a pass, got %v\n%s", err, out)
			}
			if strings.Contains(out, "::error::") {
				t.Fatalf("a pass must print no error:\n%s", out)
			}
		})
	}
}

// The error line ends at the rule; what the rule is for is a notice line.
func TestErrorLineEndsAtTheRule(t *testing.T) {
	out, err := run(t, "delivery", "", fixture{files: files("ns.yaml", namespace)})
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("want ErrFailed, got %v", err)
	}
	want := "::error::company-boundary: delivery: ns.yaml: namespace\n"
	if !strings.Contains(out, want) {
		t.Fatalf("want the exact line %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "::notice::company-boundary: namespace: "+hints[RuleNamespace]+"\n") {
		t.Fatalf("want the hint on a notice line in:\n%s", out)
	}
}

func TestEveryViolationIsReportedNotJustTheFirst(t *testing.T) {
	out, err := run(t, "delivery", "", fixture{files: files("a.yaml", application, "b.yaml", namespace, "Pulumi.yaml", "name: x\n")})
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("want ErrFailed, got %v", err)
	}
	for _, want := range []string{errorLine("delivery", "a.yaml", "argo-object"), errorLine("delivery", "b.yaml", "namespace"), errorLine("delivery", "Pulumi.yaml", "pulumi-project")} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	fx := fixture{files: files("z.yaml", application, "a.yaml", namespace, "m.yaml", secretData)}
	first, _ := run(t, "delivery", "", fx)
	second, _ := run(t, "delivery", "", fx)
	if first != second || strings.Index(first, "a.yaml") > strings.Index(first, "z.yaml") {
		t.Fatalf("output must be sorted and stable:\n%s\n---\n%s", first, second)
	}
}

func TestAFileNameCannotInjectWorkflowCommands(t *testing.T) {
	out, err := run(t, "delivery", "", fixture{files: files("x\n::error::forged%.yaml", application)})
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("want ErrFailed, got %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "::error::forged") {
			t.Fatalf("a file name started a line of its own:\n%s", out)
		}
	}
	if !strings.Contains(out, "x%0A::error::forged%25.yaml") {
		t.Fatalf("the name should be percent-escaped:\n%s", out)
	}
}

func TestUsageErrorsAreNotRefusals(t *testing.T) {
	root := fixture{files: files("a", "x")}.write(t)
	for name, o := range map[string]Options{
		"no kind":                       {Dir: root},
		"unknown kind":                  {Kind: "bogus", Dir: root},
		"missing dir":                   {Kind: "config", Dir: filepath.Join(root, "nope")},
		"dir is a file":                 {Kind: "config", Dir: filepath.Join(root, "a")},
		"prefix with a shell character": {Kind: "config", Dir: root, ForbiddenImportPrefixes: "example.org/x;rm"},
		"absolute prefix":               {Kind: "config", Dir: root, ForbiddenImportPrefixes: "/example.org/x"},
		"prefix that climbs":            {Kind: "config", Dir: root, ForbiddenImportPrefixes: "example.org/../x"},
	} {
		t.Run(name, func(t *testing.T) {
			o.Out = &bytes.Buffer{}
			err := Run(o)
			if err == nil || errors.Is(err, ErrFailed) {
				t.Fatalf("want a usage error, got %v", err)
			}
		})
	}
}
