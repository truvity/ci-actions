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
	if _, err := os.Lstat(filepath.Join(dst, "home/u/.cache/x/link")); err == nil {
		t.Fatal("symlink was saved and restored")
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

func TestRestoreNeverCreatesSymlinks(t *testing.T) {
	var b bytes.Buffer
	zw, _ := zstd.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, h := range []tar.Header{
		{Name: "ok/inside", Typeflag: tar.TypeSymlink, Linkname: "f"},
		{Name: "ok/abs", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
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
	for _, n := range []string{"inside", "abs"} {
		if _, err := os.Lstat(filepath.Join(dst, "ok", n)); err == nil {
			t.Fatalf("link %s created", n)
		}
	}
}

func TestWithinHandlesTheFilesystemRoot(t *testing.T) {
	for _, c := range []struct {
		root, dst string
		want      bool
	}{
		{"/", "/home/u/.cache/x", true},
		{"/", "/", true},
		{"/tmp/r", "/tmp/r/a/b", true},
		{"/tmp/r", "/tmp/r", true},
		{"/tmp/r", "/tmp/rx/a", false},
		{"/tmp/r", "/etc/passwd", false},
	} {
		if got := within(c.root, c.dst); got != c.want {
			t.Errorf("within(%q, %q) = %v, want %v", c.root, c.dst, got, c.want)
		}
	}
}

func TestTaskIdentityOnlyDropsEveryJobCredentialAndKeepsTheRunners(t *testing.T) {
	env := map[string]string{
		"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s", "AWS_SESSION_TOKEN": "t", "AWS_PROFILE": "p",
		"AWS_DEFAULT_PROFILE": "p", "AWS_ROLE_ARN": "r", "AWS_WEB_IDENTITY_TOKEN_FILE": "/tok",
		"AWS_CONFIG_FILE": "/home/x/config", "AWS_SHARED_CREDENTIALS_FILE": "/home/x/credentials",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x", "AWS_REGION": "eu-west-1",
	}
	isolateFromJobCredentials(
		func(k string) error { delete(env, k); return nil },
		func(k, v string) error { env[k] = v; return nil })
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
		"AWS_DEFAULT_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s survived: %q", k, v)
		}
	}
	for _, k := range []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE"} {
		if env[k] != os.DevNull {
			t.Errorf("%s = %q, want %s", k, env[k], os.DevNull)
		}
	}
	if env["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "/v2/credentials/x" || env["AWS_REGION"] != "eu-west-1" {
		t.Errorf("the runner's own identity and the region must stay: %v", env)
	}
}

func TestNewS3TaskIdentityOnlyIgnoresTheJobsKeys(t *testing.T) {
	// With the job's keys in the environment, a plain NewS3 resolves them; with
	// TaskIdentityOnly it must not: nothing else is configured here, so the
	// credential lookup fails and ErrNoStore is returned.
	t.Setenv("AWS_ACCESS_KEY_ID", "job-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "job-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_CONFIG_FILE", "") // restored by the test; NewS3 points them at /dev/null
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "")
	if _, err := NewS3(context.Background(), S3Config{Bucket: "b", Region: "eu-west-1"}); err != nil {
		t.Fatalf("job keys: %v", err)
	}
	if _, err := NewS3(context.Background(), S3Config{Bucket: "b", Region: "eu-west-1", TaskIdentityOnly: true, CredentialTimeout: 2 * time.Second}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("TaskIdentityOnly with only job keys: err %v, want ErrNoStore", err)
	}
}
