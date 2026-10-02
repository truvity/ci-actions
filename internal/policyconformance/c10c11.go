package policyconformance

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ghcrHost is the registry whose written-out image references C11 reads,
// wherever in a line they sit: a search for them, not a check of a URL.
const ghcrHost = "ghcr" + ".io"

var (
	reRecipeHeader = regexp.MustCompile(`^@?[A-Za-z_][A-Za-z0-9_-]*([ \t][^:]*)?:([ \t]|$)`)
	reBodyLine     = regexp.MustCompile(`^[ \t]+[^ \t]`)
	reBlankLine    = regexp.MustCompile(`^[ \t]*$`)
	reJustVuln     = regexp.MustCompile(`(^|[^A-Za-z0-9_-])just[ \t]+([^#]*[ \t])?vuln([ \t]|$)`)
	reDepsStrip    = regexp.MustCompile(`&&|[()]`)
	reWSRun        = regexp.MustCompile(`[ \t]+`)
)

// justfileReachesVuln prints "<line>: <why>" for each way the `check` recipe
// reaches `vuln`: `vuln` among its dependencies, or among those of any recipe
// it depends on (transitively, as far as this Justfile defines them), or a
// body line that runs `just vuln`. A recipe header is a column-0 line
// `name [params]: deps`; `name := value` and `set ...` are not recipes.
func justfileReachesVuln(content string) []string {
	dl := map[string]string{}
	ln := map[string]int{}
	body := map[string][]int{}
	cur := ""
	for i, raw := range lines(content) {
		nr := i + 1
		switch {
		case reRecipeHeader.MatchString(raw):
			line := raw
			if j := strings.Index(line, "#"); j >= 0 {
				line = line[:j]
			}
			name := strings.TrimPrefix(line, "@")
			if j := strings.IndexAny(name, " \t:"); j >= 0 {
				name = name[:j]
			}
			deps := line
			if j := strings.Index(deps, ":"); j >= 0 {
				deps = deps[j+1:]
			}
			deps = reDepsStrip.ReplaceAllString(deps, " ")
			dl[name] = deps
			ln[name] = nr
			cur = name
		case reBodyLine.MatchString(raw):
			if cur != "" && reJustVuln.MatchString(raw) {
				body[cur] = append(body[cur], nr)
			}
		case reBlankLine.MatchString(raw):
		default:
			cur = ""
		}
	}
	if _, ok := dl["check"]; !ok {
		return nil
	}
	var out []string
	queue := []string{"check"}
	seen := map[string]bool{"check": true}
	for head := 0; head < len(queue); head++ {
		r := queue[head]
		for _, n := range body[r] {
			out = append(out, fmt.Sprintf("%d: recipe %s runs `just vuln`", n, r))
		}
		for _, d := range reWSRun.Split(dl[r], -1) {
			if d == "" {
				continue
			}
			if d == "vuln" {
				if r == "check" {
					out = append(out, fmt.Sprintf("%d: the check recipe depends on vuln", ln[r]))
				} else {
					out = append(out, fmt.Sprintf("%d: recipe %s depends on vuln, and check depends on %s", ln[r], r, r))
				}
			} else if _, def := dl[d]; def && !seen[d] {
				seen[d] = true
				queue = append(queue, d)
			}
		}
	}
	return out
}

var reRecipesLine = regexp.MustCompile(`^[ \t\n\v\f\r]*recipes:`)

func (r *run) c10() {
	if !r.exists("go.mod") {
		r.emit("C10", "SKIP", "no go.mod at the root")
		return
	}
	var probs []string
	sec := r.firstExisting(".github/workflows/security.yaml", ".github/workflows/security.yml")
	if sec == "" {
		probs = append(probs, ".github/workflows/security.yaml is missing")
	} else if !strings.Contains(r.read(sec), "truvity/ci-workflows/") {
		probs = append(probs, sec+" does not call truvity/ci-workflows")
	}
	// A new CVE must not turn `check` red: vuln belongs to security.yaml's own
	// schedule, never to the recipes a merge gate runs.
	files := append(r.glob(".github/workflows/*.yaml"), r.glob(".github/workflows/*.yml")...)
	for _, f := range files {
		if f == sec {
			continue
		}
		for _, l := range lines(r.read(f)) {
			if reRecipesLine.MatchString(l) && strings.Contains(l, `"vuln"`) {
				probs = append(probs, f+" runs vuln as a gate recipe")
				break
			}
		}
	}
	// ...and `check` is the recipe every gate runs, so it must not reach `vuln`
	// either: not as a dependency, not through another recipe it depends on,
	// not by calling `just vuln` from a body.
	if jf := r.firstExisting("Justfile", "justfile", ".justfile"); jf != "" {
		for _, hit := range justfileReachesVuln(r.read(jf)) {
			probs = append(probs, jf+":"+hit)
		}
	}
	name := sec
	if name == "" {
		name = "security.yaml"
	}
	r.verdict("C10", name+" present; vuln is neither a gate recipe nor reachable from check", probs...)
}

