package releasepublic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// copyTree copies src (without golden/) into dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "golden" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The goldens were produced by the Python generator this package replaces,
// run on the same fixtures: every byte of every flake.nix, and the log line.
func TestGoldenFlakes(t *testing.T) {
	for _, name := range []string{"multi", "ascii"} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			copyTree(t, filepath.Join("testdata", name), work)
			var c struct {
				IDs  []string `json:"ids"`
				Tag  string   `json:"tag"`
				Repo string   `json:"repo"`
			}
			b, _ := os.ReadFile(filepath.Join(work, "case.json"))
			if err := json.Unmarshal(b, &c); err != nil {
				t.Fatal(err)
			}
			ids, _ := json.Marshal(c.IDs)
			var out bytes.Buffer
			flakes := filepath.Join(t.TempDir(), "nix-flakes")
			dirs, err := GenerateFlakes(NixOptions{Repo: c.Repo, Tag: c.Tag, Flakes: string(ids), FlakeDir: flakes, Dir: work, Out: &out, Err: os.Stderr})
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", name, "golden")
			want, _ := os.ReadFile(filepath.Join(golden, "stdout.txt"))
			if got := strings.ReplaceAll(out.String(), flakes, "$FLAKE_DIR"); got != string(want) {
				t.Errorf("log:\n%s\nwant:\n%s", got, want)
			}
			entries, _ := os.ReadDir(golden)
			n := 0
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				n++
				w, _ := os.ReadFile(filepath.Join(golden, e.Name(), "flake.nix"))
				g, err := os.ReadFile(filepath.Join(flakes, e.Name(), "flake.nix"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(g, w) {
					t.Errorf("%s differs:\n%s\nwant:\n%s", e.Name(), g, w)
				}
			}
			if n != len(dirs) || n != len(c.IDs) {
				t.Errorf("%d golden flakes, %d generated, %d asked", n, len(dirs), len(c.IDs))
			}
		})
	}
}

