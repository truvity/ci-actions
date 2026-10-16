// Package s3cache is actions/cache for a self-hosted runner that has a bucket
// and no reason to send cache traffic through the NAT to GitHub's cache.
//
// Same contract as actions/cache: an exact key wins, then each restore key as
// a PREFIX (the newest object under it), a saved key is immutable, and a
// missing cache is a miss, never a failure. The payload is a tar stream
// compressed with zstd, held in the bucket under
//
//	<prefix>/<repository>/<version>/<key>.tar.zst
//
// where <version> is a short hash of the cached paths (as actions/cache does,
// so the same key over different paths never collides). The bucket's own
// lifecycle rule ages objects out; nothing here deletes.
//
// Fail-open: every bucket error is a warning and a miss, because a slower job
// beats a failed one. When the credentials cannot even be obtained the result
// says `fallback=true`, so the composite can hand the same key to GitHub's
// cache instead.
package s3cache

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Object is one entry of a listing.
type Object struct {
	Key          string
	LastModified time.Time
}

// Store is the bucket as this package needs it. Tests substitute a fake.
type Store interface {
	// Exists reports whether key is present.
	Exists(ctx context.Context, key string) (bool, error)
	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Get streams one object; the caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Put uploads r (of unknown length) as key.
	Put(ctx context.Context, key string, r io.Reader) error
}

// Options configures Restore and Save.
type Options struct {
	Store      Store
	Prefix     string // key prefix inside the bucket, default "gha-cache"
	Repository string // owner/name
	Key        string
	Paths      []string // files or directories; ~ is expanded; no globs
	RestoreKey []string // prefixes, tried in order
	Home       string   // for ~; default the user's home
	Workdir    string   // relative paths resolve here; default the current directory
	Root       string   // extraction/archive root; default "/" (tests set a temp dir)
	Out, Err   io.Writer
	Now        func() time.Time
}

// Result is what a step reports through GITHUB_OUTPUT.
type Result struct {
	Hit        bool   // an EXACT match
	Matched    string // key of the restored cache, empty on a miss
	PrimaryKey string
	Fallback   bool // the store was unusable; the caller should try another cache
	Bytes      int64
}

// ErrNoStore is returned by a Store constructor that cannot get credentials.
var ErrNoStore = errors.New("s3cache: the bucket is unusable")

func (o *Options) defaults() {
	if o.Prefix == "" {
		o.Prefix = "gha-cache"
	}
	if o.Root == "" {
		o.Root = "/"
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.Workdir == "" {
		o.Workdir, _ = os.Getwd()
	}
}

// resolve turns the configured paths into clean absolute paths.
func (o *Options) resolve() ([]string, error) {
	var out []string
	for _, p := range o.Paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch {
		case p == "~":
			p = o.Home
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(o.Home, p[2:])
		case !filepath.IsAbs(p):
			p = filepath.Join(o.Workdir, p)
		}
		if strings.ContainsAny(p, "*?[") {
			return nil, fmt.Errorf("path %q: globs are not supported, name directories", p)
		}
		out = append(out, filepath.Clean(p))
	}
	if len(out) == 0 {
		return nil, errors.New("no paths to cache")
	}
	return out, nil
}

func (o *Options) validKey(k string) error {
	if k == "" {
		return errors.New("key is empty")
	}
	if strings.ContainsAny(k, ",\n\r") {
		return fmt.Errorf("key %q contains a comma or a newline", k)
	}
	return nil
}

// base is the directory in the bucket for these paths.
func (o *Options) base(paths []string) string {
	h := sha256.Sum256([]byte(strings.Join(paths, "\x00") + "\x00zstd-tar-v1"))
	return path.Join(o.Prefix, o.Repository, hex.EncodeToString(h[:])[:8])
}

func objectKey(base, key string) string { return base + "/" + key + ".tar.zst" }

