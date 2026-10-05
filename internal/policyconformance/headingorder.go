package policyconformance

import (
	"strconv"
	"strings"

	"github.com/truvity/ci-actions/internal/verscmp"
)

// headingCompare orders two CHANGELOG version headings (vX.Y.Z[-pre]).
// It follows semver precedence, where a pre-release precedes its release
// (v1.64.0-rc.1 < v1.64.0), which verscmp's sort -V ordering gets backwards.
// A heading that is not valid semver falls back to verscmp.
func headingCompare(a, b string) int {
	pa, okA := parseSemver(a)
	pb, okB := parseSemver(b)
	if !okA || !okB {
		return verscmp.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if pa.num[i] != pb.num[i] {
			if pa.num[i] < pb.num[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa.pre == nil && pb.pre == nil:
		return 0
	case pa.pre == nil:
		return 1
	case pb.pre == nil:
		return -1
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := comparePreIdent(pa.pre[i], pb.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(pa.pre) < len(pb.pre):
		return -1
	case len(pa.pre) > len(pb.pre):
		return 1
	}
	return 0
}

type semver struct {
	num [3]uint64
	pre []string
}

func parseSemver(v string) (semver, bool) {
	var s semver
	if !strings.HasPrefix(v, "v") {
		return s, false
	}
	v = v[1:]
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
			if id == "" || (isNumeric(id) && len(id) > 1 && id[0] == '0') {
				return s, false
			}
		}
	}
	return s, true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
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
