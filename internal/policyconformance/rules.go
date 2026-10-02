package policyconformance

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/truvity/ci-actions/internal/verscmp"
)

// C4 · the leak canary, run by `just check`
func (r *run) c4() {
	var probs []string
	if !r.exists("hack/leak-canary.sh") {
		probs = append(probs, "hack/leak-canary.sh is missing")
	}
	jf := r.firstExisting("Justfile", "justfile", ".justfile")
	if jf == "" {
		probs = append(probs, "no Justfile, so `just check` cannot run the canary")
	} else if !strings.Contains(r.read(jf), "leak-canary") {
		probs = append(probs, jf+" never mentions hack/leak-canary.sh")
	}
	r.verdict("C4", "hack/leak-canary.sh exists and "+jf+" runs it", probs...)
}

// C5 · CHANGELOG.md headings

// latestTag is the newest release tag reachable from the default branch,
// after fetching the tags (and unshallowing a depth-1 checkout).
func (r *run) latestTag() (string, bool) {
	if _, err := r.git("rev-parse", "--git-dir"); err != nil {
		return "", false
	}
	if shallow, _ := r.git("rev-parse", "--is-shallow-repository"); strings.TrimSpace(shallow) == "true" {
		_, _ = r.git("fetch", "--quiet", "--unshallow", "--tags", "origin")
	} else {
		_, _ = r.git("fetch", "--quiet", "--tags", "origin")
	}
	var cands []string
	if r.o.DefaultBranch != "" {
		cands = append(cands, "origin/"+r.o.DefaultBranch)
	}
	cands = append(cands, "origin/master", "origin/main", "HEAD")
	base := ""
	for _, b := range cands {
		if _, err := r.git("rev-parse", "--verify", "-q", b+"^{commit}"); err == nil {
			base = b
			break
		}
	}
	if base == "" {
		return "", false
	}
	tag, err := r.git("describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", base)
	tag = strings.TrimRight(tag, "\n")
	if err != nil {
		return "", false
	}
	// Two tags on one commit (a re-tag, or a patch cut on an unchanged tree):
	// describe picks either, so take the highest version there.
	out, _ := r.git("tag", "--points-at", tag+"^{commit}", "--list", "v[0-9]*")
	tags := lines(out)
	sort.SliceStable(tags, func(i, j int) bool { return verscmp.Compare(tags[i], tags[j]) < 0 })
	if len(tags) == 0 {
		return "", true
	}
	return tags[len(tags)-1], true
}

