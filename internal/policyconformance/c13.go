package policyconformance

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// C13 · estate facts are inputs, never defaults
//
// A value only one estate would choose, written as a default: in a chart's
// values.yaml or values.schema.json, or in a Go or TypeScript constant. The
// check is deliberately narrow: it names five shapes it can tell from neutral
// text with confidence, and a reader still judges the rest:
//
//	domain   an organisation domain (the org name + .com/.xyz/.co/.private), in
//	         a chart default or a code string
//	tenancy  the organisation's tenancy API group
//	env      a real cluster or environment name (kernel, devel, stage, prod)
//	         as the default of a key or identifier that is named for one (env,
//	         environment, cluster, stage, tier): `env: prod`,
//	         `flag.String("cluster", "kernel", ...)`. A comparison or a case
//	         label is a test of the name, not a default, and is not flagged
//	region   a real cloud region (eu-west-1 ...) as a chart default, or as the
//	         default of an identifier named for one
//	ticket   an internal ticket key anywhere in a tracked file: a key is an
//	         internal name, and the public history keeps it for ever
//
// Neutral placeholders (example.com, eu-example-1) match none of these.
// Comments are not defaults and are not read, except by `ticket`, which reads
// everything. Test, fixture and golden directories, generated code and
// `*_test.go`/`*.test.ts` are not read: they may name anything.
//
// An exemption (.github/policy-conformance.yaml) may narrow this rule: each
// entry is its own pair of checks and paths; the older single block still
// works, but its checks and paths combine as a cross product.
const (
	rplS = `(af|ap|ca|eu|il|me|mx|sa|us)-(central|north|northeast|northwest|south|southeast|southwest|east|west)-[0-9]`
	envv = `(kernel|devel|stage|prod)`
	notq = "[^\"'`{]*"
	qS   = "[\"'`]"
)

// The organisation's name, assembled so this file is not itself a hit of the
// rule it implements, nor of the repository's own canary.
var orgName = "truv" + "ity"

var (
	reDomain  = regexp.MustCompile(`(^|[^A-Za-z0-9])` + orgName + `[.](com|xyz|co|private)([^A-Za-z0-9]|$)`)
	reTenancy = regexp.MustCompile(`tenancy[.]` + orgName + `[.]io`)
	reRegion  = regexp.MustCompile(`(^|[^A-Za-z0-9])` + rplS + `([^0-9A-Za-z]|$)`)
	reEnvKey  = regexp.MustCompile(`^(env|environment|cluster|clustername|cluster_name|clusterid|stage|tier)$`)
	reEnvVal  = regexp.MustCompile(`^` + envv + `$`)

	reYamlComment = regexp.MustCompile(`(^|[ \t])#.*$`)
	reYamlBlank   = regexp.MustCompile(`^[ \t]*$`)
	reYamlKV      = regexp.MustCompile(`^[ \t-]*[A-Za-z_][A-Za-z0-9_]*:[ \t]*["']?[A-Za-z]+["']?[ \t]*$`)
	reYamlLead    = regexp.MustCompile(`^[ \t-]*`)
	reYamlVal     = regexp.MustCompile(`^[^:]*:[ \t]*`)
	reYamlStrip   = regexp.MustCompile(`["' \t]`)

	reJSONKey      = regexp.MustCompile(`^[ \t]*"[^"]+"[ \t]*:[ \t]*\{`)
	reJSONDefault  = regexp.MustCompile(`"default"[ \t]*:`)
	reJSONDefLead  = regexp.MustCompile(`^.*"default"[ \t]*:[ \t]*`)
	reJSONStrip    = regexp.MustCompile(`[",' \t]`)
	reCodeComment  = regexp.MustCompile(`^[ \t]*(//|/\*|\*|#)`)
	reCodeTrail    = regexp.MustCompile(`[ \t]//.*$`)
	reCodeCompare  = regexp.MustCompile(`==|!=|(^|[ \t])case[ \t]`)
	reCodeEnv      = regexp.MustCompile(`(env|environment|cluster|stage|tier)[a-z0-9_]*` + qS + `?` + notq + qS + envv + qS)
	reCodeRegion   = regexp.MustCompile(`region[a-z0-9_]*` + qS + `?` + notq + qS + rplS + qS)
	reTicketPrefix = regexp.MustCompile(`^[^:]*:[^:]*:`)
)

type finding struct {
	file string
	line int
	kind string
	text string
}

func hit(kind, file string, fnr int, text string) finding {
	text = strings.Trim(text, " \t")
	if len(text) > 90 {
		text = text[:87] + "..."
	}
	return finding{file, fnr, kind, text}
}