// json.dumps of the same strings, from Python.
func TestNixString(t *testing.T) {
	for in, want := range map[string]string{
		"a\"b\\c\n\té ü 😀 \x01 ${x} <&>": `"a\"b\\c\n\t\u00e9 \u00fc \ud83d\ude00 \u0001 ${x} <&>"`,
		"":                               `""`,
		"plain-1.2.3":                    `"plain-1.2.3"`,
		"\x7f\b\f\r":                     "\"\x7f\\b\\f\\r\"",
	} {
		if got := nixString(in); got != want {
			t.Errorf("nixString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestFlakeRefusals(t *testing.T) {
	art := func(id, name, goos, goarch, format string, bins []string, wrap string) map[string]any {
		return map[string]any{"type": "Archive", "name": name, "path": "dist/" + name, "goos": goos, "goarch": goarch, "goamd64": "v1",
			"extra": map[string]any{"ID": id, "Format": format, "Binaries": bins, "WrappedIn": wrap}}
	}
	for _, tc := range []struct {
		name string
		arts []map[string]any
		want string
	}{
		{"a bare binary", []map[string]any{art("t", "t_linux_amd64", "linux", "amd64", "binary", []string{"t"}, "")},
			"::error::archive t_linux_amd64 is binary; a flake needs an archive, not a bare binary\n"},
		{"no linux or darwin archive", []map[string]any{art("t", "t_windows.zip", "windows", "amd64", "zip", []string{"t"}, "")},
			"::error::no linux or darwin archive with id t in dist/artifacts.json\n"},
		{"another id only", []map[string]any{art("x", "x.tgz", "linux", "amd64", "tgz", []string{"x"}, "")},
			"::error::no linux or darwin archive with id t in dist/artifacts.json\n"},
		{"no binaries", []map[string]any{art("t", "t.tgz", "linux", "amd64", "tgz", nil, "")},
			"::error::archive t lists no binaries\n"},
		{"different binaries per platform", []map[string]any{
			art("t", "t_a.tgz", "linux", "amd64", "tgz", []string{"t"}, ""), art("t", "t_b.tgz", "linux", "arm64", "tgz", []string{"t", "u"}, "")},
			"::error::archive t holds different binaries per platform\n"},
		{"different wrapper per platform", []map[string]any{
			art("t", "t_a.tgz", "linux", "amd64", "tgz", []string{"t"}, "w1"), art("t", "t_b.tgz", "linux", "arm64", "tgz", []string{"t"}, "w2")},
			"::error::archive t holds different binaries per platform\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			os.MkdirAll(filepath.Join(work, "dist"), 0o755)
			b, _ := json.Marshal(tc.arts)
			os.WriteFile(filepath.Join(work, "dist", "artifacts.json"), b, 0o644)
			for _, a := range tc.arts {
				os.WriteFile(filepath.Join(work, a["path"].(string)), []byte("x"), 0o644)
			}
			var errb bytes.Buffer
			_, err := GenerateFlakes(NixOptions{Repo: "a/t", Tag: "v1", Flakes: `["t"]`, FlakeDir: t.TempDir(), Dir: work, Out: &bytes.Buffer{}, Err: &errb})
			if !errors.Is(err, ErrFailed) || errb.String() != tc.want {
				t.Errorf("err %v, stderr %q, want %q", err, errb.String(), tc.want)
			}
		})
	}
}

func TestPublishFlakes(t *testing.T) {
	work := t.TempDir()
	copyTree(t, filepath.Join("testdata", "multi"), work)
	flakes := filepath.Join(t.TempDir(), "nix-flakes")
	var calls []string
	exec := func(_ context.Context, c runcmd.Cmd) error {
		line := c.Name + " " + strings.Join(c.Args, " ")
		switch c.Name {
		case "tar", "gzip":
			return runcmd.OS(context.Background(), c)
		case "nix":
			if strings.Contains(line, " build ") {
				c.Stdout.Write([]byte("/nix/store/abc-tool\n"))
			}
		}
		if c.Name != "tar" && c.Name != "gzip" {
			calls = append(calls, strings.ReplaceAll(line, flakes, "$F")+" @"+strings.ReplaceAll(c.Dir, flakes, "$F"))
		}
		return nil
	}
	var out bytes.Buffer
	err := PublishFlakes(context.Background(), NixOptions{Repo: "acme/tool", Tag: "v1.2.3", Flakes: `["tool","other"]`, FlakeDir: flakes, Dir: work, Out: &out, Err: os.Stderr, Exec: exec})
	if err != nil {
		t.Fatal(err)
	}
	const x = "--extra-experimental-features nix-command flakes"
	var want []string
	for _, id := range []string{"other", "tool"} { // glob order
		d := id + "_1.2.3_nix-flake"
		p := "path:$F/" + d
		want = append(want,
			"nix "+x+" flake lock "+p+" @$F",
			"nix "+x+" flake check --no-build --all-systems "+p+" @$F",
			"nix "+x+" build --no-link --print-out-paths "+p+"#"+id+" @$F",
			"ls -l /nix/store/abc-tool/bin @$F",
			"gh release upload v1.2.3 "+d+".tar.gz --clobber --repo acme/tool @$F")
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), "::group::other_1.2.3_nix-flake\n") || !strings.HasSuffix(out.String(), "::endgroup::\n") {
		t.Errorf("log %q", out.String())
	}
	// The archive is the deterministic one the shell pipeline wrote.
	for _, d := range []string{"other_1.2.3_nix-flake", "tool_1.2.3_nix-flake"} {
		if _, err := os.Stat(filepath.Join(flakes, d+".tar.gz")); err != nil {
			t.Error(err)
		}
	}
}

func TestPublishStopsOnAFailedStep(t *testing.T) {
	work := t.TempDir()
	copyTree(t, filepath.Join("testdata", "ascii"), work)
	n := 0
	err := PublishFlakes(context.Background(), NixOptions{Repo: "acme/x", Tag: "1.0", Flakes: `["t\"ü"]`, FlakeDir: t.TempDir(), Dir: work,
		Exec: func(_ context.Context, c runcmd.Cmd) error {
			n++
			if strings.Contains(strings.Join(c.Args, " "), "flake check") {
				return errors.New("boom")
			}
			return nil
		}})
	if err == nil || n != 2 {
		t.Errorf("err %v after %d commands: a flake that fails its check must never be uploaded", err, n)
	}
}

func TestPackIsDeterministic(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "f_1_nix-flake"), 0o755)
	os.WriteFile(filepath.Join(dir, "f_1_nix-flake", "flake.nix"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "f_1_nix-flake", "flake.lock"), []byte("{}\n"), 0o644)
	var sums []string
	for i := 0; i < 2; i++ {
		if err := packDeterministic(context.Background(), NixOptions{FlakeDir: dir, Exec: runcmd.OS, Err: os.Stderr}, "f_1_nix-flake"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "f_1_nix-flake.tar.gz"))
		sums = append(sums, string(b))
		os.Chtimes(filepath.Join(dir, "f_1_nix-flake", "flake.nix"), testTime(i), testTime(i))
	}
	if sums[0] != sums[1] || len(sums[0]) == 0 {
		t.Error("the same tree must pack to the same bytes whatever its mtimes")
	}
	// What the old pipeline wrote, byte for byte.
	want, err := exec.Command("sh", "-c", "tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf - f_1_nix-flake | gzip -n").Output()
	cmd := exec.Command("sh", "-c", "tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf - f_1_nix-flake | gzip -n")
	cmd.Dir = dir
	want, err = cmd.Output()
	if err != nil || string(want) != sums[0] {
		t.Errorf("differs from the shell pipeline (err %v)", err)
	}
}

func testTime(i int) time.Time { return time.Unix(1000000000+int64(i)*86400, 0) }
