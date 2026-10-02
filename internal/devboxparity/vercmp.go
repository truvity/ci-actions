package devboxparity

import "strings"

// versionCompare orders two version strings the way `sort -V` does for the
// shapes that occur here (1.25, 1.25.4, 1.26rc1): digit runs compare as
// numbers, other runs as text, and a version that is a prefix of another is
// the lesser.
func versionCompare(a, b string) int {
	for a != "" && b != "" {
		da, db := isDigit(a[0]), isDigit(b[0])
		switch {
		case da && db:
			na, ra := splitRun(a, true)
			nb, rb := splitRun(b, true)
			na, nb = strings.TrimLeft(na, "0"), strings.TrimLeft(nb, "0")
			if len(na) != len(nb) {
				if len(na) < len(nb) {
					return -1
				}
				return 1
			}
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
			a, b = ra, rb
		case !da && !db:
			ta, ra := splitRun(a, false)
			tb, rb := splitRun(b, false)
			if ta != tb {
				if ta < tb {
					return -1
				}
				return 1
			}
			a, b = ra, rb
		case da:
			return 1
		default:
			return -1
		}
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	}
	return 1
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// splitRun cuts the leading run of digits (or of non-digits).
func splitRun(s string, digits bool) (string, string) {
	i := 0
	for i < len(s) && isDigit(s[i]) == digits {
		i++
	}
	return s[:i], s[i:]
}

// maxVersion is `printf '%s\n%s\n' a b | sort -V | tail -1`: on a tie the
// second argument, which is the same string anyway.
func maxVersion(a, b string) string {
	if versionCompare(a, b) > 0 {
		return a
	}
	return b
}

// firstTwo is `cut -d. -f1-2`.
func firstTwo(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, ".")
}
