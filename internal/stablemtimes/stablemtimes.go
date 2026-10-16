// Package stablemtimes gives every tracked file of a checkout a
// deterministic, content-derived modification time.
//
// Why: Go's test cache keys a cached result on the SIZE and MODIFICATION
// TIME of every file the test opened (cmd/go/internal/test hashOpen hashes
// the stat, not the content), and refuses to cache when a file changed in
// the last couple of seconds. A fresh checkout stamps every file with "now",
// so in CI every test that reads repository files misses the cache even
// though the content is unchanged and the result sits in the shared cache.
// With a time derived from the blob id, unchanged content has the same
// mtime on every runner and every branch.
//
// Regular files get 2000-01-01T00:00:00Z plus (first 8 bytes of the blob id
// as a big-endian uint64) mod twenty years, in seconds. Directories that
// hold tracked files, and the root, get the fixed 2000-01-01T00:00:00Z: Go
// hashes a directory listing by its names, so a constant time loses nothing.
// Symlinks get the same content-derived time on the link itself where the
// platform can set it, and are counted as skipped where it cannot.
package stablemtimes

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Epoch is the fixed time of every directory and the base of every file time.
var Epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// spanSeconds is the width of the window file times fall into: twenty years.
const spanSeconds = 20 * 365 * 24 * 3600

// Options configures Run.
type Options struct {
	Dir string    // the working tree; empty is the current directory
	Out io.Writer // the summary line
	Err io.Writer // warnings
	// ListFiles returns `git ls-files -s -z` output for Dir; nil runs git.
	ListFiles func(ctx context.Context, dir string) ([]byte, error)
}

// Result counts what Run did.
type Result struct {
	Files, Symlinks, Dirs, Skipped, Warnings int
}

// FileTime is the modification time of a blob with the given hex object id.
func FileTime(blobHex string) (time.Time, error) {
	if len(blobHex) < 16 {
		return time.Time{}, fmt.Errorf("object id %q is too short", blobHex)
	}
	b, err := hex.DecodeString(blobHex[:16])
	if err != nil {
		return time.Time{}, fmt.Errorf("object id %q: %w", blobHex, err)
	}
	return Epoch.Add(time.Duration(binary.BigEndian.Uint64(b)%spanSeconds) * time.Second), nil
}

func gitLsFiles(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "-s", "-z")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Run applies the times and prints one summary line.
func Run(ctx context.Context, o Options) (Result, error) {
	var r Result
	dir := o.Dir
	if dir == "" {
		dir = "."
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	list := o.ListFiles
	if list == nil {
		list = gitLsFiles
	}
	raw, err := list(ctx, dir)
	if err != nil {
		return r, err
	}
	dirs := map[string]struct{}{".": {}}
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		// "<mode> <sha> <stage>\t<path>"
		meta, path, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return r, fmt.Errorf("unexpected git ls-files entry %q", entry)
		}
		mode, sha := fields[0], fields[1]
		if mode == "160000" { // gitlink: a submodule's own tree is not ours to touch
			r.Skipped++
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(path))
		fi, err := os.Lstat(full)
		if err != nil { // sparse checkout, or deleted in the tree
			r.Skipped++
			continue
		}
		t, err := FileTime(sha)
		if err != nil {
			return r, err
		}
		if mode == "120000" {
			switch err := setLinkTime(full, t); {
			case err == nil:
				r.Symlinks++
			case isUnsupported(err):
				r.Skipped++
				continue
			default:
				r.Warnings++
				fmt.Fprintf(o.Err, "stable-mtimes: warning: %s: %v\n", path, err)
				continue
			}
		} else {
			if !fi.Mode().IsRegular() {
				r.Skipped++
				continue
			}
			if err := os.Chtimes(full, t, t); err != nil {
				r.Warnings++
				fmt.Fprintf(o.Err, "stable-mtimes: warning: %s: %v\n", path, err)
				continue
			}
			r.Files++
		}
		for d := filepath.ToSlash(filepath.Dir(path)); d != "." && d != "/"; d = filepath.ToSlash(filepath.Dir(d)) {
			if _, seen := dirs[d]; seen {
				break
			}
			dirs[d] = struct{}{}
		}
	}
	for d := range dirs {
		full := filepath.Join(dir, filepath.FromSlash(d))
		if err := os.Chtimes(full, Epoch, Epoch); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			r.Warnings++
			fmt.Fprintf(o.Err, "stable-mtimes: warning: directory %s: %v\n", d, err)
			continue
		}
		r.Dirs++
	}
	fmt.Fprintf(o.Out, "stable-mtimes: %d files, %d symlinks, %d directories set; %d skipped, %d warnings\n",
		r.Files, r.Symlinks, r.Dirs, r.Skipped, r.Warnings)
	return r, nil
}
