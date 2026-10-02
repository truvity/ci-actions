// Package callerparity is the port of caller-parity/caller-parity.sh: do the
// estate's shared caller workflows still say the same thing?
//
// Two of the caller workflows every repository carries are the same file
// everywhere: `security.yaml` and `auto-release.yaml` differ only in their
// prose and in one staggered `cron:` minute. Nothing asserted it, so one
// repository lost its `push:` trigger and nobody noticed until it was read
// by eye. This compares each enrolled repository against the canonical
// copies in `kits/`, and REPORTS: no branch, no pull request, no rewrite.
//
// The normalisation decides whether a difference is real, and a wrong
// answer is quiet either way: a comparison that is too strict is noise
// everyone learns to skip, one that is too loose reports parity that is not
// there. The shapes it has to survive are pinned by the tests. The log,
// summary and output lines are the shell script's, byte for byte.
package callerparity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/truvity/ci-actions/internal/ghapi"
)

// Options are the action's inputs and the runner's files.
type Options struct {
	Token        string
	Repositories string // JSON array of owner/name
	Kits         string // directory of canonical copies
	FailOnDiff   string // "true" fails the job when a repository differs
	API          string

	Out     io.Writer
	Summary string
	Output  string

	Client *ghapi.Client
	// Diff produces the unified diff of two texts; nil runs `diff -u`.
	Diff func(oldLabel, newLabel, oldContent, newContent string) ([]byte, error)
}

// ErrReported marks a failure whose ::error:: line is already written.
var ErrReported = errors.New("caller-parity: reported")

type row struct{ repo, file, state string }

type run struct {
	o        Options
	c        *ghapi.Client
	man      *manifest
	branch   string
	applies  map[string]string
	workDir  string
	diffs    map[string]string // diff file name -> content, in name order at print time
	nonempty bool
}

