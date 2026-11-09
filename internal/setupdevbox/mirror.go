package setupdevbox

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/truvity/ci-actions/internal/devboxconfig"
	"github.com/truvity/ci-actions/internal/ghmirror"
	"github.com/truvity/ci-actions/internal/runcmd"
)

// flakeRef finds a string literal that BEGINS with a GitHub release-download
// URL, optionally behind devbox's `tarball+` prefix: the way devbox.json names
// a flake tarball as a package (a key or a list entry) and devbox.lock records
// it (its key, and `resolved`). A string that merely mentions such a URL, such
// as a command in an init hook, does not begin with it and is left alone.
var flakeRef = regexp.MustCompile(`"((?:tarball\+)?)(https://github\.com/[^"\s]*)"`)

// MirrorFlakes points devbox's flake-tarball downloads at the runner's GitHub
// releases mirror (CI_RUNNER_GITHUB_RELEASES). devbox hands such a package to
// nix, which downloads the tarball by itself with no way to fall back, so the
// URL is rewritten in the workspace copy of devbox.json and devbox.lock
// before the first devbox run, like StripLocalTools does for .prototools: a
// workspace copy only, hidden from git with `update-index --assume-unchanged`
// (goreleaser reads `git status`), nothing committed.
//
// Why nix accepts the rewrite: a flake reference in the lock carries
// ?narHash=..., the hash of the unpacked CONTENT. It is not a hash of the URL
// and not of the tarball file either, so a mirror that serves the same bytes
// (and so the same tree) satisfies it, and one that does not is refused by nix
// with a NAR hash mismatch. A lock entry without a narHash is still correct:
// devbox re-pins it as it would from github.com.
//
// Because nix cannot fall back, each URL is asked of the mirror first and is
// rewritten only if the mirror delivered the whole file; an error leaves that
// URL pointing at github.com. With the variable unset or empty the step does
// nothing at all.
func MirrorFlakes(ctx context.Context, e Env) error {
	e.defaults()
	base := ghmirror.Base(e.Getenv(ghmirror.EnvVar))
	if base == "" {
		return nil
	}
	jsonPath := devboxconfig.JSONPath(e.devboxConfig())
	files := []string{jsonPath, filepath.Join(filepath.Dir(jsonPath), "devbox.lock")}

	var found []mirrorFile
	var refs []string // distinct URLs, without query or fragment, in order
	seen := map[string]bool{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		found = append(found, mirrorFile{p, st.Mode().Perm(), string(b)})
		for _, m := range flakeRef.FindAllStringSubmatch(string(b), -1) {
			if ghmirror.Rewrite(base, m[2]) == m[2] {
				continue // not a release download
			}
			u := bare(m[2])
			if !seen[u] {
				seen[u] = true
				refs = append(refs, u)
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}

	usable := map[string]bool{}
	for _, u := range refs {
		if err := ghmirror.Serves(ctx, e.HTTP, ghmirror.Rewrite(base, u), ghmirror.MirrorTimeout, 256<<20); err != nil {
			e.printf("::warning::the GitHub releases mirror did not serve %s (%v); devbox fetches it from github.com\n", u, err)
			continue
		}
		usable[u] = true
	}
	if len(usable) == 0 {
		return nil
	}

	var changed []mirrorFile
	for _, f := range found {
		out := flakeRef.ReplaceAllStringFunc(f.orig, func(s string) string {
			m := flakeRef.FindStringSubmatch(s)
			if !usable[bare(m[2])] {
				return s
			}
			return `"` + m[1] + ghmirror.Rewrite(base, m[2]) + `"`
		})
		if out == f.orig {
			continue
		}
		if err := os.WriteFile(f.path, []byte(out), f.mode); err != nil {
			restore(changed)
			return err
		}
		changed = append(changed, f)
	}
	// Tracked files only: git refuses to mark an untracked one, and an
	// untracked file cannot make a release build dirty. If the index cannot be
	// told, put the originals back: a dirty tree is worse than a slow download.
	for _, f := range changed {
		if e.Exec(ctx, runcmd.Cmd{Name: "git", Args: []string{"ls-files", "--error-unmatch", "--", f.path}}) != nil {
			continue
		}
		if err := e.Exec(ctx, runcmd.Cmd{Name: "git", Args: []string{"update-index", "--assume-unchanged", "--", f.path}, Stdout: e.Out, Stderr: e.Err}); err != nil {
			e.printf("::warning::git update-index %s: %v; leaving devbox's flake downloads on github.com\n", f.path, err)
			restore(changed)
			return nil
		}
	}
	e.printf("devbox flake tarballs from GitHub releases now come through the runner's mirror (%d URL(s) in %d file(s))\n", len(usable), len(changed))
	return nil
}

// mirrorFile is a workspace file as it was before the rewrite.
type mirrorFile struct {
	path string
	mode os.FileMode
	orig string
}

// restore puts the files back as they were.
func restore(fs []mirrorFile) {
	for _, f := range fs {
		_ = os.WriteFile(f.path, []byte(f.orig), f.mode)
	}
}

// bare is a URL without its query string and fragment.
func bare(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}
