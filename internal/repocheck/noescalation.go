package repocheck

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The no-privilege contract, enforced: no action file and no script in this
// repository may call the privilege-escalation command.
//
// These actions run on runners under the Pod Security `restricted` profile
// (uid 1001, no_new_privs, all capabilities dropped), where it can never work.
// GitHub-hosted runners do have it passwordless, and we no longer use it: an
// action that needs it works on one and breaks on the other, which is exactly
// how setup-devbox broke every pinned caller.
//
// Everything is scanned but prose (*.md), because a changelog may SAY the word,
// and hack/restricted-sim/, which installs the command on purpose as the
// control that proves the simulation fails because of no_new_privs and nothing
// else. The word is matched as a whole word, in comments too: a commented-out
// call is one edit from a live one. Binary files are not read.
//
// The word is spelled in two pieces so this file does not trip itself.
var escalationWord = "su" + "do"

var reEscalation = regexp.MustCompile(`\b` + escalationWord + `\b`)

// NoEscalation scans root and writes every hit as `path:line:text`, in path
// order, returning ErrFailed when there is one.
func NoEscalation(root string, out io.Writer) error {
	hits, err := scanEscalation(root)
	if err != nil {
		return err
	}
	for _, h := range hits {
		fmt.Fprintln(out, h)
	}
	if len(hits) > 0 {
		fmt.Fprintf(out, "::error::the no-privilege contract is broken: no %s in action files or scripts\n", escalationWord)
		return ErrFailed
	}
	fmt.Fprintf(out, "ok    this repository is free of %s\n", escalationWord)
	return nil
}

func scanEscalation(root string) ([]string, error) {
	var hits []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "restricted-sim", "node_modules":
				if p != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		head := b
		if len(head) > 32768 {
			head = head[:32768]
		}
		if bytes.IndexByte(head, 0) >= 0 {
			return nil // grep -I: a binary file
		}
		for i, line := range strings.Split(string(b), "\n") {
			if reEscalation.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", p, i+1, line))
			}
		}
		return nil
	})
	return hits, err
}
