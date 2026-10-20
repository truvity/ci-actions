package policyconformance

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// C18 · workflow and action YAML is runner-agnostic
//
// The same workflow has to run on an ARC runner, a GitHub-hosted one and
// whatever pool comes next, so nothing in it may know which one it is on. In
// the workflow files and the action files of the checkout it names three
// constructs that tie a job to one runner:
//
//	svc      an in-cluster service address (`<svc>.<ns>.svc`, with or
//	         without the cluster domain after it): it resolves only inside
//	         the cluster of one pool. Addresses are org variables.
//	runner   an expression that branches on `runner.environment` (or the
//	         shell's $RUNNER_ENVIRONMENT): the job then behaves differently on
//	         the two kinds of runner, and only one of them is ever tested.
//	         Capabilities are probed, not inferred from the kind of runner.
//	aws      a static AWS credential: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
//	         or AWS_SESSION_TOKEN set in env or a script, a secret of that
//	         name, or the `aws-access-key-id` family of inputs. The identity is
//	         the runner's (a pod identity, or the job's OIDC token), never a
//	         key in the repository's secrets. Setting one to the empty string
//	         (to force the default chain) is not a credential.
//
// WHERE IT READS: tracked `.github/workflows/*.yaml|yml` and every tracked
// `action.yaml|yml`, except under test, fixture, golden and vendored
// directories. Comments are not read.
//
// WARN FIRST. Current code still violates it, so by default a finding is a
// WARN: an annotation and a summary row that never fail the job, not even with
// `strict: true`. `runner-code: error` makes each finding a FAIL, which is
// what `strict` then gates. An exemption (.github/policy-conformance.yaml)
// names a reviewed exception: `reason` is required, `paths: [glob, ...]`
// narrows it.
var (
	reC18Comment = regexp.MustCompile(`(^|[ \t])#.*$`)
	reC18Svc     = regexp.MustCompile(`([A-Za-z0-9}-]\.svc(\.cluster\.local)?([^A-Za-z0-9_.-]|$))|(\.svc\.cluster\.local)`)
	reC18Runner  = regexp.MustCompile(`(^|[^A-Za-z0-9_])(runner\.environment|RUNNER_ENVIRONMENT)([^A-Za-z0-9_]|$)`)
	reC18Env     = regexp.MustCompile(`(^|[^A-Za-z0-9_])AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)[ \t]*(:[ \t]|:$|=)(.*)$`)
	reC18Secret  = regexp.MustCompile(`secrets\.AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)([^A-Za-z0-9_]|$)`)
	reC18Input   = regexp.MustCompile(`^[ \t-]*aws-(access-key-id|secret-access-key|session-token)[ \t]*:(.*)$`)
	reC18Empty   = regexp.MustCompile(`^[ \t]*(""|'')?[ \t]*$`)
)

// c18Scan reads one file's lines and appends what it finds.
func c18Scan(file, content string, out *[]finding) {
	for i, raw := range lines(content) {
		fnr := i + 1
		l := reC18Comment.ReplaceAllString(raw, "")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if reC18Svc.MatchString(l) {
			*out = append(*out, hit("svc", file, fnr, raw))
		}
		if reC18Runner.MatchString(l) {
			*out = append(*out, hit("runner", file, fnr, raw))
		}
		if m := reC18Env.FindStringSubmatch(l); m != nil {
			sep, val := m[3], m[4]
			// `AWS_ACCESS_KEY_ID: ""` clears the variable; it sets no key.
			// `==` is a comparison. A value on the next line (`:` and
			// nothing after it) is not empty.
			switch {
			case sep == "=" && strings.HasPrefix(val, "="):
			case sep != ":" && reC18Empty.MatchString(val):
			default:
				*out = append(*out, hit("aws", file, fnr, raw))
			}
		} else if reC18Secret.MatchString(l) {
			*out = append(*out, hit("aws", file, fnr, raw))
		} else if m := reC18Input.FindStringSubmatch(l); m != nil && !reC18Empty.MatchString(m[2]) {
			*out = append(*out, hit("aws", file, fnr, raw))
		}
	}
}

func c18Why(kind string) string {
	switch kind {
	case "svc":
		return "an in-cluster service address resolves on one pool only; take it from an org variable"
	case "runner":
		return "branches on the kind of runner; probe the capability instead"
	case "aws":
		return "a static AWS credential; use the runner's identity or the job's OIDC token"
	}
	return kind
}

func (r *run) c18() {
	if _, err := r.git("rev-parse", "--git-dir"); err != nil {
		r.emit("C18", "SKIP", "not a git checkout, so there is no tracked-file list to read")
		return
	}
	out, _ := r.git("ls-files", "-z")
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			continue
		}
		slash := "/" + f
		skip := false
		for _, seg := range []string{"/tests/", "/test/", "/testdata/", "/fixtures/", "/golden/", "/node_modules/", "/vendor/"} {
			if strings.Contains(slash, seg) {
				skip = true
			}
		}
		if skip {
			continue
		}
		base := path.Base(f)
		isYAML := strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml")
		workflow := isYAML && path.Dir(f) == ".github/workflows"
		action := base == "action.yaml" || base == "action.yml"
		if workflow || action {
			files = append(files, f)
		}
	}
	var found []finding
	for _, f := range files {
		if b, err := os.ReadFile(r.path(f)); err == nil {
			c18Scan(f, string(b), &found)
		}
	}

	exempt := strings.TrimSpace(r.exemptReason("C18"))
	paths := r.exemptList("C18", "paths")
	n, dropped := 0, 0
	var probs []string
	for _, f := range found {
		if exempt != "" && pathIn(f.file, paths) {
			dropped++
			continue
		}
		r.printf("  C18 %s:%d: %s: %s\n", f.file, f.line, c18Why(f.kind), f.text)
		n++
		if n <= 3 {
			probs = append(probs, fmt.Sprintf("%s:%d (%s)", f.file, f.line, f.kind))
		}
	}
	ok := fmt.Sprintf("no in-cluster address, runner-kind branch or static AWS credential in %d workflow/action file(s)", len(files))
	if dropped != 0 {
		ok += fmt.Sprintf(" (%d finding(s) exempt: %s)", dropped, exempt)
	}
	if n == 0 {
		r.emit("C18", "PASS", ok)
		return
	}
	msg := strings.Join(probs, " ")
	if n > 3 {
		msg = fmt.Sprintf("%d runner-specific construct(s): %s and %d more, listed above", n, msg, n-3)
	} else {
		msg = fmt.Sprintf("%d runner-specific construct(s): %s", n, msg)
	}
	if strings.EqualFold(r.o.RunnerCode, "warn") {
		r.emit("C18", "WARN", msg+"; a warning because runner-code is warn")
		return
	}
	r.emit("C18", "FAIL", msg)
}
