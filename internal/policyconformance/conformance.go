// Package policyconformance is the port of policy-conformance.sh: hold a
// repository's checkout against the component contract's checkable rules, C1
// to C13, and print one line per rule:
//
//	C5 FAIL: CHANGELOG.md has no heading for v1.2.3
//
// The contract itself lives in truvity/policy (docs/contracts/component.md);
// this is the mechanical half of it and carries no doctrine of its own. Where
// a rule needs judgement the check cannot make, it says what it looked at
// rather than pretending to certainty.
//
// It runs from the root of the repository being judged and needs git. There
// is no token: the only network call is `git fetch --tags` against the
// caller's own origin, and a failure there downgrades one sub-check, never
// the run. The output lines, annotations, summary rows and exit status are
// the shell script's, byte for byte.
package policyconformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options are the action's inputs and the runner's environment.
type Options struct {
	Dir              string // the repository root to judge; "" is the working directory
	Strict           string // "true" exits 1 when any rule fails
	Skip             string // comma- or whitespace-separated rule ids not to evaluate
	Reason           string // why; required whenever Skip is not empty
	RenovatePreset   string // the preset C7 expects `extends` to name
	DefaultBranch    string // the branch C5 finds the latest tag from
	GithubRepository string // owner/repo, for C11's repository name
	SummaryPath      string // GITHUB_STEP_SUMMARY, "" for none
	Out              io.Writer
	// Git runs git in the repository and returns its stdout; stderr is
	// dropped, as the shell version dropped it. Nil runs the real one.
	Git func(ctx context.Context, dir string, args ...string) (string, error)
}

// Rules is the rule order.
var Rules = []string{"C1", "C2", "C3", "C4", "C5", "C6", "C7", "C8", "C9", "C10", "C11", "C12", "C13"}

// README_HEADINGS: the headings C8 asks for, in this order. Other headings
// may sit between them; these must all be present and must not be reordered.
var readmeHeadings = []string{
	"Who it is for",
	"The model",
	"Install and a worked example",
	"Consumers",
	"Neighbours",
	"Documentation",
	"The rule that makes this repository public",
	"Status",
	"Development",
	"Releasing",
	"Licence",
}

// Verdict is one rule's line.
type result struct{ id, verdict, msg string }

type run struct {
	o       Options
	ctx     context.Context
	results []result
	failed  int
}

// ExitError carries the step's exit status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Run evaluates every rule not skipped. A nil error is exit status 0.
func Run(ctx context.Context, o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Strict == "" {
		o.Strict = "false"
	}
	if o.RenovatePreset == "" {
		o.RenovatePreset = "github>truvity/ci-workflows"
	}
	if o.Git == nil {
		o.Git = osGit
	}
	r := &run{o: o, ctx: ctx}

	skipped := map[string]bool{}
	for _, id := range strings.Fields(strings.ReplaceAll(o.Skip, ",", " ")) {
		id = strings.ToUpper(id)
		known := false
		for _, k := range Rules {
			if k == id {
				known = true
			}
		}
		if !known {
			r.printf("::error::skip names '%s', which is not one of %s\n", id, strings.Join(Rules, " "))
			return &ExitError{1}
		}
		skipped[id] = true
	}
	if len(skipped) > 0 && strings.TrimSpace(o.Reason) == "" {
		r.printf("::error::skip needs a reason: a rule turned off with no reason is a rule nobody remembers turning off\n")
		return &ExitError{1}
	}

	for _, id := range Rules {
		if skipped[id] {
			r.emit(id, "SKIP", "skipped by the caller: "+o.Reason)
			continue
		}
		r.rule(id)
	}

	if o.SummaryPath != "" {
		var b strings.Builder
		b.WriteString("### policy-conformance\n\n")
		fmt.Fprintf(&b, "Rules from truvity/policy `docs/contracts/component.md`; strict: `%s`.\n\n", o.Strict)
		b.WriteString("| rule | verdict | detail |\n| -- | -- | -- |\n")
		for _, res := range r.results {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", res.id, res.verdict, strings.ReplaceAll(res.msg, "|", `\|`))
		}
		if f, err := os.OpenFile(o.SummaryPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			f.WriteString(b.String())
			f.Close()
		}
	}

	if r.failed > 0 {
		if o.Strict == "true" {
			r.printf("::error::policy-conformance: %d rule(s) failed\n", r.failed)
			return &ExitError{1}
		}
		r.printf("policy-conformance: %d rule(s) failed; reported as warnings (strict: false)\n", r.failed)
		return nil
	}
	r.printf("policy-conformance: every evaluated rule passed\n")
	return nil
}

func (r *run) rule(id string) {
	switch id {
	case "C1":
		r.c1()
	case "C2":
		r.c2()
	case "C3":
		r.c3()
	case "C4":
		r.c4()
	case "C5":
		r.c5()
	case "C6":
		r.c6()
	case "C7":
		r.c7()
	case "C8":
		r.c8()
	case "C9":
		r.c9()
	case "C10":
		r.c10()
	case "C11":
		r.c11()
	case "C12":
		r.c12()
	case "C13":
		r.c13()
	}
}

func (r *run) printf(format string, a ...any) { fmt.Fprintf(r.o.Out, format, a...) }

func (r *run) emit(id, verdict, msg string) {
	r.printf("%s %s: %s\n", id, verdict, msg)
	r.results = append(r.results, result{id, verdict, msg})
	if verdict == "FAIL" {
		r.failed++
		if r.o.Strict == "true" {
			r.printf("::error title=policy-conformance %s::%s\n", id, msg)
		} else {
			r.printf("::warning title=policy-conformance %s::%s\n", id, msg)
		}
	}
}

// verdict: several problems under one rule still make one line.
func (r *run) verdict(id, okMsg string, probs ...string) {
	if len(probs) == 0 {
		r.emit(id, "PASS", okMsg)
		return
	}
	r.emit(id, "FAIL", strings.Join(probs, "; "))
}

// path resolves a repository-relative path.
func (r *run) path(p string) string { return filepath.Join(r.o.Dir, p) }

func (r *run) exists(p string) bool {
	st, err := os.Stat(r.path(p))
	return err == nil && !st.IsDir()
}

func (r *run) isDir(p string) bool {
	st, err := os.Stat(r.path(p))
	return err == nil && st.IsDir()
}

func (r *run) read(p string) string {
	b, err := os.ReadFile(r.path(p))
	if err != nil {
		return ""
	}
	return string(b)
}

// glob is a sorted bash glob (nullglob) of a pattern relative to the root.
func (r *run) glob(pattern string) []string {
	m, _ := filepath.Glob(r.path(pattern))
	out := make([]string, 0, len(m))
	for _, p := range m {
		rel, err := filepath.Rel(r.o.Dir, p)
		if err != nil {
			rel = p
		}
		if r.o.Dir == "" {
			rel = p
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func (r *run) git(args ...string) (string, error) { return r.o.Git(r.ctx, r.o.Dir, args...) }

func osGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

func (r *run) firstExisting(names ...string) string {
	for _, n := range names {
		if r.exists(n) {
			return n
		}
	}
	return ""
}

// lines splits text into lines the way a shell loop reads them: no trailing
// empty element for a final newline.
func lines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
