// Package semver holds the two version orderings this tool needs: GNU
// `sort -V` order over arbitrary tag names (what tagged-pins used to get
// from the shell) and strict vX.Y.Z parsing for the fleet gate.
package semver

import (
	"strconv"
	"strings"
)

// Version is a parsed vX.Y.Z[-pre] tag.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// Parse reads vX.Y.Z (the leading v is optional, a -prerelease suffix is
// kept). Anything else is not a version.
func Parse(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p == "" {
			return Version{}, false
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2], pre}, true
}

// Compare orders versions; a prerelease sorts before its release.
func (v Version) Compare(o Version) int {
	for _, p := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	return strings.Compare(v.Pre, o.Pre)
}

func (v Version) String() string {
	s := "v" + strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// VersionSortLess reports a < b in the order of GNU `sort -V` for the
// shapes tags take: runs of digits compare as numbers, everything else
// byte-wise.
func VersionSortLess(a, b string) bool {
	for a != "" && b != "" {
		ca, ra := chunk(a)
		cb, rb := chunk(b)
		a, b = ra, rb
		if ca == cb {
			continue
		}
		da, db := isDigit(ca[0]), isDigit(cb[0])
		if da && db {
			x, y := strings.TrimLeft(ca, "0"), strings.TrimLeft(cb, "0")
			if len(x) != len(y) {
				return len(x) < len(y)
			}
			if x != y {
				return x < y
			}
			continue
		}
		return ca < cb
	}
	return len(a) < len(b)
}

func chunk(s string) (head, rest string) {
	d := isDigit(s[0])
	i := 1
	for i < len(s) && isDigit(s[i]) == d {
		i++
	}
	return s[:i], s[i:]
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