func generic(l, orig, file string, fnr int, out *[]finding) {
	if reDomain.MatchString(l) {
		*out = append(*out, hit("domain", file, fnr, orig))
	}
	if reTenancy.MatchString(l) {
		*out = append(*out, hit("tenancy", file, fnr, orig))
	}
	if reRegion.MatchString(l) {
		*out = append(*out, hit("region", file, fnr, orig))
	}
}

// scanYAML is the awk program's `yaml` mode over one file.
func scanYAML(file string, content string, out *[]finding) {
	for i, raw := range lines(content) {
		fnr := i + 1
		l := reYamlComment.ReplaceAllString(raw, "")
		if reYamlBlank.MatchString(l) {
			continue
		}
		generic(l, raw, file, fnr, out)
		if reYamlKV.MatchString(l) {
			k := reYamlLead.ReplaceAllString(l, "")
			v := k
			if j := strings.Index(k, ":"); j >= 0 {
				k = k[:j]
			}
			v = reYamlVal.ReplaceAllString(v, "")
			v = reYamlStrip.ReplaceAllString(v, "")
			if reEnvKey.MatchString(strings.ToLower(k)) && reEnvVal.MatchString(strings.ToLower(v)) {
				*out = append(*out, hit("env", file, fnr, raw))
			}
		}
	}
}

// scanJSON is the `json` mode. lastkey persists across files, as awk's
// variables do across the files of one invocation.
func scanJSON(file string, content string, lastkey *string, out *[]finding) {
	for i, raw := range lines(content) {
		fnr := i + 1
		if reJSONKey.MatchString(raw) {
			k := strings.TrimLeft(raw, " \t")
			k = k[1:]
			k = k[:strings.Index(k, `"`)]
			*lastkey = strings.ToLower(k)
		}
		if reJSONDefault.MatchString(raw) {
			generic(raw, raw, file, fnr, out)
			d := reJSONDefLead.ReplaceAllString(raw, "")
			d = reJSONStrip.ReplaceAllString(d, "")
			if reEnvKey.MatchString(*lastkey) && reEnvVal.MatchString(strings.ToLower(d)) {
				*out = append(*out, hit("env", file, fnr, raw))
			}
		}
	}
}

// scanCode is the `code` mode.
func scanCode(file string, content string, out *[]finding) {
	for i, raw := range lines(content) {
		fnr := i + 1
		if reCodeComment.MatchString(raw) {
			continue
		}
		l := reCodeTrail.ReplaceAllString(raw, "")
		if reDomain.MatchString(l) {
			*out = append(*out, hit("domain", file, fnr, raw))
		}
		if reTenancy.MatchString(l) {
			*out = append(*out, hit("tenancy", file, fnr, raw))
		}
		lc := strings.ToLower(l)
		if reCodeCompare.MatchString(lc) {
			continue
		}
		if reCodeEnv.MatchString(lc) {
			*out = append(*out, hit("env", file, fnr, raw))
		}
		if reCodeRegion.MatchString(lc) {
			*out = append(*out, hit("region", file, fnr, raw))
		}
	}
}

// pathIn: is the path covered by the shell globs in the comma-separated list?
// An empty list covers every path.
func pathIn(p, list string) bool {
	if list == "" {
		return true
	}
	for _, g := range strings.Split(list, ",") {
		if shellGlob(g, p) {
			return true
		}
	}
	return false
}

