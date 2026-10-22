// Package runnerenv decides which of three sources names a CI endpoint: the
// RUNNER, the CALLER or nothing.
//
// The caller's value is an input, itself usually an organisation variable.
// It is the same for every runner of a tier. A runner pool that lives
// somewhere else, with other names for the same services (an ECS Fargate task
// reaching a cache through a private gateway, where an ARC pod reaches it by
// in-cluster name), would otherwise need a variable per pool. So a value the
// runner itself exports, in the process environment of the runner (a pod
// spec, a task definition), wins over the caller's:
//
//	runner environment  >  input / organisation variable  >  the action's default
//
// The runner-provided names are all prefixed CI_RUNNER_, so they never collide
// with an input or with the organisation variables they override. With none
// of them set, every function here returns the caller's value untouched.
package runnerenv

import (
	"fmt"
	"regexp"
	"strings"
)

// Names of the variables a runner may export. Each overrides one caller value.
const (
	Goproxy       = "CI_RUNNER_GOPROXY"
	NpmRegistry   = "CI_RUNNER_NPM_REGISTRY"
	BazelRemote   = "CI_RUNNER_BAZEL_REMOTE_CACHE"
	RemoteBuilder = "CI_RUNNER_REMOTE_BUILDERS"
	// MavenRegistry is the base of the Maven pull-through cache, one path per
	// upstream (<base>/central, <base>/google, <base>/gradle-plugins), and
	// GradleBuildCache the URL of Gradle's remote build cache, ending where the
	// key begins. A runner that names no build cache exports only the first;
	// each environment names its own, and a tier reads only the cache of its own
	// environment (the stable tier's jobs write and read the stable one).
	MavenRegistry    = "CI_RUNNER_MAVEN_REGISTRY"
	GradleBuildCache = "CI_RUNNER_GRADLE_BUILD_CACHE"
)

// Source says where an effective value came from.
type Source string

const (
	// FromRunner is a CI_RUNNER_* variable of the runner's own environment.
	FromRunner Source = "runner"
	// FromCaller is the input or, behind it, the caller's organisation variable.
	FromCaller Source = "caller"
	// Unset means neither named anything.
	Unset Source = "unset"
)

// Pick returns the effective value and its source. runnerVal is what the
// runner exported under runnerName; callerVal is the caller's input.
//
// A runner value is operator configuration, so a malformed one is an error and
// never a silent fall back to the caller's value: the pool would then run on
// an endpoint nobody chose. Whitespace around it is trimmed (a task
// definition easily carries a trailing newline). An entry list (the builders)
// is whitespace-separated, so allowList lets inner whitespace through; every
// other value must have none.
func Pick(runnerName, runnerVal, callerVal string, allowList bool) (string, Source, error) {
	if v := strings.TrimSpace(runnerVal); v != "" {
		bad := reControl
		if !allowList {
			bad = reSpaceOrControl
		}
		if bad.MatchString(v) {
			return "", FromRunner, fmt.Errorf("%s holds whitespace or control characters; it must be a single plain value", runnerName)
		}
		return v, FromRunner, nil
	}
	if strings.TrimSpace(callerVal) != "" {
		return callerVal, FromCaller, nil
	}
	return "", Unset, nil
}

var (
	reControl        = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	reSpaceOrControl = regexp.MustCompile(`[[:space:]\x00-\x1f\x7f]`)
	reUserinfo       = regexp.MustCompile(`(://)[^/@\s,]*@`)
)

// Redact removes the userinfo of every URL in s (scheme://user:password@host
// becomes scheme://host), so a log line never carries a credential an address
// may embed. It is for log lines; the value itself is never altered.
func Redact(s string) string { return reUserinfo.ReplaceAllString(s, "$1") }

// Describe is one log line: the name, which source won and the value, redacted.
func Describe(what string, src Source, value, runnerName string) string {
	switch src {
	case FromRunner:
		return fmt.Sprintf("%s: %s (from the runner's %s)", what, Redact(value), runnerName)
	case FromCaller:
		return fmt.Sprintf("%s: %s (from the caller's input or organisation variable)", what, Redact(value))
	}
	return fmt.Sprintf("%s: unset (neither %s nor the caller's input or organisation variable names one)", what, runnerName)
}
