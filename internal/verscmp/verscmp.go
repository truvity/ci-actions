// Package verscmp orders version strings the way `sort -V` does for the shapes
// the actions meet, and keeps `cut -d. -f1-2`.
package verscmp

import (
	"strconv"
	"strings"
)

// Compare orders two version strings. Two strings of the form
// v?X.Y.Z(-prerelease)?(+build)? follow semver precedence (a release ranks
// above its pre-releases, build metadata is ignored); anything else falls
// back to the natural ordering of compareNatural.
func Compare(a, b string) int {
	if sa, ok := parseSemver(a); ok {
		if sb, ok := parseSemver(b); ok {
			return compareSemver(sa, sb)
		}
	}
	return compareNatural(a, b)
}

// compareNatural orders two version strings the way `sort -V` does for the
// shapes that occur here (1.25, 1.25.4, 1.26rc1): digit runs compare as
// numbers, other runs as text, and a version that is a prefix of another is
// the lesser.
func compareNatural(a, b string) int {
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

// Max is `printf '%s\n%s\n' a b | sort -V | tail -1`: on a tie the
// second argument, which is the same string anyway.
func Max(a, b string) string {
	if Compare(a, b) > 0 {
		return a
	}
	return b
}

// FirstTwo is `cut -d. -f1-2`.
func FirstTwo(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, ".")
}

type semver struct {
	num [3]uint64
	pre []string
}

// parseSemver accepts v?X.Y.Z(-prerelease)?(+build)?; a string that is not
// strictly that (a leading zero, an empty identifier, a short core) is not.
func parseSemver(v string) (semver, bool) {
	var s semver
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return s, false
	}
	for i, p := range parts {
		if !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return s, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return s, false
		}
		s.num[i] = n
	}
	if hasPre {
		s.pre = strings.Split(pre, ".")
		for _, id := range s.pre {
			if id == "" || !isIdent(id) || (isNumeric(id) && len(id) > 1 && id[0] == '0') {
				return s, false
			}
		}
	}
	return s, true
}

func compareSemver(a, b semver) int {
	for i := 0; i < 3; i++ {
		if a.num[i] != b.num[i] {
			if a.num[i] < b.num[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.pre == nil && b.pre == nil:
		return 0
	case a.pre == nil:
		return 1
	case b.pre == nil:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := comparePreIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// isIdent reports whether s is made of [0-9A-Za-z-] only.
func isIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isDigit(c) && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && c != '-' {
			return false
		}
	}
	return true
}

// comparePreIdent compares two pre-release identifiers: numeric ones
// numerically and below alphanumeric ones, alphanumeric ones in ASCII order.
func comparePreIdent(a, b string) int {
	na, nb := isNumeric(a), isNumeric(b)
	switch {
	case na && nb:
		x, errX := strconv.ParseUint(a, 10, 64)
		y, errY := strconv.ParseUint(b, 10, 64)
		if errX == nil && errY == nil {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}
		// Beyond uint64: longer is larger, then lexical.
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case na:
		return -1
	case nb:
		return 1
	}
	return strings.Compare(a, b)
}
