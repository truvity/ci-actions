package s3cache

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

type memStore struct {
	objs   map[string][]byte
	mod    map[string]time.Time
	failOn string // "exists", "list", "get", "put"
	puts   int
	clock  time.Time
}

func newMem() *memStore {
	return &memStore{objs: map[string][]byte{}, mod: map[string]time.Time{}, clock: time.Unix(1_700_000_000, 0)}
}

func (m *memStore) fail(op string) error {
	if m.failOn == op {
		return errors.New("boom " + op)
	}
	return nil
}
func (m *memStore) Exists(_ context.Context, k string) (bool, error) {
	if err := m.fail("exists"); err != nil {
		return false, err
	}
	_, ok := m.objs[k]
	return ok, nil
}
func (m *memStore) List(_ context.Context, p string) ([]Object, error) {
	if err := m.fail("list"); err != nil {
		return nil, err
	}
	var out []Object
	for k := range m.objs {
		if strings.HasPrefix(k, p) {
			out = append(out, Object{Key: k, LastModified: m.mod[k]})
		}
	}
	return out, nil
}
func (m *memStore) Get(_ context.Context, k string) (io.ReadCloser, error) {
	if err := m.fail("get"); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(m.objs[k])), nil
}
func (m *memStore) Put(_ context.Context, k string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := m.fail("put"); err != nil {
		return err
	}
	m.puts++
	m.clock = m.clock.Add(time.Minute)
	m.objs[k] = b
	m.mod[k] = m.clock
	return nil
}

func opts(t *testing.T, st Store, root string, paths ...string) Options {
	t.Helper()
	return Options{Store: st, Repository: "o/r", Key: "k-1", Paths: paths, Root: root, Home: "/home/u", Workdir: "/w"}
}

// plant is the object key a restore of o over path would look for.
func plant(o Options, path string) string {
	o.defaults()
	return objectKey(o.base([]string{path}), o.Key)
}