// Restore looks up the cache and unpacks it. It never returns an error for a
// bucket problem: those become a miss with a warning.
func Restore(ctx context.Context, o Options) Result {
	o.defaults()
	res := Result{PrimaryKey: o.Key}
	if err := o.validKey(o.Key); err != nil {
		warn(o, "restore skipped: %v", err)
		return res
	}
	paths, err := o.resolve()
	if err != nil {
		warn(o, "restore skipped: %v", err)
		return res
	}
	start := o.Now()
	base := o.base(paths)

	found, exact, err := lookup(ctx, o, base)
	if err != nil {
		warn(o, "restore: lookup failed, treating it as a miss: %v", err)
		return res
	}
	if found == "" {
		fmt.Fprintf(o.Out, "s3-cache: miss for key %s (no exact or restore-key match)\n", o.Key)
		return res
	}
	n, err := download(ctx, o, found, paths)
	if err != nil {
		warn(o, "restore: %v; treating it as a miss", err)
		return res
	}
	res.Hit = exact
	res.Matched = strings.TrimSuffix(strings.TrimPrefix(found, base+"/"), ".tar.zst")
	res.Bytes = n
	kind := "restore-key"
	if exact {
		kind = "exact"
	}
	fmt.Fprintf(o.Out, "s3-cache: restored %s (%s match) %.1f MB unpacked from s3 in %.1fs\n",
		res.Matched, kind, float64(n)/1e6, o.Now().Sub(start).Seconds())
	return res
}

// lookup returns the object to restore: the exact key, else the newest object
// under the first restore key that has any.
func lookup(ctx context.Context, o Options, base string) (key string, exact bool, err error) {
	ek := objectKey(base, o.Key)
	ok, err := o.Store.Exists(ctx, ek)
	if err != nil {
		return "", false, err
	}
	if ok {
		return ek, true, nil
	}
	for _, rk := range o.RestoreKey {
		rk = strings.TrimSpace(rk)
		if rk == "" {
			continue
		}
		objs, err := o.Store.List(ctx, base+"/"+rk)
		if err != nil {
			return "", false, err
		}
		var cands []Object
		for _, ob := range objs {
			if strings.HasSuffix(ob.Key, ".tar.zst") {
				cands = append(cands, ob)
			}
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(i, j int) bool {
			if !cands[i].LastModified.Equal(cands[j].LastModified) {
				return cands[i].LastModified.After(cands[j].LastModified)
			}
			return cands[i].Key > cands[j].Key
		})
		return cands[0].Key, false, nil
	}
	return "", false, nil
}

func download(ctx context.Context, o Options, key string, paths []string) (int64, error) {
	rc, err := o.Store.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("get: %w", err)
	}
	defer rc.Close()
	zr, err := zstd.NewReader(rc)
	if err != nil {
		return 0, fmt.Errorf("zstd: %w", err)
	}
	defer zr.Close()
	n, skipped, err := untar(tar.NewReader(zr), o.Root, paths)
	if skipped > 0 {
		warn(o, "restore: %d symlink(s) in the archive were left out", skipped)
	}
	return n, err
}

// untar extracts only entries under one of the configured paths, so an object
// someone else wrote cannot place a file anywhere else, and refuses to write
// through a symlink. Symlinks in an archive are never created, only counted.
func untar(tr *tar.Reader, root string, paths []string) (total int64, skipped int, err error) {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return total, skipped, nil
		}
		if err != nil {
			return total, skipped, fmt.Errorf("tar: %w", err)
		}
		if !filepath.IsLocal(filepath.FromSlash(h.Name)) {
			return total, skipped, fmt.Errorf("entry %q is not a relative path inside the root", h.Name)
		}
		name := path.Clean("/" + h.Name)
		if !under(name, paths) {
			return total, skipped, fmt.Errorf("entry %q is outside the cached paths", h.Name)
		}
		dst := filepath.Join(root, filepath.FromSlash(name))
		if !strings.HasPrefix(dst, filepath.Clean(root)+string(filepath.Separator)) && dst != filepath.Clean(root) {
			return total, skipped, fmt.Errorf("entry %q escapes the extraction root", h.Name)
		}
		if err := safeParent(root, dst); err != nil {
			return total, skipped, err
		}
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return total, skipped, err
			}
			if err := os.Chmod(dst, mode|0o700); err != nil {
				return total, skipped, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return total, skipped, err
			}
			_ = os.Remove(dst)
			f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o200)
			if err != nil {
				return total, skipped, err
			}
			n, err := io.Copy(f, tr)
			total += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return total, skipped, err
			}
			if err := os.Chmod(dst, mode); err != nil {
				return total, skipped, err
			}
			_ = os.Chtimes(dst, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			// Never created: an archive link is the one way a cache object
			// writes outside the paths it claims. Tools rebuild a link far
			// more cheaply than a cache can vouch for one.
			skipped++
		default:
			// Hard links, devices, fifos: nothing a tool cache needs.
		}
	}
}

