// Package taggedpins is the port of tagged-pins/tagged-pins.sh: every
// `uses: <library>/...@<sha>` anywhere in a checkout must name the COMMIT
// OF A TAG, for each shared CI library in the list.
//
// A pin to an untagged commit is a pin to whatever was on master that
// afternoon: it names no release, so it cannot be diffed against one, the
// `# v3.1.0` beside it is a wish rather than a fact, and renovate has
// nothing to bump it from. The failure is quiet.
//
// Behaviour and output lines are the shell script's, byte for byte, so the
// composite action's callers and logs do not change.
package taggedpins

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/truvity/ci-actions/internal/semver"
)

// DefaultLibraries is the shared CI library: the workflows and the actions
// they are built from.
const DefaultLibraries = "truvity/ci-workflows truvity/ci-actions"

// ErrUntagged is returned when at least one pin names no release.
var ErrUntagged = fmt.Errorf("a pinned commit is not a release")

// TagLister returns the `<commit> <tag>` pairs of a library's annotated
// tags, in remote order.
type TagLister func(library string) ([]TagRef, error)

// TagRef is one dereferenced tag.
type TagRef struct{ Commit, Name string }

// Options configures a run.
type Options struct {
	// Root is the checkout to search. Empty means ".".
	Root string
	// Libraries is whitespace- or comma-separated. Empty means the default.
	Libraries string
	// Tags resolves a library's tags. Nil means `git ls-remote --tags`.
	Tags TagLister
	// Out receives the log lines; Run never writes elsewhere.
	Out io.Writer
}

// ParseLibraries splits on whitespace and commas; an empty or blank value
// is the default, never "check nothing". An action input is always set
// and usually empty, and an empty string read as an empty list is a gate
// that passes everything while looking healthy.
func ParseLibraries(s string) []string {
	libs := strings.Fields(strings.ReplaceAll(s, ",", " "))
	if len(libs) == 0 {
		libs = strings.Fields(DefaultLibraries)
	}
	return libs
}

// Run judges the checkout. It returns ErrUntagged (after printing the
// ::error:: line) when a pin names no release.
func Run(o Options) error {
	root := o.Root
	if root == "" {
		root = "."
	}
	tags := o.Tags
	if tags == nil {
		tags = GitLsRemote
	}
	libraries := ParseLibraries(o.Libraries)

	files, err := candidateFiles(root)
	if err != nil {
		return err
	}

	fail := false
	seen := 0
	for _, library := range libraries {
		re, err := regexp.Compile(regexp.QuoteMeta(library) + `/[^@\s"']+@[0-9a-f]{40}`)
		if err != nil {
			return err
		}
		refs := findRefs(files, re)
		if len(refs) == 0 {
			fmt.Fprintf(o.Out, "no %s pins in this checkout — nothing to check\n", library)
			continue
		}
		seen++

		list, err := tags(library)
		if err != nil {
			return err
		}
		newest := newestTag(list)

		for _, ref := range refs {
			at := strings.LastIndex(ref, "@")
			sha := ref[at+1:]
			what := strings.TrimPrefix(ref[:at], library+"/")

			tag := ""
			for _, t := range list {
				if t.Commit == sha {
					tag = t.Name
					break
				}
			}
			if tag != "" {
				fmt.Fprintf(o.Out, "ok        %-46s %.12s  %s\n", library+"/"+what, sha, tag)
				continue
			}
			fail = true
			fmt.Fprintf(o.Out, "UNTAGGED  %-46s %.12s  names no release; newest is %s\n", library+"/"+what, sha, newest)
		}
	}

	if fail {
		fmt.Fprintln(o.Out, "::error::A pinned commit is not a release. Pin the commit of a tag (git rev-parse <tag>^{}), with the version in a trailing comment.")
		return ErrUntagged
	}

	// Said out loud, because "nothing to check" printed once per library
	// and then silence is indistinguishable from a guard that matched
	// nothing because the name it was given is wrong.
	fmt.Fprintf(o.Out, "tagged-pins: %d of %d libraries had pins in this checkout, all naming releases\n", seen, len(libraries))
	return nil
}

// candidateFiles lists the regular, non-binary files under root outside
// .git, which is what `grep -rI --exclude-dir=.git` reads. Searches the
// WHOLE checkout, not just .github: setup-devbox's nested ci-cache pin is
// not under .github at all.
func candidateFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: grep -s would skip it too
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func findRefs(files []string, re *regexp.Regexp) []string {
	set := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			for _, m := range re.FindAllString(sc.Text(), -1) {
				set[m] = true
			}
		}
	}
	refs := make([]string, 0, len(set))
	for r := range set {
		refs = append(refs, r)
	}
	sort.Strings(refs) // `sort -u`, byte order
	return refs
}

func newestTag(list []TagRef) string {
	names := make([]string, 0, len(list))
	for _, t := range list {
		names = append(names, t.Name)
	}
	sort.SliceStable(names, func(i, j int) bool { return semver.VersionSortLess(names[i], names[j]) })
	if len(names) == 0 {
		return ""
	}
	return names[len(names)-1]
}

var lsRemoteLine = regexp.MustCompile(`^([0-9a-f]{40})\s*refs/tags/(.*)\^\{\}$`)

// GitLsRemote reads every tag's COMMIT straight from the remote: no API,
// no token. `^{}` is the dereferenced commit of an annotated tag, which is
// what a `uses:` pin must name; the tag OBJECT sha looks just as
// plausible and resolves to nothing.
func GitLsRemote(library string) ([]TagRef, error) {
	out, err := exec.Command("git", "ls-remote", "--tags", "https://github.com/"+library).Output()
	if err != nil {
		// Under `set -o pipefail` the shell version died here too, with
		// git's own stderr: an unreachable library is not "untagged".
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("git ls-remote %s: %w: %s", library, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git ls-remote %s: %w", library, err)
	}
	return parseLsRemote(out), nil
}

func parseLsRemote(out []byte) []TagRef {
	var refs []TagRef
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if m := lsRemoteLine.FindStringSubmatch(sc.Text()); m != nil {
			refs = append(refs, TagRef{Commit: m[1], Name: m[2]})
		}
	}
	return refs
}
