package policyconformance

import "github.com/truvity/ci-actions/internal/verscmp"

// headingCompare orders two CHANGELOG version headings (vX.Y.Z[-pre]) by
// semver precedence, where a pre-release precedes its release
// (v1.64.0-rc.1 < v1.64.0). A heading that is not valid semver falls back to
// verscmp's natural ordering.
func headingCompare(a, b string) int { return verscmp.Compare(a, b) }
