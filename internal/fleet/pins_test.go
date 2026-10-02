package fleet

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	shaAction140 = "1111111111111111111111111111111111111111" // ci-actions v1.4.0
	shaAction161 = "2222222222222222222222222222222222222222" // ci-actions v1.6.1
	shaAction150 = "3333333333333333333333333333333333333333" // ci-actions v1.5.0
	shaLoose     = "4444444444444444444444444444444444444444" // ci-actions, no tag
	shaWf316     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // ci-workflows v3.16.0
	shaWf3181    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // ci-workflows v3.18.1
	shaWf32      = "cccccccccccccccccccccccccccccccccccccccc" // ci-workflows v3.2.0 (setup-devbox still in-tree)
	shaWfMaster  = "dddddddddddddddddddddddddddddddddddddddd" // ci-workflows, untagged
)

// fakeSource is an in-memory GitHub.
type fakeSource struct {
	repos map[string][]Repo   // org -> repos
	files map[string]string   // repo|path|ref -> content
	dirs  map[string][]string // repo|dir|ref -> files
	tags  map[string][]Tag    // lib -> tags
	errs  map[string]error    // repo|path|ref -> error
	reads map[string]int      // repo|path|ref -> count
	mu    sync.Mutex
}

func (f *fakeSource) ListRepos(_ context.Context, o string) ([]Repo, error) { return f.repos[o], nil }
func (f *fakeSource) ListDir(_ context.Context, repo, dir, ref string) ([]string, error) {
	return f.dirs[repo+"|"+dir+"|"+ref], nil
}
func (f *fakeSource) File(_ context.Context, repo, p, ref string) ([]byte, error) {
	k := repo + "|" + p + "|" + ref
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	f.reads[k]++
	if err := f.errs[k]; err != nil {
		return nil, err
	}
	c, ok := f.files[k]
	if !ok {
		return nil, ErrNotFound
	}
	return []byte(c), nil
}
func (f *fakeSource) Tree(_ context.Context, repo, ref string) ([]string, error) {
	var out []string
	for k := range f.files {
		parts := strings.SplitN(k, "|", 3)
		if parts[0] == repo && parts[2] == ref {
			out = append(out, parts[1])
		}
	}
	sort.Strings(out)
	return out, nil
}
func (f *fakeSource) Tags(_ context.Context, repo string) ([]Tag, error) { return f.tags[repo], nil }

func newFake() *fakeSource {
	f := &fakeSource{
		repos: map[string][]Repo{},
		files: map[string]string{},
		dirs:  map[string][]string{},
		errs:  map[string]error{},
		tags: map[string][]Tag{
			"truvity/ci-actions": {
				{"v1.4.0", shaAction140}, {"v1.5.0", shaAction150}, {"v1.6.1", shaAction161}, {"v1.6.0", shaAction161}, // two tags, one commit
			},
			"truvity/ci-workflows": {
				{"v3.16.0", shaWf316}, {"v3.18.1", shaWf3181}, {"v3.2.0", shaWf32},
			},
		},
	}
	// check.yaml at v3.16.0 pins setup-devbox v1.4.0; at v3.18.1, v1.6.1.
	f.files["truvity/ci-workflows|.github/workflows/check.yaml|"+shaWf316] = "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-actions/setup-devbox@" + shaAction140 + " # v9.9.9\n      - uses: truvity/ci-actions/recipe@" + shaAction140 + "\n"
	f.files["truvity/ci-workflows|.github/workflows/check.yaml|"+shaWf3181] = "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-actions/setup-devbox@" + shaAction161 + "\n"
	f.files["truvity/ci-workflows|.github/workflows/check.yaml|"+shaWf32] = "jobs:\n  a:\n    steps:\n      - uses: ./.github/actions/setup-devbox\n"
	f.files["truvity/ci-workflows|.github/workflows/check.yaml|"+shaWfMaster] = "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-actions/setup-devbox@" + shaLoose + "\n"
	return f
}

func (f *fakeSource) addRepo(org, name string, archived bool, workflows map[string]string) {
	full := org + "/" + name
	f.repos[org] = append(f.repos[org], Repo{Name: name, FullName: full, DefaultBranch: "main", Archived: archived})
	var names []string
	for file, content := range workflows {
		p := ".github/workflows/" + file
		names = append(names, p)
		f.files[full+"|"+p+"|main"] = content
	}
	f.dirs[full+"|.github/workflows|main"] = names
}

func caller(workflowSHA string) string {
	return "jobs:\n  check:\n    uses: truvity/ci-workflows/.github/workflows/check.yaml@" + workflowSHA + " # v3.99.0\n"
}

