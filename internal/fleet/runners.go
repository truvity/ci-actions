package fleet

import (
	"regexp"
	"sort"
	"strings"
)

// Runner kinds a job's `runs-on:` resolves to. Reporting only: nothing
// gates on them.
const (
	RunnerHosted     = "hosted"
	RunnerSelfHosted = "self-hosted"
	RunnerDynamic    = "dynamic" // an expression: the label is not known from the file
	RunnerMixed      = "mixed"   // a repository whose jobs span more than one kind
)

var hostedLabelRe = regexp.MustCompile(`^(ubuntu|windows|macos)-`)

// ParseRunsOn returns, for every `runs-on:` of a workflow, the labels it
// names (one slice per job). It reads lines rather than YAML, like
// ParseUses: scalar, flow list, block list, and the `group:`/`labels:`
// mapping are all understood. A runner group is somebody's own pool.
func ParseRunsOn(content string) [][]string {
	lines := strings.Split(content, "\n")
	var out [][]string
	for i := 0; i < len(lines); i++ {
		line := stripComment(strings.TrimRight(lines[i], "\r"))
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "runs-on:") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		val := strings.TrimSpace(strings.TrimPrefix(trim, "runs-on:"))
		if val != "" {
			if l := scalarOrFlow(val); len(l) > 0 {
				out = append(out, l)
			}
			continue
		}
		var labels []string
		for i+1 < len(lines) {
			next := stripComment(strings.TrimRight(lines[i+1], "\r"))
			nt := strings.TrimSpace(next)
			if nt == "" {
				i++
				continue
			}
			ni := len(next) - len(strings.TrimLeft(next, " "))
			if ni < indent || (ni == indent && !strings.HasPrefix(nt, "- ")) {
				break
			}
			i++
			switch {
			case strings.HasPrefix(nt, "- "):
				labels = append(labels, scalarOrFlow(strings.TrimSpace(nt[2:]))...)
			case strings.HasPrefix(nt, "group:"):
				if g := unquote(strings.TrimSpace(strings.TrimPrefix(nt, "group:"))); g != "" {
					labels = append(labels, "group:"+g)
				}
			case strings.HasPrefix(nt, "labels:"):
				labels = append(labels, scalarOrFlow(strings.TrimSpace(strings.TrimPrefix(nt, "labels:")))...)
			}
		}
		if len(labels) > 0 {
			out = append(out, labels)
		}
	}
	return out
}

func stripComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		return s[:i]
	}
	return s
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func scalarOrFlow(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		var out []string
		for _, p := range strings.Split(v[1:len(v)-1], ",") {
			if p = unquote(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return []string{unquote(v)}
}

// JobRunnerKind classifies one job's labels. A job is self-hosted when
// any label is not one of GitHub's three hosted platforms (the rule the
// public-runners action applies), dynamic when an expression hides the
// label and nothing else gives it away, hosted otherwise.
func JobRunnerKind(labels []string) string {
	dynamic := false
	for _, l := range labels {
		switch {
		case strings.Contains(l, "${{"):
			dynamic = true
		case hostedLabelRe.MatchString(l):
		default:
			return RunnerSelfHosted
		}
	}
	if dynamic {
		return RunnerDynamic
	}
	return RunnerHosted
}

// summariseRunners folds the kinds of every job of a repository into one
// cell: the single kind, "mixed" when there are several, empty when no
// job of the repository's own workflows names a runner (a caller of a
// reusable workflow has its runs-on in the pinned file).
func summariseRunners(kinds map[string]bool) string {
	switch len(kinds) {
	case 0:
		return ""
	case 1:
		for k := range kinds {
			return k
		}
	}
	return RunnerMixed
}

func sortedLabels(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// matchesRunnerFilter: "" and "any" show everything; "self-hosted" shows
// repositories with at least one self-hosted job; "hosted" those with only
// hosted jobs.
func matchesRunnerFilter(r RepoResult, filter string) bool {
	switch filter {
	case "", "any":
		return true
	case RunnerSelfHosted:
		for _, k := range r.RunnerKinds {
			if k == RunnerSelfHosted {
				return true
			}
		}
		return false
	case RunnerHosted:
		return len(r.RunnerKinds) == 1 && r.RunnerKinds[0] == RunnerHosted
	}
	return true
}