func write(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestRoundTripAndImmutability(t *testing.T) {
	st := newMem()
	src := t.TempDir()
	write(t, src, "home/u/.cache/x/a.txt", "alpha")
	write(t, src, "home/u/.cache/x/sub/b.txt", "beta")
	if err := os.Symlink("a.txt", filepath.Join(src, "home/u/.cache/x/link")); err != nil {
		t.Fatal(err)
	}
	o := opts(t, st, src, "~/.cache/x")
	if r := Save(context.Background(), o); r.Bytes == 0 || st.puts != 1 {
		t.Fatalf("save: %+v puts=%d", r, st.puts)
	}
	// Immutable: a second save of the same key uploads nothing.
	if Save(context.Background(), o); st.puts != 1 {
		t.Fatalf("second save uploaded: puts=%d", st.puts)
	}
	dst := t.TempDir()
	o.Root = dst
	r := Restore(context.Background(), o)
	if !r.Hit || r.Matched != "k-1" {
		t.Fatalf("restore: %+v", r)
	}
	got, err := os.ReadFile(filepath.Join(dst, "home/u/.cache/x/sub/b.txt"))
	if err != nil || string(got) != "beta" {
		t.Fatalf("b.txt: %q %v", got, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "home/u/.cache/x/link")); err != nil || l != "a.txt" {
		t.Fatalf("link: %q %v", l, err)
	}
}

func TestRestoreKeysPickNewestUnderFirstMatchingPrefix(t *testing.T) {
	st := newMem()
	src := t.TempDir()
	write(t, src, "w/d/f", "v")
	for _, k := range []string{"gl-A-1", "gl-A-2", "gl-B-1"} {
		o := opts(t, st, src, "/w/d")
		o.Key = k
		Save(context.Background(), o)
	}
	dst := t.TempDir()
	o := opts(t, st, dst, "/w/d")
	o.Key = "gl-A-3"
	o.RestoreKey = []string{"gl-A-", "gl-"}
	r := Restore(context.Background(), o)
	if r.Hit || r.Matched != "gl-A-2" {
		t.Fatalf("want restore-key match gl-A-2, got %+v", r)
	}
	o.RestoreKey = []string{"nope-", "gl-"}
	if r := Restore(context.Background(), o); r.Matched != "gl-B-1" {
		t.Fatalf("want newest under gl-, got %+v", r)
	}
	o.RestoreKey = []string{"nope-"}
	if r := Restore(context.Background(), o); r.Matched != "" || r.Hit {
		t.Fatalf("want miss, got %+v", r)
	}
}

func TestDifferentPathsDoNotCollide(t *testing.T) {
	st := newMem()
	src := t.TempDir()
	write(t, src, "a/f", "1")
	write(t, src, "b/f", "2")
	Save(context.Background(), opts(t, st, src, "/a"))
	if r := Restore(context.Background(), opts(t, st, t.TempDir(), "/b")); r.Matched != "" {
		t.Fatalf("same key over other paths matched: %+v", r)
	}
}

func TestBucketErrorsAreMissesNotFailures(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a/f", "1")
	for _, op := range []string{"exists", "list", "get", "put"} {
		st := newMem()
		o := opts(t, st, src, "/a")
		o.Key = "k"
		o.RestoreKey = []string{"k"}
		if op == "get" {
			Save(context.Background(), o)
			o.Key = "kk"
		}
		st.failOn = op
		var errb bytes.Buffer
		o.Err = &errb
		Restore(context.Background(), o)
		Save(context.Background(), o)
		if op != "list" && !strings.Contains(errb.String(), "::warning::s3-cache") {
			t.Errorf("%s: no warning: %q", op, errb.String())
		}
	}
}

func TestMissingPathsAndBadKey(t *testing.T) {
	st := newMem()
	var errb bytes.Buffer
	o := opts(t, st, t.TempDir(), "/nowhere")
	o.Err = &errb
	if r := Save(context.Background(), o); r.Bytes != 0 || st.puts != 0 {
		t.Fatalf("saved nothing expected: %+v", r)
	}
	o.Key = "a,b"
	Save(context.Background(), o)
	if st.puts != 0 || !strings.Contains(errb.String(), "comma") {
		t.Fatalf("bad key: %q", errb.String())
	}
	o.Paths = []string{"/a/**"}
	if _, err := o.resolve(); err == nil {
		t.Fatal("glob accepted")
	}
}

func tarball(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, _ := zstd.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for n, c := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(c))
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func TestRestoreRefusesEntriesOutsideThePaths(t *testing.T) {
	st := newMem()
	dst := t.TempDir()
	o := opts(t, st, dst, "/ok")
	st.objs[plant(o, "/ok")] = tarball(t, map[string]string{"ok/f": "fine", "etc/evil": "x", "ok/../../etc/evil2": "x"})
	var errb bytes.Buffer
	o.Err = &errb
	r := Restore(context.Background(), o)
	if r.Hit {
		t.Fatalf("poisoned archive restored: %+v", r)
	}
	for _, p := range []string{"etc/evil", "etc/evil2"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Fatalf("%s written", p)
		}
	}
}

func TestRestoreRefusesWritingThroughSymlink(t *testing.T) {
	dst := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "ok", "d")); err != nil {
		t.Fatal(err)
	}
	st := newMem()
	o := opts(t, st, dst, "/ok")
	st.objs[plant(o, "/ok")] = tarball(t, map[string]string{"ok/d/pwn": "x"})
	Restore(context.Background(), o)
	if _, err := os.Stat(filepath.Join(outside, "pwn")); err == nil {
		t.Fatal("wrote through a symlink")
	}
}

func TestRestoreRefusesSymlinkOutsideThePaths(t *testing.T) {
	var b bytes.Buffer
	zw, _ := zstd.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, h := range []tar.Header{
		{Name: "ok/inside", Typeflag: tar.TypeSymlink, Linkname: "f"},
		{Name: "ok/abs", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "ok/up", Typeflag: tar.TypeSymlink, Linkname: "../../etc"},
	} {
		h.Mode = 0o777
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	zw.Close()
	dst := t.TempDir()
	st := newMem()
	o := opts(t, st, dst, "/ok")
	st.objs[plant(o, "/ok")] = b.Bytes()
	var eb bytes.Buffer
	o.Err = &eb
	if r := Restore(context.Background(), o); !r.Hit {
		t.Fatalf("archive not restored: %+v", r)
	}
	if !strings.Contains(eb.String(), "2 symlink(s)") {
		t.Fatalf("no warning about the left-out links: %q", eb.String())
	}
	if _, err := os.Lstat(filepath.Join(dst, "ok", "inside")); err != nil {
		t.Fatalf("inside link not made: %v", err)
	}
	for _, n := range []string{"abs", "up"} {
		if _, err := os.Lstat(filepath.Join(dst, "ok", n)); err == nil {
			t.Fatalf("escaping link %s created", n)
		}
	}
}
