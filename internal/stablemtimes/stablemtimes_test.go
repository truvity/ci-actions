package stablemtimes

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repo(t *testing.T, a, b string) string {
	t.Helper()
	d := t.TempDir()
	git(t, d, "init", "-q")
	write(t, filepath.Join(d, "a.txt"), a)
	write(t, filepath.Join(d, "sub/deep/b.txt"), b)
	if err := os.Symlink("a.txt", filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	git(t, d, "add", "-A")
	return d
}

func apply(t *testing.T, d string) (Result, string) {
	t.Helper()
	var out, errb bytes.Buffer
	r, err := Run(context.Background(), Options{Dir: d, Out: &out, Err: &errb})
	if err != nil {
		t.Fatal(err)
	}
	if errb.Len() != 0 {
		t.Fatalf("warnings: %s", errb.String())
	}
	return r, out.String()
}

func mtime(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime().UTC()
}

func TestSameContentSameMtimeAcrossClones(t *testing.T) {
	d1, d2 := repo(t, "one", "two"), repo(t, "one", "two")
	time.Sleep(1100 * time.Millisecond) // the clones are stamped at different moments
	apply(t, d1)
	apply(t, d2)
	for _, p := range []string{"a.txt", "sub/deep/b.txt"} {
		if a, b := mtime(t, filepath.Join(d1, p)), mtime(t, filepath.Join(d2, p)); !a.Equal(b) {
			t.Errorf("%s: %v != %v", p, a, b)
		}
	}
	got := mtime(t, filepath.Join(d1, "a.txt"))
	if got.Before(Epoch) || !got.Before(Epoch.Add(spanSeconds*time.Second)) {
		t.Errorf("a.txt time %v outside the window", got)
	}
}

func TestChangedContentDifferentMtime(t *testing.T) {
	d1, d2 := repo(t, "one", "two"), repo(t, "one", "changed")
	apply(t, d1)
	apply(t, d2)
	if a, b := mtime(t, filepath.Join(d1, "sub/deep/b.txt")), mtime(t, filepath.Join(d2, "sub/deep/b.txt")); a.Equal(b) {
		t.Errorf("changed content kept mtime %v", a)
	}
	if a, b := mtime(t, filepath.Join(d1, "a.txt")), mtime(t, filepath.Join(d2, "a.txt")); !a.Equal(b) {
		t.Errorf("unchanged file moved: %v != %v", a, b)
	}
}

func TestDirectoriesAreConstant(t *testing.T) {
	d := repo(t, "one", "two")
	r, line := apply(t, d)
	for _, p := range []string{".", "sub", "sub/deep"} {
		if got := mtime(t, filepath.Join(d, p)); !got.Equal(Epoch) {
			t.Errorf("dir %s = %v, want %v", p, got, Epoch)
		}
	}
	if r.Dirs != 3 || r.Files != 2 {
		t.Errorf("result %+v", r)
	}
	if line == "" {
		t.Error("no summary line")
	}
}

func TestSymlinkOwnTime(t *testing.T) {
	d := repo(t, "one", "two")
	r, _ := apply(t, d)
	if runtime.GOOS != "linux" {
		if r.Symlinks != 0 || r.Skipped != 1 {
			t.Errorf("result %+v", r)
		}
		return
	}
	if r.Symlinks != 1 {
		t.Fatalf("result %+v", r)
	}
	want, err := FileTime(blob(t, d, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if got := mtime(t, filepath.Join(d, "link")); !got.Equal(want) {
		t.Errorf("link time %v, want %v", got, want)
	}
}

func blob(t *testing.T, d, path string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ":"+path)
	cmd.Dir = d
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(out))
}

func TestGitlinkAndMissingSkipped(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "keep.txt"), "x")
	if err := os.MkdirAll(filepath.Join(d, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := mtime(t, filepath.Join(d, "mod"))
	list := "100644 " + "0123456789abcdef0123456789abcdef01234567" + " 0\tkeep.txt\x00" +
		"160000 0123456789abcdef0123456789abcdef01234567 0\tmod\x00" +
		"100644 0123456789abcdef0123456789abcdef01234567 0\tabsent/file.txt\x00"
	var out bytes.Buffer
	r, err := Run(context.Background(), Options{Dir: d, Out: &out, ListFiles: func(context.Context, string) ([]byte, error) { return []byte(list), nil }})
	if err != nil {
		t.Fatal(err)
	}
	if r.Files != 1 || r.Skipped != 2 {
		t.Errorf("result %+v", r)
	}
	if got := mtime(t, filepath.Join(d, "mod")); !got.Equal(before) {
		t.Errorf("gitlink directory touched: %v -> %v", before, got)
	}
	if _, err := os.Stat(filepath.Join(d, "absent")); !os.IsNotExist(err) {
		t.Errorf("missing path appeared: %v", err)
	}
}

func TestIdempotent(t *testing.T) {
	d := repo(t, "one", "two")
	apply(t, d)
	paths := []string{".", "a.txt", "link", "sub", "sub/deep", "sub/deep/b.txt"}
	first := map[string]time.Time{}
	for _, p := range paths {
		first[p] = mtime(t, filepath.Join(d, p))
	}
	_, l1 := apply(t, d)
	_, l2 := apply(t, d)
	for _, p := range paths {
		if got := mtime(t, filepath.Join(d, p)); !got.Equal(first[p]) {
			t.Errorf("%s changed: %v -> %v", p, first[p], got)
		}
	}
	if l1 != l2 {
		t.Errorf("summaries differ: %q vs %q", l1, l2)
	}
}

func TestFileTimeRejectsShortID(t *testing.T) {
	if _, err := FileTime("abc"); err == nil {
		t.Error("want error")
	}
}
