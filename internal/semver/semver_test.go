package semver

import (
	"sort"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"v1.6.1", "v1.6.1", true},
		{"1.6.1", "v1.6.1", true},
		{"v1.10.0-rc1", "v1.10.0-rc1", true},
		{"v1.6", "", false},
		{"main", "", false},
		{"v1.x.0", "", false},
		{"", "", false},
	} {
		v, ok := Parse(tc.in)
		if ok != tc.ok || (ok && v.String() != tc.want) {
			t.Errorf("Parse(%q) = %v, %v; want %q, %v", tc.in, v, ok, tc.want, tc.ok)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v1.6.1", "v1.6.1", 0},
		{"v1.6.0", "v1.6.1", -1},
		{"v1.10.0", "v1.9.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.6.1-rc1", "v1.6.1", -1},
	} {
		a, _ := Parse(tc.a)
		b, _ := Parse(tc.b)
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%s vs %s = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestVersionSort(t *testing.T) {
	got := []string{"v1.10.0", "v1.2.0", "v1.9.0", "v1.2.10", "v1.2.9", "v0.9.0"}
	sort.Slice(got, func(i, j int) bool { return VersionSortLess(got[i], got[j]) })
	want := []string{"v0.9.0", "v1.2.0", "v1.2.9", "v1.2.10", "v1.9.0", "v1.10.0"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