// Run compares every repository against every kit.
func Run(ctx context.Context, o Options) error {
	work, err := os.MkdirTemp("", "caller-parity-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if o.Diff == nil {
		o.Diff = osDiff
	}
	c := o.Client
	if c == nil {
		c = ghapi.New(o.API, o.Token)
	}
	r := &run{o: o, c: c, applies: map[string]string{}, workDir: work, diffs: map[string]string{}}

	// WHERE A KIT LIVES, AND HOW MUCH OF IT IS SHARED: kits/kits.yaml says
	// so, per kit, and says nothing about the ones that are the default
	// shape.
	r.man, err = loadManifest(filepath.Join(o.Kits, "kits.yaml"))
	if err != nil {
		fmt.Fprintf(o.Out, "::error::%s: %v\n", filepath.Join(o.Kits, "kits.yaml"), err)
		return ErrReported
	}

	entries, err := filepath.Glob(filepath.Join(o.Kits, "*.yaml"))
	if err != nil {
		return err
	}
	sort.Strings(entries)
	var kits, skipped []string
	for _, k := range entries {
		name := filepath.Base(k)
		if name == "kits.yaml" {
			continue
		}
		if r.man.setting(name, "enabled", "true") != "true" {
			skipped = append(skipped, name)
			continue
		}
		kits = append(kits, k)
	}
	if len(kits) == 0 {
		fmt.Fprintf(o.Out, "::error::no kit files in %s\n", o.Kits)
		return ErrReported
	}

	var repos []string
	if err := json.Unmarshal([]byte(orDefault(o.Repositories, "[]")), &repos); err != nil {
		repos = nil
	}
	if len(repos) == 0 {
		if err := r.emit("no repositories to check\n"); err != nil {
			return err
		}
		return r.output("differences=0\nabsent=0\n")
	}

	// Six states, and the difference between them is the point of the
	// check: same, differs, absent (a repository that carries none of the
	// file), unreadable (a read that failed, never reported as any of the
	// others), n/a (a repository this kit's `applies_if` says to skip) and
	// exempt (<reason>) (named in the kit's `exempt:` map, not compared).
	var rows []row
	differences, absent, unreadable, napplicable, nexempt := 0, 0, 0, 0, 0

	for _, repo := range repos {
		status, body, err := c.Raw(ctx, "GET", c.BaseURL+"/repos/"+repo, nil)
		if err != nil {
			status = 0
		}
		if status != 200 {
			fmt.Fprintf(o.Out, "::warning::%s: could not read the repository (HTTP %03d) — not compared\n", repo, status)
			for _, k := range kits {
				rows = append(rows, row{repo, filepath.Base(k), "unreadable"})
				unreadable++
			}
			continue
		}
		var meta struct {
			DefaultBranch string `json:"default_branch"`
		}
		_ = json.Unmarshal(body, &meta)
		r.branch = meta.DefaultBranch

		for _, kit := range kits {
			name := filepath.Base(kit)
			state := "unreadable"
			skip := false

			// DOCUMENTED EXEMPTIONS. `exempt:` names a repository and says
			// why, in prose that ends up in the table. Checked first, and it
			// costs no read of its own.
			if reason := r.man.exemptReason(name, repo[strings.LastIndex(repo, "/")+1:]); reason != "" {
				state = "exempt (" + reason + ")"
				skip = true
				nexempt++
			}

			// ONLY CHECK REPOSITORIES THIS KIT APPLIES TO. `applies_if`
			// names a file at the repository's ROOT: for the lint kit, the
			// go.mod that makes a repository a Go repository at all. Kept to
			// the root deliberately: a multi-module repository with no root
			// go.mod would need a tree search to tell "no Go here" from "Go,
			// just not at the root", and this check has no reason to guess.
			if !skip {
				if appliesIf := r.man.setting(name, "applies_if", ""); appliesIf != "" {
					switch r.appliesState(ctx, repo, appliesIf) {
					case "no":
						state, skip = "n/a", true
						napplicable++
					case "unreadable":
						fmt.Fprintf(o.Out, "::warning::%s: could not read %s — not compared\n", repo, appliesIf)
						state, skip = "unreadable", true
						unreadable++
					}
				}
			}

			if !skip {
				state = r.compareKit(ctx, repo, kit, &differences, &absent, &unreadable)
			}
			rows = append(rows, row{repo, name, state})
		}
	}

	// Everything absent everywhere is not an estate that carries no
	// callers; it is a token that cannot read file contents. Say so rather
	// than reporting a clean-looking sweep of nothing.
	if absent == len(rows) {
		fmt.Fprintf(o.Out, "::warning::every file is reported absent — check that the token carries contents: read\n")
	}

	var sum bytes.Buffer
	sum.WriteString("## caller-parity — the shared caller files\n\n")
	sum.WriteString("Compared on SUBSTANCE: comment lines, blank lines, the `cron:` line and\n")
	sum.WriteString("this library's pinned ref are not compared. The `cron:` minute is\n")
	sum.WriteString("staggered per repository on purpose.\n\n")
	fmt.Fprintf(&sum, "**%d** differ, **%d** absent, **%d** n/a, **%d** exempt,\n", differences, absent, napplicable, nexempt)
	fmt.Fprintf(&sum, "**%d** could not be read, across %d repositories\n", unreadable, len(repos))
	fmt.Fprintf(&sum, "and %d files.\n\n", len(kits))
	if len(skipped) > 0 {
		fmt.Fprintf(&sum, "Kits turned off in `kits/kits.yaml` and NOT compared: %s.\n\n", strings.Join(skipped, " "))
	}
	sum.WriteString("| repository |")
	for _, k := range kits {
		fmt.Fprintf(&sum, " %s |", filepath.Base(k))
	}
	sum.WriteString("\n|---|")
	for range kits {
		sum.WriteString("---|")
	}
	sum.WriteString("\n")
	for _, repo := range repos {
		fmt.Fprintf(&sum, "| %s |", repo)
		for _, k := range kits {
			cell := "-"
			for _, rw := range rows {
				if rw.repo == repo && rw.file == filepath.Base(k) {
					cell = rw.state
					break
				}
			}
			fmt.Fprintf(&sum, " %s |", cell)
		}
		sum.WriteString("\n")
	}
	sum.WriteString("\n")
	if differences > 0 {
		sum.WriteString("### What differs\n\n")
		sum.WriteString("Normalised diffs. NOTHING IS REWRITTEN: bring the repository to the\n")
		sum.WriteString("canonical copy by hand, or change the canonical copy for the estate.\n\n")
		names := make([]string, 0, len(r.diffs))
		for n := range r.diffs {
			names = append(names, n)
		}
		sort.Strings(names) // the shell's glob, in the C locale
		for _, n := range names {
			sum.WriteString("```diff\n")
			sum.WriteString(r.diffs[n])
			sum.WriteString("```\n\n")
		}
	}
	if err := r.emit(sum.String()); err != nil {
		return err
	}
	if err := r.output(fmt.Sprintf("differences=%d\nabsent=%d\n", differences, absent)); err != nil {
		return err
	}
	if o.FailOnDiff == "true" && differences > 0 {
		fmt.Fprintf(o.Out, "::error::%d caller file(s) differ from the canonical kit\n", differences)
		return ErrReported
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// emit is `| tee -a $GITHUB_STEP_SUMMARY`.
func (r *run) emit(s string) error {
	if _, err := io.WriteString(r.o.Out, s); err != nil {
		return err
	}
	return appendFile(r.o.Summary, []byte(s))
}

func (r *run) output(s string) error { return appendFile(r.o.Output, []byte(s)) }

func appendFile(path string, data []byte) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *run) contentsURL(repo, p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", r.c.BaseURL, repo, strings.Join(segs, "/"), url.QueryEscape(r.branch))
}

// appliesState says whether a repository carries the file `applies_if`
// names, cached per repository so the file is read at most once however
// many kits ask. The key does not include the branch because a repository
// has exactly one default branch for the lifetime of the loop.
func (r *run) appliesState(ctx context.Context, repo, file string) string {
	key := repo + "|" + file
	if s, ok := r.applies[key]; ok {
		return s
	}
	status, _, err := r.c.Raw(ctx, "GET", r.contentsURL(repo, file), nil)
	s := "unreadable"
	if err == nil {
		switch status {
		case 200:
			s = "yes"
		case 404:
			s = "no"
		}
	}
	r.applies[key] = s
	return s
}

// compareKit reads the repository's copy of one kit and compares it.
func (r *run) compareKit(ctx context.Context, repo string, kit string, differences, absent, unreadable *int) string {
	name := filepath.Base(kit)
	compare := r.man.setting(name, "compare", "")
	within := r.man.setting(name, "within", ".")

	// `path:` may be one scalar or a list. Tried in order; the first 200
	// wins. A 404 on every candidate is absent; a non-404 failure on any of
	// them, with no 200 among them, is unreadable, reported for whichever
	// candidate hit it first.
	var found []byte
	foundPath, errStatus, errPath := "", -1, ""
	hit := false
	for _, cand := range r.man.paths(name, ".github/workflows/"+name) {
		status, body, err := r.c.Raw(ctx, "GET", r.contentsURL(repo, cand), nil)
		if err != nil {
			status = 0
		}
		if status == 200 {
			hit, found, foundPath = true, body, cand
			break
		}
		if status != 404 && errStatus < 0 {
			errStatus, errPath = status, cand
		}
	}
	switch {
	case hit:
		theirs := decodeContent(found)
		ourBytes, err := os.ReadFile(kit)
		if err != nil {
			ourBytes = nil
		}
		var ours, theirsNorm []byte
		if compare != "" {
			// A BLOCK inside a larger file, compared as data rather than as
			// text: canonical JSON, keys sorted. A subtree that is not there
			// reads as `null`, which differs from the kit: a repository that
			// dropped the block has fallen behind it, and it is not "absent",
			// because the file it belongs in is right there.
			theirsNorm = subtree(theirs, compare)
			ours = subtree(ourBytes, within)
		} else {
			theirsNorm = substance(theirs)
			ours = substance(ourBytes)
		}
		if bytes.Equal(ours, theirsNorm) {
			return "same"
		}
		*differences++
		d, _ := r.o.Diff("kits/"+name+" (canonical)", repo+" "+foundPath, string(ours), string(theirsNorm))
		r.diffs[strings.ReplaceAll(repo, "/", "__")+"."+name+".diff"] = string(d)
		fmt.Fprintf(r.o.Out, "::warning::%s: %s differs from the canonical kit\n", repo, foundPath)
		return "differs"
	case errStatus >= 0:
		fmt.Fprintf(r.o.Out, "::warning::%s: could not read %s (HTTP %03d) — not compared\n", repo, errPath, errStatus)
		*unreadable++
		return "unreadable"
	}
	*absent++
	return "absent"
}

// decodeContent is `jq -r '.content // empty' | tr -d '\n' | base64 -d`,
// with an empty file for anything that does not decode.
func decodeContent(body []byte) []byte {
	var m struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(m.Content, "\n", ""))
	if err != nil {
		return nil
	}
	return b
}

// osDiff is `diff -u --label old --label new`, run on two temporary files
// that hold the given contents: the hunks are GNU diff's own.
func osDiff(oldLabel, newLabel, oldContent, newContent string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "caller-parity-diff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	a, b := filepath.Join(dir, "ours"), filepath.Join(dir, "theirs")
	if err := os.WriteFile(a, []byte(oldContent), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(b, []byte(newContent), 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command("diff", "-u", "--label", oldLabel, "--label", newLabel, a, b)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		err = nil // 1 means "they differ", which is the point
	}
	return out, err
}
