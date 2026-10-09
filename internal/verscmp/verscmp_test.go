package verscmp

import "testing"

func TestCompare(t *testing.T) {
	// Each row lists versions in strictly ascending order.
	for _, order := range [][]string{
		{"v1.73.0", "v1.74.0-rc.2", "v1.74.0-rc.4", "v1.74.0"},
		{"v1.0.0-rc.9", "v1.0.0-rc.10"},
		{"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0"},
		{"v1.0.0-1", "v1.0.0-alpha"},
		{"1.2.3-rc.1", "1.2.3", "1.10.0"},
		{"v2.0.0", "v10.0.0"},
		// Not semver: the natural ordering stays.
		{"1.25", "1.25.4", "1.26", "1.26rc1"},
		{"1.9", "1.10"},
		{"1.25.4", "1.26rc1"},
	} {
		for i := 0; i+1 < len(order); i++ {
			if Compare(order[i], order[i+1]) >= 0 || Compare(order[i+1], order[i]) <= 0 {
				t.Errorf("%s should rank below %s", order[i], order[i+1])
			}
		}
	}
	for _, same := range [][2]string{
		{"v1.2.3", "v1.2.3"},
		{"v1.2.3+a", "v1.2.3+b"},
		{"v1.2.3-rc.1+x", "1.2.3-rc.1"},
		{"1.25", "1.25"},
	} {
		if Compare(same[0], same[1]) != 0 {
			t.Errorf("%s and %s should compare equal", same[0], same[1])
		}
	}
}

func TestMaxAndFirstTwo(t *testing.T) {
	if got := Max("v1.74.0-rc.4", "v1.74.0"); got != "v1.74.0" {
		t.Errorf("Max = %s", got)
	}
	if got := Max("v1.74.0", "v1.74.0-rc.4"); got != "v1.74.0" {
		t.Errorf("Max = %s", got)
	}
	if got := FirstTwo("1.25.4"); got != "1.25" {
		t.Errorf("FirstTwo = %s", got)
	}
}
