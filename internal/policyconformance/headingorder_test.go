package policyconformance

import "testing"

func TestHeadingCompare(t *testing.T) {
	// Each list is strictly newest first.
	for _, order := range [][]string{
		{"v1.64.0", "v1.64.0-rc.2", "v1.64.0-rc.1", "v1.63.0"},
		{"v1.0.0", "v1.0.0-rc.1", "v1.0.0-beta.11", "v1.0.0-beta.2", "v1.0.0-beta", "v1.0.0-alpha.1", "v1.0.0-alpha"},
		{"v1.0.0-1", "v1.0.0-0"},
		{"v1.0.0-alpha", "v1.0.0-1"},
		{"v10.0.0", "v9.0.0"},
		{"v1.10.0", "v1.9.0"},
	} {
		for i := 0; i+1 < len(order); i++ {
			if headingCompare(order[i], order[i+1]) <= 0 || headingCompare(order[i+1], order[i]) >= 0 {
				t.Errorf("%s should be newer than %s", order[i], order[i+1])
			}
		}
	}
	if headingCompare("v1.2.3", "v1.2.3") != 0 {
		t.Error("equal versions should compare equal")
	}
}
