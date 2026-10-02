package fleet

import (
	"regexp"
	"strings"
)

// Use is one `uses:` reference parsed out of a workflow file.
type Use struct {
	Owner, Repo string // "truvity", "ci-actions"
	Path        string // "setup-devbox" or ".github/workflows/check.yaml"; empty for a repo root action
	Ref         string // the text after @
	Hint        string // the trailing `# v1.6.1` comment, which is a wish, not a fact
	Local       string // for `uses: ./path`, the path
	Line        int
}

// FullRepo is owner/repo.
func (u Use) FullRepo() string { return u.Owner + "/" + u.Repo }

// IsSHA reports a 40-hex ref, the only kind of pin that names a commit.
func (u Use) IsSHA() bool { return shaRe.MatchString(u.Ref) }

var (
	shaRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	usesRe = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(?:"([^"]*)"|'([^']*)'|([^\s#]+))\s*(?:#\s*(\S.*?))?\s*$`)
)

// ParseUses returns every `uses:` of a workflow or action file, in order.
// It reads lines rather than YAML: a `uses:` is always a one-line scalar,
// a commented-out one must not count, and a file that does not parse as
// YAML should still be judged.
func ParseUses(content string) []Use {
	var out []Use
	for i, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		m := usesRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		spec := m[1] + m[2] + m[3]
		hint := strings.Fields(m[4])
		u := Use{Line: i + 1}
		if len(hint) > 0 {
			u.Hint = hint[0]
		}
		switch {
		case strings.HasPrefix(spec, "./"):
			u.Local = spec
		case strings.HasPrefix(spec, "docker://"):
			continue
		default:
			at := strings.LastIndex(spec, "@")
			if at < 0 {
				continue
			}
			u.Ref = spec[at+1:]
			parts := strings.SplitN(spec[:at], "/", 3)
			if len(parts) < 2 {
				continue
			}
			u.Owner, u.Repo = parts[0], parts[1]
			if len(parts) == 3 {
				u.Path = parts[2]
			}
		}
		out = append(out, u)
	}
	return out
}