// C11 · image names do not repeat the repository name
func (r *run) repoName() string {
	if r.o.GithubRepository != "" {
		return r.o.GithubRepository[strings.Index(r.o.GithubRepository, "/")+1:]
	}
	url, err := r.git("remote", "get-url", "origin")
	url = strings.TrimRight(url, "\n")
	if err != nil || url == "" {
		url, _ = filepath.Abs(r.o.Dir)
	}
	url = strings.TrimSuffix(url, ".git")
	return url[strings.LastIndex(url, "/")+1:]
}

var (
	reKoRepoKey  = regexp.MustCompile(`^[ \t\n\v\f\r]*(ko-docker-repo|image-repo|KO_DOCKER_REPO):`)
	reReposList  = regexp.MustCompile(`^[ \t\n\v\f\r]*repositories:[ \t\n\v\f\r]*\[`)
	reMainKey    = regexp.MustCompile(`^[ \t\n\v\f\r]*-?[ \t\n\v\f\r]*main:`)
	reCommentOut = regexp.MustCompile(`#.*$`)
	reQuoteSpace = regexp.MustCompile(`["'\s]`)
	reTemplate   = regexp.MustCompile(`\{\{`)
	reGhcrCharts = regexp.MustCompile(`^ghcr\.io/[^/]+/charts(/|$)`)
	reReposItems = regexp.MustCompile(`repositories:[ \t\n\v\f\r]*\[([^\]]+)\]`)
	reBaseFalse  = regexp.MustCompile(`base_import_paths:[ \t\n\v\f\r]*false`)
	reCommentLn  = regexp.MustCompile(`^[ \t\n\v\f\r]*#`)
)

// ghcrRefs is `grep -o 'ghcr\.io/[A-Za-z0-9._/-]+'`: every image reference
// written out in a line, found by search, left to right, without overlap.
func ghcrRefs(line string) []string {
	var out []string
	for {
		i := strings.Index(line, ghcrHost+"/")
		if i < 0 {
			return out
		}
		start := i + len(ghcrHost) + 1
		end := start
		for end < len(line) {
			c := line[end]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '/' || c == '-' {
				end++
				continue
			}
			break
		}
		if end == start {
			line = line[start:]
			continue
		}
		out = append(out, line[i:end])
		line = line[end:]
	}
}