func scan(t *testing.T, f *fakeSource, min string) *Report {
	t.Helper()
	rep, err := Scan(context.Background(), f, Config{Orgs: []string{"acme", "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if min != "" {
		if _, err := ApplyGate(rep, min); err != nil {
			t.Fatal(err)
		}
	}
	return rep
}

func byName(rep *Report) map[string]RepoResult {
	m := map[string]RepoResult{}
	for _, r := range rep.Repos {
		m[r.Repo] = r
	}
	return m
}

func TestScanResolvesTransitiveSetupDevbox(t *testing.T) {
	f := newFake()
	f.addRepo("acme", "old", false, map[string]string{"ci.yaml": caller(shaWf316)})
	f.addRepo("acme", "new", false, map[string]string{"ci.yaml": caller(shaWf3181)})
	f.addRepo("other", "direct", false, map[string]string{"ci.yaml": "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-actions/setup-devbox@" + shaAction150 + "\n"})
	f.addRepo("acme", "none", false, map[string]string{"ci.yaml": "jobs:\n  a:\n    steps:\n      - uses: actions/checkout@v4\n"})
	f.addRepo("acme", "nodir", false, nil)

	got := byName(scan(t, f, ""))

	for _, tc := range []struct {
		repo, setup, workflows, actions string
	}{
		// the comment says v9.9.9 and v3.99.0: only the sha counts
		{"acme/old", "v1.4.0", "v3.16.0", ""},
		{"acme/new", "v1.6.1", "v3.18.1", ""},
		{"other/direct", "v1.5.0", "", "v1.5.0"},
	} {
		r, ok := got[tc.repo]
		if !ok || r.SetupDevbox == nil {
			t.Fatalf("%s: no setup-devbox resolved: %+v", tc.repo, r)
		}
		if r.SetupDevbox.Lowest != tc.setup {
			t.Errorf("%s: lowest = %q, want %q", tc.repo, r.SetupDevbox.Lowest, tc.setup)
		}
		if g := strings.Join(r.CIWorkflows, ","); g != tc.workflows {
			t.Errorf("%s: ci-workflows = %q, want %q", tc.repo, g, tc.workflows)
		}
		if g := strings.Join(r.CIActions, ","); g != tc.actions {
			t.Errorf("%s: ci-actions = %q, want %q", tc.repo, g, tc.actions)
		}
	}
	if got["acme/none"].SetupDevbox != nil || len(got["acme/none"].Pins) != 0 {
		t.Errorf("a repository pinning nothing has no pins: %+v", got["acme/none"])
	}
	if got["acme/nodir"].Error != "" {
		t.Errorf("a repository without workflows is not an error")
	}
	// the transitive pin says where it came from
	if via := got["acme/old"].SetupDevbox.Pins[0].Via; via != "truvity/ci-workflows/.github/workflows/check.yaml@v3.16.0" {
		t.Errorf("via = %q", via)
	}
	// a commit with two tags resolves to the higher
	f2 := newFake()
	f2.addRepo("acme", "x", false, map[string]string{"ci.yaml": "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-actions/setup-devbox@" + shaAction161 + "\n"})
	if v := byName(scan(t, f2, ""))["acme/x"].SetupDevbox.Lowest; v != "v1.6.1" {
		t.Errorf("two tags on one commit resolved to %q, want v1.6.1", v)
	}
}

func TestGate(t *testing.T) {
	build := func() *fakeSource {
		f := newFake()
		f.addRepo("acme", "old", false, map[string]string{"ci.yaml": caller(shaWf316)})
		f.addRepo("acme", "new", false, map[string]string{"ci.yaml": caller(shaWf3181)})
		f.addRepo("acme", "none", false, map[string]string{"ci.yaml": "name: x\n"})
		return f
	}
	for _, tc := range []struct {
		name, min string
		below     []string
	}{
		{"boundary is inclusive", "v1.6.1", []string{"acme/old"}},
		{"raised above everything", "v1.7.0", []string{"acme/new", "acme/old"}},
		{"lowered below everything", "v1.4.0", nil},
		{"no gate marks nothing", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := scan(t, build(), tc.min)
			if !reflect.DeepEqual(rep.Below, tc.below) {
				t.Errorf("below = %v, want %v", rep.Below, tc.below)
			}
		})
	}
	if _, err := ApplyGate(&Report{}, "latest"); err == nil {
		t.Error("a gate that is not a version is refused")
	}
}

func TestUnprovablePinsCountAsBelow(t *testing.T) {
	f := newFake()
	// untagged ci-actions sha, reached through an untagged ci-workflows master commit
	f.addRepo("acme", "loose", false, map[string]string{"ci.yaml": caller(shaWfMaster)})
	// pre-split: setup-devbox vendored inside ci-workflows v3.2.0
	f.addRepo("acme", "intree", false, map[string]string{"ci.yaml": caller(shaWf32)})
	// pre-split, pinned directly
	f.addRepo("acme", "directintree", false, map[string]string{"ci.yaml": "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-workflows/.github/actions/setup-devbox@" + shaWf32 + "\n"})

	rep := scan(t, f, "v0.0.1")
	got := byName(rep)
	if l := got["acme/loose"].SetupDevbox.Lowest; l != "untagged:"+shaLoose[:12] {
		t.Errorf("loose lowest = %q", l)
	}
	if l := got["acme/intree"].SetupDevbox.Lowest; !strings.HasPrefix(l, "in-tree") {
		t.Errorf("intree lowest = %q", l)
	}
	if l := got["acme/directintree"].SetupDevbox.Lowest; l != "in-tree v3.2.0" {
		t.Errorf("directintree lowest = %q", l)
	}
	want := []string{"acme/directintree", "acme/intree", "acme/loose"}
	if !reflect.DeepEqual(rep.Below, want) {
		t.Errorf("below = %v, want %v even against v0.0.1", rep.Below, want)
	}
}

func TestMultiplePinsTakeTheLowest(t *testing.T) {
	f := newFake()
	f.addRepo("acme", "mixed", false, map[string]string{
		"a.yaml": caller(shaWf3181),
		"b.yaml": caller(shaWf316),
	})
	rep := scan(t, f, "v1.6.1")
	r := byName(rep)["acme/mixed"]
	if r.SetupDevbox.Lowest != "v1.4.0" || len(r.SetupDevbox.Pins) != 2 || !r.SetupDevbox.BelowMin {
		t.Errorf("%+v", r.SetupDevbox)
	}
	if cell := setupCell(r); cell != "v1.4.0 (also v1.6.1) *" {
		t.Errorf("cell = %q", cell)
	}
}

func TestArchivedAndErrorsAndCaching(t *testing.T) {
	f := newFake()
	f.addRepo("acme", "live", false, map[string]string{"ci.yaml": caller(shaWf316)})
	f.addRepo("acme", "dead", true, map[string]string{"ci.yaml": caller(shaWf316)})
	f.addRepo("acme", "twin", false, map[string]string{"ci.yaml": caller(shaWf316)})
	// a pin whose reusable workflow cannot be read must not be silently skipped
	f.addRepo("acme", "gone", false, map[string]string{"ci.yaml": "jobs:\n  c:\n    uses: truvity/ci-workflows/.github/workflows/missing.yaml@" + shaWf316 + "\n"})

	rep := scan(t, f, "")
	got := byName(rep)
	if _, ok := got["acme/dead"]; ok {
		t.Error("archived repositories are skipped by default")
	}
	if got["acme/gone"].Error == "" || len(rep.Errors) != 1 {
		t.Errorf("an unreadable pinned workflow is an error, not a pass: %+v / %v", got["acme/gone"], rep.Errors)
	}
	if n := f.reads["truvity/ci-workflows|.github/workflows/check.yaml|"+shaWf316]; n != 1 {
		t.Errorf("a pinned file is read once however many repositories pin it, got %d", n)
	}
	rep2, _ := Scan(context.Background(), f, Config{Orgs: []string{"acme"}, IncludeArchived: true})
	if _, ok := byName(rep2)["acme/dead"]; !ok {
		t.Error("--include-archived includes them")
	}
}

func TestNestedLocalWorkflowAndCycle(t *testing.T) {
	f := newFake()
	f.files["truvity/ci-workflows|.github/workflows/outer.yaml|"+shaWf3181] = "jobs:\n  a:\n    uses: ./.github/workflows/inner.yaml\n"
	f.files["truvity/ci-workflows|.github/workflows/inner.yaml|"+shaWf3181] = "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-actions/setup-devbox@" + shaAction161 + "\n      - uses: ./.github/workflows/outer.yaml\n"
	f.addRepo("acme", "nest", false, map[string]string{"ci.yaml": "jobs:\n  c:\n    uses: truvity/ci-workflows/.github/workflows/outer.yaml@" + shaWf3181 + "\n"})
	r := byName(scan(t, f, ""))["acme/nest"]
	if r.SetupDevbox == nil || r.SetupDevbox.Lowest != "v1.6.1" {
		t.Fatalf("nested local workflow not followed: %+v", r)
	}
	if got := strings.Join(r.CIWorkflows, ","); got != "v3.18.1" {
		t.Errorf("the nested file's own pin is not the repository's: %q", got)
	}
}

func TestTableAndJSON(t *testing.T) {
	f := newFake()
	f.addRepo("acme", "old", false, map[string]string{"ci.yaml": caller(shaWf316)})
	f.addRepo("acme", "none", false, map[string]string{"ci.yaml": "name: x\n"})
	rep := scan(t, f, "v1.6.1")

	var b strings.Builder
	WriteTable(&b, rep, false)
	out := b.String()
	for _, want := range []string{"REPOSITORY", "acme/old", "v3.16.0", "v1.4.0 *", "2 repositories scanned, 1 pin the shared CI libraries", "1 repositories resolve setup-devbox below v1.6.1:", "  acme/old"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "acme/none") {
		t.Errorf("a repository pinning nothing is hidden without --all:\n%s", out)
	}
	b.Reset()
	WriteTable(&b, rep, true)
	if !strings.Contains(b.String(), "acme/none") {
		t.Error("--all lists it")
	}

	b.Reset()
	if err := WriteJSON(&b, rep); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"repo": "acme/old"`, `"lowest": "v1.4.0"`, `"below_min": true`, `"min_setup_devbox": "v1.6.1"`, fmt.Sprintf(`"sha": %q`, shaAction140)} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("json lacks %s:\n%s", want, b.String())
		}
	}
}

func pin(sha string) string { return "      - uses: truvity/ci-actions/setup-devbox@" + sha + "\n" }

func TestRepoLocalCompositeActionsAreFollowed(t *testing.T) {
	f := newFake()
	f.addRepo("acme", "local", false, map[string]string{
		"ci.yaml": "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/setup\n",
	})
	// setup calls a second local action, which holds the setup-devbox pin
	f.files["acme/local|.github/actions/setup/action.yaml|main"] = "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/inner\n"
	f.files["acme/local|.github/actions/inner/action.yml|main"] = "runs:\n  using: composite\n  steps:\n" + pin(shaAction140)

	// an action nothing references is still read
	f.addRepo("acme", "orphan", false, map[string]string{"ci.yaml": "name: x\n"})
	f.files["acme/orphan|.github/actions/deep/er/action.yaml|main"] = "runs:\n  steps:\n" + pin(shaAction150)

	// a cycle between two local actions terminates
	f.addRepo("acme", "cycle", false, map[string]string{"ci.yaml": "jobs:\n  a:\n    steps:\n      - uses: ./.github/actions/a\n"})
	f.files["acme/cycle|.github/actions/a/action.yaml|main"] = "runs:\n  steps:\n    - uses: ./.github/actions/b\n" + pin(shaAction161)
	f.files["acme/cycle|.github/actions/b/action.yaml|main"] = "runs:\n  steps:\n    - uses: ./.github/actions/a\n"

	// a pinned reusable workflow whose local action holds the pin
	f.files["truvity/ci-workflows|.github/workflows/viaaction.yaml|"+shaWf3181] = "jobs:\n  a:\n    steps:\n      - uses: ./.github/actions/helper\n"
	f.files["truvity/ci-workflows|.github/actions/helper/action.yaml|"+shaWf3181] = "runs:\n  steps:\n" + pin(shaAction150)
	f.addRepo("acme", "viapinned", false, map[string]string{"ci.yaml": "jobs:\n  c:\n    uses: truvity/ci-workflows/.github/workflows/viaaction.yaml@" + shaWf3181 + "\n"})

	got := byName(scan(t, f, ""))
	for repo, want := range map[string]string{"acme/local": "v1.4.0", "acme/orphan": "v1.5.0", "acme/cycle": "v1.6.1", "acme/viapinned": "v1.5.0"} {
		r := got[repo]
		if r.Error != "" || r.SetupDevbox == nil || r.SetupDevbox.Lowest != want {
			t.Errorf("%s: want setup-devbox %s, got %+v (%s)", repo, want, r.SetupDevbox, r.Error)
		}
	}
	p := got["acme/local"].SetupDevbox.Pins[0]
	if p.File != ".github/actions/inner/action.yml" || p.Via != "" {
		t.Errorf("a pin in a local action is the repository's own, and names its file: %+v", p)
	}
	if v := viaCell(got["acme/local"]); v != "local action .github/actions/inner/action.yml" {
		t.Errorf("via = %q", v)
	}
	if got["acme/local"].CIActions[0] != "v1.4.0" {
		t.Errorf("a direct pin in a local action counts as the repository's: %v", got["acme/local"].CIActions)
	}
	if v := got["acme/viapinned"].SetupDevbox.Pins[0].Via; !strings.HasPrefix(v, "truvity/ci-workflows/.github/workflows/viaaction.yaml@") {
		t.Errorf("via = %q", v)
	}
	// a local action that is not there, and a path that escapes, are ignored
	f.addRepo("acme", "dangling", false, map[string]string{"ci.yaml": "jobs:\n  a:\n    steps:\n      - uses: ./missing\n      - uses: ./../x\n      - uses: ./.github/workflows/other.yaml\n"})
	if r := byName(scan(t, f, ""))["acme/dangling"]; r.Error != "" || len(r.Pins) != 0 {
		t.Errorf("%+v", r)
	}
}

func TestRunnerColumn(t *testing.T) {
	f := newFake()
	job := func(runs string) string { return "jobs:\n  a:\n    " + runs + "\n    steps: []\n" }
	f.addRepo("acme", "hosted", false, map[string]string{"ci.yaml": job("runs-on: ubuntu-latest") + caller(shaWf3181)[5:]})
	f.addRepo("acme", "self", false, map[string]string{"ci.yaml": job("runs-on: [self-hosted, linux]") + "\n" + caller(shaWf3181)})
	f.addRepo("acme", "mixed", false, map[string]string{"a.yaml": job("runs-on: ubuntu-24.04"), "b.yaml": job("runs-on: tier-small")})
	f.addRepo("acme", "dyn", false, map[string]string{"ci.yaml": job("runs-on: ${{ inputs.runner }}")})
	f.addRepo("acme", "caller", false, map[string]string{"ci.yaml": caller(shaWf3181)})
	rep := scan(t, f, "")
	got := byName(rep)
	for repo, want := range map[string]string{"acme/hosted": "hosted", "acme/self": "self-hosted", "acme/mixed": "mixed", "acme/dyn": "dynamic", "acme/caller": ""} {
		if got[repo].Runner != want {
			t.Errorf("%s: runner = %q, want %q", repo, got[repo].Runner, want)
		}
	}

	var b strings.Builder
	WriteTable(&b, rep, true)
	for _, want := range []string{"RUNNERS", "acme/self", "self-hosted"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, b.String())
		}
	}
	// the filter only narrows the printed rows; the report and gate keep everything
	rep.RunnerFilter = "self-hosted"
	b.Reset()
	WriteTable(&b, rep, true)
	if !strings.Contains(b.String(), "acme/self") || !strings.Contains(b.String(), "acme/mixed") || strings.Contains(b.String(), "acme/hosted") || strings.Contains(b.String(), "acme/dyn") {
		t.Errorf("--runner-filter self-hosted:\n%s", b.String())
	}
	rep.RunnerFilter = "hosted"
	b.Reset()
	WriteTable(&b, rep, true)
	if !strings.Contains(b.String(), "acme/hosted") || strings.Contains(b.String(), "acme/self") || strings.Contains(b.String(), "acme/mixed") {
		t.Errorf("--runner-filter hosted:\n%s", b.String())
	}
	if len(rep.Repos) != 5 {
		t.Error("the filter must not drop repositories from the report")
	}
}

func TestParseRunsOn(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     [][]string
		kinds    []string
	}{
		{"scalar", "jobs:\n  a:\n    runs-on: ubuntu-latest # hosted\n", [][]string{{"ubuntu-latest"}}, []string{"hosted"}},
		{"quoted", "    runs-on: \"macos-14\"\n", [][]string{{"macos-14"}}, []string{"hosted"}},
		{"flow list", "    runs-on: [self-hosted, ubuntu-latest]\n", [][]string{{"self-hosted", "ubuntu-latest"}}, []string{"self-hosted"}},
		{"block list", "    runs-on:\n      - ubuntu-latest\n      - x64\n    steps: []\n", [][]string{{"ubuntu-latest", "x64"}}, []string{"self-hosted"}},
		{"group", "    runs-on:\n      group: big\n      labels: [ubuntu-latest]\n    steps: []\n", [][]string{{"group:big", "ubuntu-latest"}}, []string{"self-hosted"}},
		{"expression", "    runs-on: ${{ matrix.os }}\n", [][]string{{"${{ matrix.os }}"}}, []string{"dynamic"}},
		{"two jobs", "  a:\n    runs-on: ubuntu-latest\n  b:\n    runs-on: windows-2022\n", [][]string{{"ubuntu-latest"}, {"windows-2022"}}, []string{"hosted", "hosted"}},
		{"commented out", "    # runs-on: self-hosted\n", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseRunsOn(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i, labels := range got {
				if k := JobRunnerKind(labels); k != tc.kinds[i] {
					t.Errorf("kind of %v = %s, want %s", labels, k, tc.kinds[i])
				}
			}
		})
	}
}
