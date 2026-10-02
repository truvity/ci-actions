package devboxparity

import "github.com/truvity/ci-actions/internal/verscmp"

func versionCompare(a, b string) int { return verscmp.Compare(a, b) }
func maxVersion(a, b string) string  { return verscmp.Max(a, b) }
func firstTwo(v string) string       { return verscmp.FirstTwo(v) }
