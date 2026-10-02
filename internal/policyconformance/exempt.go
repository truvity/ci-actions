package policyconformance

import (
	"regexp"
	"strings"
)

// exemptions · .github/policy-conformance.yaml
//
// A named, reviewed exception to one rule, for a repository kind the
// contract's plain text does not fit (a library chart has no values to schema;
// a CRD chart republished from upstream carries the upstream's own version; a
// fork of a non-MIT upstream cannot relicense; a CHANGELOG whose own preamble
// documents that a dependency-only patch carries no heading). This is not the
// caller-side `skip:` input: that silences a whole rule for one run and
// demands a reason every time. An exemption here is committed, reviewed, and,
// where the rule is chart-scoped, can name just the charts it covers rather
// than the whole repository.
//
// Shape (a restricted, line-oriented subset of YAML; no flow collections other
// than `charts: [a, b, c]`):
//
//	exempt:
//	  C2:
//	    reason: library chart takes no values
//	    charts: [gateway-routes]
//	  C9:
//	    reason: fork of an Apache-2.0 upstream; cannot relicense
//
// The file is read the way the shell version's awk read it, line by line: a
// column-0 line of any kind (a comment included) ends the `exempt:` block.
const exemptFile = ".github/policy-conformance.yaml"

var (
	reNonSpaceStart = regexp.MustCompile(`^[^ \t\n\v\f\r]`)
	reRuleLetter    = regexp.MustCompile(`^  [A-Za-z]`)
)

// exemptRule is the part of the exemption file under one rule id, line by
// line, with the awk program's state machine.
func (r *run) exemptScan(id string, visit func(line string) (done bool)) {
	data := r.read(exemptFile)
	if !r.exists(exemptFile) {
		return
	}
	ruleStart := regexp.MustCompile(`^  ` + regexp.QuoteMeta(id) + `:[ \t\n\v\f\r]*$`)
	inExempt, inRule := false, false
	for _, line := range lines(data) {
		if strings.HasPrefix(line, "exempt:") {
			inExempt = true
			continue
		}
		if inExempt && reNonSpaceStart.MatchString(line) {
			inExempt = false
		}
		if inExempt && ruleStart.MatchString(line) {
			inRule = true
			continue
		}
		if inExempt && inRule && reRuleLetter.MatchString(line) {
			inRule = false
		}
		if inExempt && inRule {
			if visit(line) {
				return
			}
		}
	}
}

// exemptReason is the reason string for a rule, or "" if it carries no
// exemption.
func (r *run) exemptReason(id string) string {
	reasonRe := regexp.MustCompile(`^    reason:[ \t\n\v\f\r]*`)
	out := ""
	r.exemptScan(id, func(line string) bool {
		if loc := reasonRe.FindStringIndex(line); loc != nil {
			out = line[loc[1]:]
			return true
		}
		return false
	})
	return out
}

// exemptList is the comma-separated members of a flow list (`key: [a, b]`)
// under a rule's exemption, spaces removed, or "" when the rule has no such
// line.
func (r *run) exemptList(id, key string) string {
	re := regexp.MustCompile(`^    ` + regexp.QuoteMeta(key) + `:[ \t\n\v\f\r]*\[`)
	out := ""
	r.exemptScan(id, func(line string) bool {
		if loc := re.FindStringIndex(line); loc != nil {
			v := line[loc[1]:]
			if i := strings.Index(v, "]"); i >= 0 {
				v = v[:i]
			}
			out = strings.ReplaceAll(v, " ", "")
			return true
		}
		return false
	})
	return out
}

// chartIn: true when chart is named in the comma-separated list, or when the
// list is empty (an unscoped, whole-repository exemption).
func chartIn(list, chart string) bool {
	if list == "" {
		return true
	}
	return strings.Contains(","+list+",", ","+chart+",")
}

type c13Entry struct {
	form, checks, paths, reason string
}

// c13Entries reads the C13 exemption entries: form is `list` for a
// `- checks: ...` entry and `block` for the older single block of keys
// directly under `C13:`. checks and paths are comma-joined, one pair of
// matching quotes stripped from each item. A list entry may carry no reason;
// the caller refuses it.
func (r *run) c13Entries() []c13Entry {
	if !r.exists(exemptFile) {
		return nil
	}
	var out []c13Entry
	var cur c13Entry
	have := false
	flush := func() {
		if have {
			out = append(out, cur)
		}
		have = false
		cur = c13Entry{}
	}
	kv := func(s string) {
		k := s
		if i := strings.Index(k, ":"); i >= 0 {
			k = k[:i]
		}
		v := s
		if i := strings.Index(v, ":"); i >= 0 {
			v = strings.TrimLeft(v[i+1:], " \t")
		} else {
			v = s
		}
		switch k {
		case "reason":
			cur.reason = unq(v)
		case "checks":
			cur.checks = flow(v)
		case "paths":
			cur.paths = flow(v)
		}
	}
	reComment := regexp.MustCompile(`^[ \t]*#`)
	reDash := regexp.MustCompile(`^    -[ \t]*`)
	reKey4 := regexp.MustCompile(`^    [A-Za-z]`)
	reKey6 := regexp.MustCompile(`^      [A-Za-z]`)
	inExempt, inRule := false, false
	for _, line := range lines(r.read(exemptFile)) {
		if reComment.MatchString(line) {
			continue
		}
		if strings.HasPrefix(line, "exempt:") {
			inExempt = true
			continue
		}
		if inExempt && reNonSpaceStart.MatchString(line) {
			inExempt = false
		}
		if inExempt && regexp.MustCompile(`^  C13:[ \t]*$`).MatchString(line) {
			inRule = true
			continue
		}
		if inExempt && inRule && reRuleLetter.MatchString(line) {
			flush()
			inRule = false
		}
		if inExempt && inRule {
			switch {
			case reDash.MatchString(line):
				flush()
				have = true
				cur.form = "list"
				if s := reDash.ReplaceAllString(line, ""); s != "" {
					kv(s)
				}
			case reKey4.MatchString(line):
				if !have {
					have = true
					cur.form = "block"
				}
				kv(strings.TrimLeft(line, " "))
			case reKey6.MatchString(line):
				kv(strings.TrimLeft(line, " "))
			}
		}
	}
	flush()
	return out
}

// unq strips surrounding whitespace and one pair of matching quotes.
func unq(v string) string {
	v = strings.Trim(v, " \t")
	if len(v) >= 2 {
		f := v[0]
		if (f == '"' || f == '\'') && v[len(v)-1] == f {
			v = v[1 : len(v)-1]
		}
	}
	return v
}

// flow is the comma-joined items of `[a, b, c]`, each unquoted, empties dropped.
func flow(v string) string {
	v = regexp.MustCompile(`^[ \t]*\[`).ReplaceAllString(v, "")
	if i := strings.Index(v, "]"); i >= 0 {
		v = v[:i]
	}
	var out []string
	for _, it := range strings.Split(v, ",") {
		if it = unq(it); it != "" {
			out = append(out, it)
		}
	}
	return strings.Join(out, ",")
}