var (
	reH2         = regexp.MustCompile(`^## `)
	reVersionH   = regexp.MustCompile(`^## (v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)([ \t\n\v\f\r].*)?$`)
	reSemverAny  = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)
	reTagFull    = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	reNewestXY   = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.[0-9]+`)
	reTrailSpace = regexp.MustCompile(`[ \t\n\v\f\r]+$`)
)

func (r *run) c5() {
	if !r.exists("CHANGELOG.md") {
		r.emit("C5", "FAIL", "CHANGELOG.md is missing")
		return
	}
	var heads, versions, bad, probs []string
	for _, l := range lines(r.read("CHANGELOG.md")) {
		if reH2.MatchString(l) {
			heads = append(heads, reTrailSpace.ReplaceAllString(l, ""))
		}
	}
	unreleased, firstUnreleased := 0, -1
	for i, h := range heads {
		if strings.HasPrefix(h, "## Unreleased") {
			unreleased++
			if firstUnreleased < 0 {
				firstUnreleased = i
			}
		} else if m := reVersionH.FindStringSubmatch(h); m != nil {
			versions = append(versions, m[1])
		} else if reSemverAny.MatchString(h) {
			bad = append(bad, strings.TrimPrefix(h, "## "))
		}
	}
	if len(bad) != 0 {
		probs = append(probs, fmt.Sprintf("%d version heading(s) not in the form ## vX.Y.Z, e.g. '%s'", len(bad), bad[0]))
	}
	if unreleased > 1 {
		probs = append(probs, fmt.Sprintf("%d Unreleased headings; at most one", unreleased))
	}
	if firstUnreleased > 0 {
		probs = append(probs, "## Unreleased is not the first heading")
	}
	if len(versions) == 0 {
		probs = append(probs, "no ## vX.Y.Z headings")
	}
	if len(versions) > 1 {
		sorted := append([]string(nil), versions...)
		sort.SliceStable(sorted, func(i, j int) bool { return verscmp.Compare(sorted[i], sorted[j]) > 0 })
		if strings.Join(sorted, "\n") != strings.Join(versions, "\n") {
			probs = append(probs, "version headings are not newest first")
		}
		lex := append([]string(nil), versions...)
		sort.Strings(lex)
		var dupes []string
		for i := 1; i < len(lex); i++ {
			if lex[i] == lex[i-1] && (len(dupes) == 0 || dupes[len(dupes)-1] != lex[i]) {
				dupes = append(dupes, lex[i])
			}
		}
		if len(dupes) != 0 {
			probs = append(probs, "duplicate headings: "+strings.Join(dupes, ","))
		}
	}

	note := ""
	exempt := r.exemptReason("C5")
	newest, xy := "", ""
	if len(versions) != 0 {
		sorted := append([]string(nil), versions...)
		sort.SliceStable(sorted, func(i, j int) bool { return verscmp.Compare(sorted[i], sorted[j]) > 0 })
		newest = sorted[0]
	}
	if tag, ok := r.latestTag(); ok && tag != "" {
		// An automatic patch (vX.Y.Z, Z > 0) needs no heading of its own when
		// its X.Y is the X.Y of the newest heading: the hand-cut vX.Y.0 (or a
		// later hand-cut patch) already announced the line it belongs to. A
		// patch whose X.Y no heading carries is a hand-cut tag in disguise, and
		// so is every vX.Y.0, every vX.0.0 and every pre-release.
		if m := reNewestXY.FindStringSubmatch(newest); m != nil {
			xy = m[1] + "." + m[2]
		}
		tm := reTagFull.FindStringSubmatch(tag)
		patch := 0
		if tm != nil {
			patch, _ = strconv.Atoi(tm[3])
		}
		has := false
		for _, v := range versions {
			if v == tag {
				has = true
			}
		}
		switch {
		case has:
			note = "heading for the latest tag " + tag + " present"
		case tm != nil && patch > 0 && xy != "" && tm[1]+"."+tm[2] == xy:
			note = fmt.Sprintf("latest tag %s is an automatic patch of %s, the newest heading; it needs none", tag, newest)
		case exempt != "":
			note = fmt.Sprintf("no heading for the latest tag %s, exempt: %s", tag, exempt)
		case tm != nil && patch > 0 && newest != "":
			probs = append(probs, fmt.Sprintf("CHANGELOG.md has no heading for %s, and it is not an automatic patch: the newest heading is %s, not a v%s.x", tag, newest, xy))
		default:
			probs = append(probs, "CHANGELOG.md has no heading for "+tag)
		}
	} else {
		note = "no v* tag reachable from the default branch, so the latest-tag heading was not checked"
	}
	r.verdict("C5", fmt.Sprintf("%d version heading(s), newest first; %s", len(versions), note), probs...)
}

// C6 · devbox pins every package
func (r *run) c6() {
	if !r.exists("devbox.json") {
		r.emit("C6", "SKIP", "no devbox.json")
		return
	}
	var doc struct {
		Packages json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal([]byte(r.read("devbox.json")), &doc); err != nil {
		r.emit("C6", "FAIL", "devbox.json does not parse as JSON")
		return
	}
	var entries []string
	pk := strings.TrimSpace(string(doc.Packages))
	switch {
	case strings.HasPrefix(pk, "["):
		var arr []json.RawMessage
		_ = json.Unmarshal(doc.Packages, &arr)
		for _, a := range arr {
			var s string
			if json.Unmarshal(a, &s) == nil {
				entries = append(entries, s)
			} else {
				entries = append(entries, string(a))
			}
		}
	case strings.HasPrefix(pk, "{"):
		mem, _ := orderedMembers(doc.Packages)
		for _, m := range mem {
			v := ""
			var s string
			if json.Unmarshal(m.val, &s) == nil {
				v = s
			} else {
				var o struct {
					Version json.RawMessage `json:"version"`
				}
				if json.Unmarshal(m.val, &o) == nil {
					var vs string
					if json.Unmarshal(o.Version, &vs) == nil {
						v = vs
					} else if t := strings.TrimSpace(string(o.Version)); t != "" && t != "null" && t != "false" {
						v = t
					}
				}
			}
			entries = append(entries, m.key+"@"+v)
		}
	}
	var bare, latest []string
	flakes := 0
	for _, e := range entries {
		// A flake reference names its own revision (or deliberately does not);
		// "name@version" is not its shape, so it is counted and not judged.
		if strings.Contains(e, "#") || strings.HasPrefix(e, "github:") || strings.HasPrefix(e, "path:") || strings.HasPrefix(e, "./") || strings.HasPrefix(e, "/") {
			flakes++
			continue
		}
		at := strings.LastIndex(e, "@")
		if !strings.Contains(e, "@") || e[at+1:] == "" {
			bare = append(bare, strings.TrimSuffix(e, "@"))
		} else if e[at+1:] == "latest" {
			latest = append(latest, e[:at])
		}
	}
	var probs []string
	if len(bare) != 0 {
		probs = append(probs, fmt.Sprintf("%d package(s) with no version: %s", len(bare), strings.Join(bare, ", ")))
	}
	if len(latest) != 0 {
		probs = append(probs, fmt.Sprintf("%d package(s) pinned to latest: %s", len(latest), strings.Join(latest, ", ")))
	}
	ok := fmt.Sprintf("%d package(s) pinned to a version", len(entries))
	if flakes != 0 {
		ok += fmt.Sprintf(" (%d flake reference(s) not judged)", flakes)
	}
	r.verdict("C6", ok, probs...)
}

type member struct {
	key string
	val json.RawMessage
}

// orderedMembers reads a JSON object in order, which a Go map cannot.
func orderedMembers(raw []byte) ([]member, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out []member
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, member{k.(string), v})
	}
	return out, nil
}

// C7 · renovate.json extends the preset
func (r *run) c7() {
	f := r.firstExisting("renovate.json", ".github/renovate.json")
	if f == "" {
		if r.exists("renovate.json5") || r.exists(".github/renovate.json5") {
			r.emit("C7", "FAIL", "renovate.json5 is not the contract's shape; use renovate.json")
		} else {
			r.emit("C7", "FAIL", "renovate.json is missing")
		}
		return
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.read(f)), &doc); err != nil {
		r.emit("C7", "FAIL", f+" does not parse as JSON")
		return
	}
	var probs []string
	if _, ok := doc["$schema"]; !ok {
		probs = append(probs, f+" has no $schema")
	}
	extends := "[]"
	if e, ok := doc["extends"]; ok && string(e) != "null" && string(e) != "false" {
		var b strings.Builder
		var compact = new(jsonCompact)
		extends = compact.do(e, &b)
	}
	good := false
	var ext []json.RawMessage
	if e, ok := doc["extends"]; !ok || string(e) == "null" {
		good = false // an empty list is not exactly the preset
	} else if json.Unmarshal(e, &ext) == nil && len(ext) == 1 {
		var s string
		if json.Unmarshal(ext[0], &s) == nil && (s == r.o.RenovatePreset || strings.HasPrefix(s, r.o.RenovatePreset+"#")) {
			good = true
		}
	}
	if !good {
		probs = append(probs, fmt.Sprintf("extends is %s, not [\"%s\"]", extends, r.o.RenovatePreset))
	}
	// "Documented overrides only": a rule or manager carries its own
	// `description`; any other override key needs the top-level one.
	undocumented := 0
	for _, k := range []string{"packageRules", "customManagers"} {
		var items []map[string]json.RawMessage
		if json.Unmarshal(doc[k], &items) == nil {
			for _, it := range items {
				if _, ok := it["description"]; !ok {
					undocumented++
				}
			}
		}
	}
	if undocumented != 0 {
		probs = append(probs, fmt.Sprintf("%d packageRules/customManagers entr(ies) without a description", undocumented))
	}
	if _, ok := doc["description"]; !ok {
		var others []string
		for k := range doc {
			switch k {
			case "$schema", "extends", "description", "packageRules", "customManagers":
			default:
				others = append(others, k)
			}
		}
		sort.Strings(others)
		if len(others) != 0 {
			probs = append(probs, fmt.Sprintf("overrides (%s) with no top-level description", strings.Join(others, ",")))
		}
	}
	r.verdict("C7", fmt.Sprintf("%s extends %s, overrides documented", f, r.o.RenovatePreset), probs...)
}

// jsonCompact is `jq -c`: the compact text of a JSON value.
type jsonCompact struct{}

func (jsonCompact) do(raw json.RawMessage, _ *strings.Builder) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

// C8 · README headings, in order
var reH2Text = regexp.MustCompile(`^## (.*[^ \t\n\v\f\r])[ \t\n\v\f\r]*$`)