func uniqSorted(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (r *run) c11() {
	repo := r.repoName()
	var files []string
	for _, f := range []string{".goreleaser.yaml", ".goreleaser.yml", ".ko.yaml", ".github/workflows/release.yaml"} {
		if r.exists(f) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		r.emit("C11", "SKIP", "no .goreleaser.yaml, .ko.yaml or release.yaml")
		return
	}
	// Where ko publishes: ko-docker-repo / image-repo (the release workflow's
	// inputs, which ko obeys), and goreleaser's kos `repositories:`.
	var raw []string
	for _, f := range files {
		for _, l := range lines(r.read(f)) {
			if reKoRepoKey.MatchString(l) {
				raw = append(raw, strings.TrimLeft(l[strings.Index(l, ":")+1:], ws))
			}
		}
	}
	for _, f := range files {
		for _, l := range lines(r.read(f)) {
			if loc := reReposList.FindStringIndex(l); loc != nil {
				v := l[loc[1]:]
				if i := strings.Index(v, "]"); i >= 0 {
					v = v[:i]
				}
				raw = append(raw, strings.Split(v, ",")...)
			}
		}
	}
	var prefixes []string
	for _, s := range raw {
		s = reCommentOut.ReplaceAllString(s, "")
		s = reQuoteSpace.ReplaceAllString(s, "")
		if s == "" || reTemplate.MatchString(s) {
			continue
		}
		prefixes = append(prefixes, s)
	}
	prefixes = uniqSorted(prefixes)

	// What ko appends: the base of each build's main package.
	var comps []string
	for _, f := range files {
		for _, l := range lines(r.read(f)) {
			if !reMainKey.MatchString(l) {
				continue
			}
			v := strings.TrimLeft(l[strings.Index(l, ":")+1:], ws)
			v = reCommentOut.ReplaceAllString(v, "")
			v = reQuoteSpace.ReplaceAllString(v, "")
			v = strings.TrimRight(v, "/")
			if v == "" {
				continue
			}
			last := v[strings.LastIndex(v, "/")+1:]
			if last == "" || last == "." {
				continue
			}
			comps = append(comps, last)
		}
	}
	comps = uniqSorted(comps)

	// A ko `repositories:` entry with `base_import_paths: false` set in the
	// same kos block publishes the repository AS the image name: ko's namer
	// consults base_import_paths before `bare`, and false wins either way, so
	// nothing from `main:` is appended for that prefix. Scoped by walking the
	// kos: block of each goreleaser file and remembering the repository last
	// seen when base_import_paths: false is hit.
	bare := map[string]bool{}
	for _, gf := range files {
		if !strings.HasSuffix(gf, ".goreleaser.yaml") && !strings.HasSuffix(gf, ".goreleaser.yml") {
			continue
		}
		cur := ""
		for _, l := range awkBlock(r.read(gf), "kos") {
			if m := reReposItems.FindStringSubmatch(l); m != nil {
				c := m[1]
				if i := strings.Index(c, ","); i >= 0 {
					c = c[:i]
				}
				cur = reQuoteSpace.ReplaceAllString(c, "")
			}
			if cur != "" && reBaseFalse.MatchString(l) {
				bare[cur] = true
			}
		}
	}
	var images []string
	for _, p := range prefixes {
		if bare[p] || len(comps) == 0 {
			images = append(images, p)
		} else {
			for _, c := range comps {
				images = append(images, p+"/"+c)
			}
		}
	}
	// Image references written out in full, including a chart's default.
	// Comment-only lines are dropped first: a line explaining what an image
	// USED to be named (e.g. "# was ghcr.io/x/x up to v0.2.0") is not a
	// reference to check, and matching alone cannot tell the two apart.
	vals := r.glob("charts/*/values.yaml")
	all := append(append([]string(nil), files...), vals...)
	var refs []string
	for _, f := range all {
		for _, l := range lines(r.read(f)) {
			if reCommentLn.MatchString(l) {
				continue
			}
			for _, m := range ghcrRefs(l) {
				if !reGhcrCharts.MatchString(m) {
					refs = append(refs, m)
				}
			}
		}
	}
	images = append(images, uniqSorted(refs)...)
	if len(images) == 0 {
		r.emit("C11", "SKIP", "no image names found in "+strings.Join(files, " "))
		return
	}
	// Distinct images first: the same image named twice (once by goreleaser,
	// once again in a chart's values.yaml default) is one image, not a sibling
	// of itself.
	seen := map[string]bool{}
	var distinct []string
	for _, img := range images {
		if !seen[img] {
			seen[img] = true
			distinct = append(distinct, img)
		}
	}
	// A component sharing a registry prefix with sibling images (a repo that
	// ships several binaries) may legitimately have one component named after
	// the repo itself; only a SOLE image under a prefix that repeats the
	// repository is the degenerate case the rule means.
	prefixCount := map[string]int{}
	for _, img := range distinct {
		if strings.Count(img, "/") >= 2 {
			prefixCount[img[:strings.LastIndex(img, "/")]]++
		}
	}
	var probs []string
	for _, img := range distinct {
		rest := img[strings.Index(img, "/")+1:] // drop the registry
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[i+1:] // drop the owner
		}
		if strings.Contains(rest, "/") && rest[strings.LastIndex(rest, "/")+1:] == repo {
			pc, ok := prefixCount[img[:strings.LastIndex(img, "/")]]
			if !ok {
				pc = 1
			}
			if pc <= 1 {
				probs = append(probs, img+" repeats the repository name")
			}
		}
	}
	r.verdict("C11", fmt.Sprintf("image names under %s do not repeat it (%d checked)", repo, len(distinct)), probs...)
}