func under(name string, paths []string) bool {
	for _, p := range paths {
		p = filepath.ToSlash(p)
		if name == p || strings.HasPrefix(name, strings.TrimRight(p, "/")+"/") {
			return true
		}
	}
	return false
}

// safeParent refuses a destination whose parent chain, below root, passes
// through a symlink: that is how an archive writes outside the paths it
// claims.
func safeParent(root, dst string) error {
	rel, err := filepath.Rel(root, filepath.Dir(dst))
	if err != nil {
		return err
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink %s", cur)
		}
	}
	return nil
}

// Save packs the paths and uploads them under the key, unless the key is
// already there (a saved key is immutable).
func Save(ctx context.Context, o Options) Result {
	o.defaults()
	res := Result{PrimaryKey: o.Key}
	if err := o.validKey(o.Key); err != nil {
		warn(o, "save skipped: %v", err)
		return res
	}
	paths, err := o.resolve()
	if err != nil {
		warn(o, "save skipped: %v", err)
		return res
	}
	start := o.Now()
	base := o.base(paths)
	ek := objectKey(base, o.Key)

	exists, err := o.Store.Exists(ctx, ek)
	if err != nil {
		warn(o, "save: lookup failed, not saving: %v", err)
		return res
	}
	if exists {
		fmt.Fprintf(o.Out, "s3-cache: key %s already exists, not saving\n", o.Key)
		res.Hit = true
		return res
	}
	var present []string
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(o.Root, p)); err != nil {
			warn(o, "save: path %s does not exist, leaving it out", p)
			continue
		}
		present = append(present, p)
	}
	if len(present) == 0 {
		warn(o, "save skipped: none of the paths exist")
		return res
	}

	pr, pw := io.Pipe()
	counter := &countWriter{}
	go func() {
		zw, err := zstd.NewWriter(io.MultiWriter(pw, counter))
		if err == nil {
			err = tarPaths(tar.NewWriter(zw), o.Root, present)
			if cerr := zw.Close(); err == nil {
				err = cerr
			}
		}
		pw.CloseWithError(err)
	}()
	err = o.Store.Put(ctx, ek, pr)
	_ = pr.CloseWithError(io.ErrClosedPipe)
	if err != nil {
		warn(o, "save: upload failed, not saved: %v", err)
		return res
	}
	res.Bytes = counter.n
	fmt.Fprintf(o.Out, "s3-cache: saved %s, %.1f MB compressed to s3 in %.1fs\n",
		o.Key, float64(counter.n)/1e6, o.Now().Sub(start).Seconds())
	return res
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func tarPaths(tw *tar.Writer, root string, paths []string) error {
	for _, p := range paths {
		top := filepath.Join(root, p)
		err := filepath.WalkDir(top, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				// A file that vanishes mid-walk (a tool still writing) is
				// not a reason to lose the whole cache.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			rel, err := filepath.Rel(root, fp)
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			// Regular files and directories only; links are not restored.
			if !fi.Mode().IsRegular() && !fi.IsDir() {
				return nil
			}
			h, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			h.Name = filepath.ToSlash(rel)
			if fi.IsDir() {
				h.Name += "/"
			}
			h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
				f, err := os.Open(fp)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				defer f.Close()
				// Size was fixed in the header; a file that grew since the
				// stat must not overrun it.
				n, err := io.CopyN(tw, f, h.Size)
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				// One that shrank: pad, the archive stays well-formed.
				if n < h.Size {
					if _, err := io.CopyN(tw, zeros{}, h.Size-n); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func warn(o Options, format string, a ...any) {
	fmt.Fprintf(o.Err, "::warning::s3-cache: "+format+"\n", a...)
}
