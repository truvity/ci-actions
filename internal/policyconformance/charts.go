package policyconformance

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const ws = ` \t\n\v\f\r`

// topKey is a top-level scalar from a YAML file, quotes stripped. Chart.yaml
// keeps `version:` and `appVersion:` at column 0, which is all this reads. The
// first line that starts with the key wins.
func topKey(content, key string) string {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(key) + `:[` + ws + `]*["']?([^"'#` + ws + `]*)["']?.*$`)
	for _, l := range lines(content) {
		if m := re.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

// chartMirror is the upstream a MIRROR chart republishes, from the Chart.yaml
// annotation `truvity.io/mirror: "<owner>/<repo>@<version>"` (C1). It is the
// whole value; empty when the chart does not declare one.
func chartMirror(content string) string {
	re := regexp.MustCompile(`^[` + ws + `]+truvity\.io/mirror:[` + ws + `]*["']?([^"'#` + ws + `]*)["']?.*$`)
	for _, l := range lines(content) {
		if m := re.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

var (
	reKos      = regexp.MustCompile(`(?m)^kos:`)
	reBuildsID = regexp.MustCompile(`^\s*-\s*id:`)
	reSkipTrue = regexp.MustCompile(`^\s*-?\s*skip:\s*true\s*$`)
	reBlank    = regexp.MustCompile(`^\s*$`)
	reMirror   = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+@[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

// awkBlock is `awk '/^<head>:/{f=1; next} /^[A-Za-z_]+:/{f=0} f' file`: the
// lines after a top-level key up to the next top-level key.
func awkBlock(content, head string) []string {
	hre := regexp.MustCompile(`^` + regexp.QuoteMeta(head) + `:`)
	kre := regexp.MustCompile(`^[A-Za-z_]+:`)
	var out []string
	f := false
	for _, l := range lines(content) {
		if hre.MatchString(l) {
			f = true
			continue
		}
		if kre.MatchString(l) {
			f = false
		}
		if f {
			out = append(out, l)
		}
	}
	return out
}

// repoShipsImage: best-effort, does this repository build and publish an
// image at all? C1 only asks appVersion to be the tag placeholder "when the
// repo ships an image": a chart-only repository (goreleaser's builds all
// `skip: true`, no ko section, no Dockerfile) has no image, so an appVersion
// it carries names something else (an upstream compatibility pin) and is out
// of scope.
func (r *run) repoShipsImage() bool {
	gf := r.firstExisting(".goreleaser.yaml", ".goreleaser.yml")
	if gf == "" {
		return r.exists("Dockerfile") || r.exists(".ko.yaml")
	}
	content := r.read(gf)
	if reKos.MatchString(content) {
		return true
	}
	if r.exists("Dockerfile") {
		return true
	}
	block := awkBlock(content, "builds")
	// a block of only blank lines is empty to a command substitution
	nonEmpty := strings.TrimRight(strings.Join(block, "\n"), "\n") != ""
	if !nonEmpty {
		return true
	}
	for _, l := range block {
		if reBuildsID.MatchString(l) {
			return true
		}
	}
	hasSkip, other := false, false
	for _, l := range block {
		switch {
		case reSkipTrue.MatchString(l):
			hasSkip = true
		case reBlank.MatchString(l):
		default:
			other = true
		}
	}
	if hasSkip && !other {
		return false
	}
	return true
}

func chartName(chartYAML string) string {
	n := strings.TrimPrefix(chartYAML, "charts/")
	return strings.TrimSuffix(n, "/Chart.yaml")
}

func (r *run) c1() {
	charts := r.glob("charts/*/Chart.yaml")
	if len(charts) == 0 {
		r.emit("C1", "SKIP", "no charts/*/Chart.yaml")
		return
	}
	exempt := r.exemptReason("C1")
	exemptList := ""
	if exempt != "" {
		exemptList = r.exemptList("C1", "charts")
	}
	shipsImage := r.repoShipsImage()
	var probs []string
	mirrors := 0
	for _, f := range charts {
		name := chartName(f)
		if exempt != "" && chartIn(exemptList, name) {
			continue
		}
		content := r.read(f)
		v := topKey(content, "version")
		if mirror := chartMirror(content); mirror != "" {
			// A mirror chart republishes a third-party artifact unchanged, so
			// its version IS the upstream's, declared once in the annotation;
			// version and appVersion must equal it. Everything else stays 0.0.0.
			mirrors++
			if !reMirror.MatchString(mirror) {
				probs = append(probs, fmt.Sprintf("charts/%s truvity.io/mirror is '%s', not <owner>/<repo>@<version>", name, mirror))
				continue
			}
			mv := mirror[strings.LastIndex(mirror, "@")+1:]
			if v != mv {
				probs = append(probs, fmt.Sprintf("charts/%s version is %s, not the mirrored %s", name, orStr(v, "absent"), mv))
			}
			if hasLine(content, "appVersion:") {
				if av := topKey(content, "appVersion"); av != mv {
					probs = append(probs, fmt.Sprintf("charts/%s appVersion is %s, not the mirrored %s", name, orStr(av, "empty"), mv))
				}
			}
			continue
		}
		if v != "0.0.0" {
			probs = append(probs, fmt.Sprintf("charts/%s version is %s, not 0.0.0", name, orStr(v, "absent")))
		}
		// appVersion is optional (a chart that ships no image has none), but
		// when it is committed AND the repo ships an image it is the same
		// placeholder the tag replaces.
		if shipsImage && hasLine(content, "appVersion:") {
			if av := topKey(content, "appVersion"); av != "0.0.0" {
				probs = append(probs, fmt.Sprintf("charts/%s appVersion is %s, not 0.0.0", name, orStr(av, "empty")))
			}
		}
	}
	ok := fmt.Sprintf("%d chart(s) commit version 0.0.0", len(charts))
	if mirrors != 0 {
		ok = fmt.Sprintf("%d chart(s) commit version 0.0.0, %d mirror chart(s) at the upstream version", len(charts)-mirrors, mirrors)
	}
	if exempt != "" {
		ok += fmt.Sprintf(" (exempt: %s: %s)", exemptList, exempt)
	}
	r.verdict("C1", ok, probs...)
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// hasLine is `grep -qE '^<prefix>'`.
func hasLine(content, prefix string) bool {
	for _, l := range lines(content) {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func (r *run) c2() {
	dirs := r.glob("charts/*/Chart.yaml")
	if len(dirs) == 0 {
		r.emit("C2", "SKIP", "no charts/*/Chart.yaml")
		return
	}
	exempt := r.exemptReason("C2")
	exemptList := ""
	if exempt != "" {
		exemptList = r.exemptList("C2", "charts")
	}
	var probs []string
	for _, f := range dirs {
		d := strings.TrimSuffix(f, "/Chart.yaml")
		name := strings.TrimPrefix(d, "charts/")
		if exempt != "" && chartIn(exemptList, name) {
			continue
		}
		if !r.exists(d + "/values.schema.json") {
			probs = append(probs, d+" has no values.schema.json")
		}
	}
	ok := fmt.Sprintf("%d chart(s) carry values.schema.json", len(dirs))
	if exempt != "" {
		ok += fmt.Sprintf(" (exempt: %s: %s)", exemptList, exempt)
	}
	r.verdict("C2", ok, probs...)
}

// hasFiles: the directory exists and holds at least one regular file.
func (r *run) hasFiles(dir string) bool {
	if !r.isDir(dir) {
		return false
	}
	found := false
	_ = filepath.WalkDir(r.path(dir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func (r *run) c3() {
	dirs := r.glob("charts/*/Chart.yaml")
	if len(dirs) == 0 {
		r.emit("C3", "SKIP", "no charts/*/Chart.yaml")
		return
	}
	// The Go alternative: a chart test under charts/ that exercises an invalid
	// values file. Textual: it proves the test names such a fixture, not that
	// the assertion is right.
	gotests := ""
	if r.isDir("charts") {
		re := regexp.MustCompile(`(?i)invalid`)
		var found []string
		_ = filepath.WalkDir(r.path("charts"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && strings.HasSuffix(d.Name(), "_test.go") {
				if b, rerr := os.ReadFile(p); rerr == nil && re.Match(b) {
					rel, _ := filepath.Rel(r.o.Dir, p)
					found = append(found, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		sort.Strings(found)
		if len(found) > 0 {
			gotests = found[0]
		}
	}
	var probs []string
	for _, f := range dirs {
		name := chartName(f)
		if r.hasFiles("tests/golden/"+name) && r.hasFiles("tests/invalid/"+name) {
			continue
		}
		if gotests != "" {
			continue
		}
		if !r.hasFiles("tests/golden/" + name) {
			probs = append(probs, fmt.Sprintf("no golden renders under tests/golden/%s/", name))
		}
		if !r.hasFiles("tests/invalid/" + name) {
			probs = append(probs, fmt.Sprintf("no negative fixture under tests/invalid/%s/", name))
		}
	}
	how := "tests/golden + tests/invalid"
	if gotests != "" {
		how += " or " + gotests
	}
	r.verdict("C3", fmt.Sprintf("%d chart(s) covered by %s", len(dirs), how), probs...)
}