func (r *run) c8() {
	if !r.exists("README.md") {
		r.emit("C8", "FAIL", "README.md is missing")
		return
	}
	var heads []string
	for _, l := range lines(r.read("README.md")) {
		if m := reH2Text.FindStringSubmatch(l); m != nil {
			heads = append(heads, m[1])
		}
	}
	var probs, missing []string
	last, lastName := -1, ""
	for _, h := range readmeHeadings {
		pos := -1
		for i, x := range heads {
			if x == h {
				pos = i
				break
			}
		}
		if pos < 0 {
			missing = append(missing, h)
			continue
		}
		if pos < last {
			probs = append(probs, fmt.Sprintf("'%s' comes before '%s'", h, lastName))
		}
		last, lastName = pos, h
	}
	if len(missing) != 0 {
		var q []string
		for _, m := range missing {
			q = append(q, "'"+m+"'")
		}
		probs = append([]string{"missing " + strings.Join(q, ", ")}, probs...)
	}
	r.verdict("C8", fmt.Sprintf("all %d headings present, in order", len(readmeHeadings)), probs...)
}

// C9 · MIT licence
var reMIT = regexp.MustCompile(`MIT License|Permission is hereby granted, free of charge`)

func (r *run) c9() {
	exempt := r.exemptReason("C9")
	f := r.firstExisting("LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE")
	switch {
	case f == "":
		if exempt != "" {
			r.emit("C9", "EXEMPT", "LICENSE is missing; exempt: "+exempt)
		} else {
			r.emit("C9", "FAIL", "LICENSE is missing")
		}
	case reMIT.MatchString(r.read(f)):
		r.emit("C9", "PASS", f+" is MIT")
	case exempt != "":
		r.emit("C9", "EXEMPT", f+" is not the MIT licence; exempt: "+exempt)
	default:
		r.emit("C9", "FAIL", f+" is not the MIT licence")
	}
}

// C12 · install pins a version
func (r *run) c12() {
	if !r.exists("README.md") {
		r.emit("C12", "FAIL", "README.md is missing")
		return
	}
	// An install command is what this rule means: `@latest` inside a fenced
	// code block. Prose that warns against it is not a command and is not what
	// a reader copies, so it does not count.
	var nums []string
	fence := false
	for i, l := range lines(r.read("README.md")) {
		if strings.HasPrefix(l, "```") {
			fence = !fence
			continue
		}
		if fence && strings.Contains(l, "@latest") {
			nums = append(nums, strconv.Itoa(i+1))
		}
	}
	if len(nums) != 0 {
		r.emit("C12", "FAIL", fmt.Sprintf("README.md installs @latest (line %s)", strings.Join(nums, ",")))
	} else {
		r.emit("C12", "PASS", "README.md never installs @latest")
	}
}