// shellGlob is a `case` pattern match: `*` crosses `/`, `?` is one character,
// `[...]` a class.
func shellGlob(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := strings.IndexByte(pattern[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			cls := pattern[i+1 : i+1+j]
			if strings.HasPrefix(cls, "!") {
				cls = "^" + cls[1:]
			}
			b.WriteString("[" + cls + "]")
			i += j + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile("(?s)" + b.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func (r *run) c13() {
	if _, err := r.git("rev-parse", "--git-dir"); err != nil {
		r.emit("C13", "SKIP", "not a git checkout, so there is no tracked-file list to read")
		return
	}
	out, _ := r.git("ls-files", "-z")
	var tracked []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			tracked = append(tracked, f)
		}
	}
	var yamls, jsons, codes []string
	for _, f := range tracked {
		slash := "/" + f
		skip := false
		for _, seg := range []string{"/tests/", "/test/", "/testdata/", "/fixtures/", "/golden/", "/node_modules/", "/vendor/"} {
			if strings.Contains(slash, seg) {
				skip = true
			}
		}
		if skip {
			continue
		}
		d := path.Dir(f)
		switch {
		case strings.HasSuffix(f, ".go"):
			if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, ".pb.go") || strings.HasSuffix(f, ".pb.gw.go") || strings.Contains(f, "zz_generated") {
				continue
			}
			codes = append(codes, f)
		case strings.HasSuffix(f, ".ts") || strings.HasSuffix(f, ".tsx"):
			skipTS := false
			for _, suf := range []string{".d.ts", ".test.ts", ".test.tsx", ".spec.ts", ".spec.tsx", ".gen.ts", "_pb.ts"} {
				if strings.HasSuffix(f, suf) {
					skipTS = true
				}
			}
			if skipTS {
				continue
			}
			codes = append(codes, f)
		case path.Base(f) == "values.yaml" || path.Base(f) == "values.yml":
			if r.exists(d + "/Chart.yaml") {
				yamls = append(yamls, f)
			}
		case path.Base(f) == "values.schema.json":
			if r.exists(d + "/Chart.yaml") {
				jsons = append(jsons, f)
			}
		}
	}
	var found []finding
	for _, f := range yamls {
		if b, err := os.ReadFile(r.path(f)); err == nil {
			scanYAML(f, string(b), &found)
		}
	}
	lastkey := ""
	for _, f := range jsons {
		if b, err := os.ReadFile(r.path(f)); err == nil {
			scanJSON(f, string(b), &lastkey, &found)
		}
	}
	for _, f := range codes {
		if b, err := os.ReadFile(r.path(f)); err == nil {
			scanCode(f, string(b), &found)
		}
	}
	// A ticket key is an internal name wherever it sits; git grep skips binary
	// files. The pattern is assembled so this file does not match it.
	key := "IN" + "F-[0-9]+"
	grepOut, _ := r.git("grep", "-nIE", "(^|[^A-Za-z0-9])"+key, "--", ".")
	for _, l := range lines(grepOut) {
		parts := strings.SplitN(l, ":", 3)
		if len(parts) < 3 {
			continue
		}
		ln := 0
		fmt.Sscanf(parts[1], "%d", &ln)
		text := reTicketPrefix.ReplaceAllString(l, "")
		// the shell version read each row with IFS=tab, which trims tabs at the ends
		found = append(found, finding{file: parts[0], line: ln, kind: "ticket", text: strings.Trim(text, "\t")})
	}

	var badent, probs []string
	type exe struct{ checks, paths, reason string }
	var ex []exe
	for _, e := range r.c13Entries() {
		if strings.TrimSpace(e.reason) == "" {
			// A block without a reason is ignored, as it always was; a list
			// entry is the documented form, so a missing reason is an error.
			if e.form == "list" {
				c, p := e.checks, e.paths
				if c == "" {
					c = "all"
				}
				if p == "" {
					p = "all"
				}
				badent = append(badent, fmt.Sprintf("an exempt: C13 entry (checks: %s; paths: %s) has no reason", c, p))
			}
			continue
		}
		ex = append(ex, exe{e.checks, e.paths, e.reason})
	}
	n, dropped := 0, 0
	for _, f := range found {
		hitEx := false
		for _, e := range ex {
			if !pathIn(f.file, e.paths) {
				continue
			}
			if e.checks != "" && !strings.Contains(","+e.checks+",", ","+f.kind+",") {
				continue
			}
			hitEx = true
			break
		}
		if hitEx {
			dropped++
			continue
		}
		var why string
		switch f.kind {
		case "domain":
			why = "an organisation domain is an estate fact, not a default"
		case "tenancy":
			why = "the organisation's tenancy API group is an estate fact, not a default"
		case "env":
			why = "a real cluster or environment name as a default"
		case "region":
			why = "a real cloud region as a default"
		case "ticket":
			why = "an internal ticket key"
		default:
			why = f.kind
		}
		// Every finding gets its own line, so the log carries file:line for
		// each; the verdict line summarises.
		r.printf("  C13 %s:%d: %s: %s\n", f.file, f.line, why, f.text)
		n++
		if n <= 3 {
			probs = append(probs, fmt.Sprintf("%s:%d (%s)", f.file, f.line, f.kind))
		}
	}
	if n > 3 {
		probs = []string{fmt.Sprintf("%d estate facts: %s and %d more, listed above", n, strings.Join(probs[:3], " "), n-3)}
	}
	ok := fmt.Sprintf("no estate fact as a default in %d values file(s), %d schema(s), %d Go/TS file(s); no ticket key", len(yamls), len(jsons), len(codes))
	if dropped != 0 {
		var rs []string
		for _, e := range ex {
			rs = append(rs, e.reason)
		}
		ok += fmt.Sprintf(" (%d finding(s) exempt: %s)", dropped, strings.Join(rs, " "))
	}
	r.verdict("C13", ok, append(badent, probs...)...)
}
