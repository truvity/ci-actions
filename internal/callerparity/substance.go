package callerparity

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// WHAT IS COMPARED: SUBSTANCE, NOT BYTES.
//
// Each rule here is an exemption, and each one is deliberate:
//
//	comments      a repository explains itself in its own words. Four prose
//	              variants of security.yaml across eleven repositories were
//	              all the same workflow.
//	blank lines   follow the comments they separated.
//	cron          THE SCHEDULE IS STAGGERED PER REPOSITORY ON PURPOSE:
//	              repositories that tag in the same minute produce downstream
//	              pin pull requests that race each other's rebases. Comparing
//	              it would report every repository as differing and teach
//	              everyone to ignore the check.
//	this library's pinned ref
//	              renovate moves `uses: <owner>/<repo>/.github/workflows/
//	              <file>@<sha>` in each repository on its own schedule, so
//	              between a release here and renovate's sweep there the estate
//	              is legitimately spread across two pins. The pin has its own
//	              keeper; this check is about shape. Only reusable-workflow
//	              refs are exempt: a third-party action pin inside a caller IS
//	              compared.
//
// Comments are stripped textually, so a `#` inside a quoted scalar would be
// cut with them. No kit carries one; do not add one.
var (
	commentRe = regexp.MustCompile(`[ \t\n\v\f\r]*#.*$`)
	blankRe   = regexp.MustCompile(`^[ \t\n\v\f\r]*$`)
	cronRe    = regexp.MustCompile(`^[ \t\n\v\f\r]*-[ \t\n\v\f\r]*cron:`)
	pinRe     = regexp.MustCompile(`(uses:[ \t\n\v\f\r]*[^@ \t\n\v\f\r]*/\.github/workflows/[^@ \t\n\v\f\r]*)@[0-9a-f]{40}`)
)

// substance is the sed pipeline, line by line. The missing final newline of
// the input is kept as sed keeps it.
func substance(in []byte) []byte {
	s := string(in)
	endsNL := strings.HasSuffix(s, "\n")
	if endsNL {
		s = s[:len(s)-1]
	}
	if s == "" && !endsNL {
		return nil
	}
	lines := strings.Split(s, "\n")
	var out bytes.Buffer
	for i, l := range lines {
		l = commentRe.ReplaceAllString(l, "")
		if blankRe.MatchString(l) || cronRe.MatchString(l) {
			continue
		}
		if loc := pinRe.FindStringSubmatchIndex(l); loc != nil {
			l = l[:loc[0]] + l[loc[2]:loc[3]] + "@<pinned>" + l[loc[1]:]
		}
		out.WriteString(l)
		if i < len(lines)-1 || endsNL {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// subtree is one subtree of a YAML document, as canonical JSON: keys
// sorted, ARRAYS sorted, comments gone, indentation irrelevant. What a
// copied BLOCK has to keep is its data: the depguard `deny:` list is a set
// of bans, not a sequence, and a repository that pasted it back with its
// entries in a different order still has the same bans. A path that matches
// nothing, or a document that does not parse, is `null`, which differs from
// any kit: a repository that dropped the block has fallen behind it.
//
// path is the dotted form the manifest uses (`.linters.settings.depguard`,
// `.depguard`, `.`).
func subtree(doc []byte, path string) []byte {
	var root any
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return []byte("null\n")
	}
	cur := root
	for _, key := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		if key == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			cur = nil
			break
		}
		cur = m[key]
	}
	cur = canonical(cur)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cur); err != nil {
		return []byte("null\n")
	}
	return b.Bytes()
}

// canonical sorts every array, bottom up (jq's `walk(sort)`), in jq's own
// order, and turns maps with non-string keys into string-keyed ones.
func canonical(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = canonical(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[toString(k)] = canonical(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonical(e)
		}
		sort.SliceStable(out, func(i, j int) bool { return jqCompare(out[i], out[j]) < 0 })
		return out
	}
	return v
}

func toString(k any) string {
	b, _ := json.Marshal(k)
	return strings.Trim(string(b), `"`)
}

// jqCompare is jq's ordering: null < false < true < numbers < strings <
// arrays < objects; arrays element by element, objects by their sorted key
// lists first and then their values in key order.
func jqCompare(a, b any) int {
	ra, rb := jqRank(a), jqRank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch x := a.(type) {
	case bool:
		if x == b.(bool) {
			return 0
		}
		if !x {
			return -1
		}
		return 1
	case string:
		return strings.Compare(x, b.(string))
	case []any:
		y := b.([]any)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := jqCompare(x[i], y[i]); c != 0 {
				return c
			}
		}
		return len(x) - len(y)
	case map[string]any:
		y := b.(map[string]any)
		ka, kb := sortedKeys(x), sortedKeys(y)
		for i := 0; i < len(ka) && i < len(kb); i++ {
			if c := strings.Compare(ka[i], kb[i]); c != 0 {
				return c
			}
		}
		if len(ka) != len(kb) {
			return len(ka) - len(kb)
		}
		for _, k := range ka {
			if c := jqCompare(x[k], y[k]); c != 0 {
				return c
			}
		}
		return 0
	case nil:
		return 0
	}
	fa, fb := toFloat(a), toFloat(b)
	switch {
	case fa < fb:
		return -1
	case fa > fb:
		return 1
	}
	return 0
}

func jqRank(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if !x {
			return 1
		}
		return 2
	case string:
		return 4
	case []any:
		return 5
	case map[string]any:
		return 6
	}
	return 3
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}
