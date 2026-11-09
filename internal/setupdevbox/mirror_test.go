package setupdevbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/truvity/ci-actions/internal/runcmd"
)

const (
	flakeA   = "https://github.com/truvity/sluis/releases/download/v1.77.5/sluisctl_1.77.5_nix-flake.tar.gz"
	flakeB   = "https://github.com/truvity/access-roster/releases/download/v1.39.0/accessctl_1.39.0_nix-flake.tar.gz"
	narHashA = "?narHash=sha256-1Ane3celH04STagZPRX6kheUlidmCktJ%2Fc1ZLGXvlIQ%3D"
)

func devboxJSON() string {
	return `{
  // devbox.json is JSONC: a comment must survive
  "packages": {
    "jq": "1.8.1",
    "` + flakeA + `#sluisctl": "",
    "` + flakeB + `#accessctl": ""
  },
  "shell": {
    "init_hook": ["curl -fsSL ` + flakeA + ` -o /dev/null"]
  }
}
`
}

func devboxLock() string {
	return `{
  "lockfile_version": "1",
  "packages": {
    "` + flakeA + `#sluisctl": {
      "last_modified": "1970-01-01T00:00:00Z",
      "resolved": "tarball+` + flakeA + narHashA + `#sluisctl"
    },
    "` + flakeB + `#accessctl": {
      "last_modified": "1970-01-01T00:00:00Z",
      "resolved": "tarball+` + flakeB + `?narHash=sha256-bbbb%3D#accessctl"
    },
    "jq@1.8.1": {
      "resolved": "github:NixOS/nixpkgs/05988b07fb05cbcb50be6bce197b4b5f75b5e61b#jq"
    }
  }
}
`
}

// mirror fakes the cache: it answers each path with its status (200 if absent).
func mirror(t *testing.T, status map[string]int) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if s := status[r.URL.Path]; s != 0 {
			http.Error(w, "no", s)
			return
		}
		fmt.Fprint(w, "tarball bytes")
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestMirrorFlakes(t *testing.T) {
	ctx := context.Background()
	pathA := "/github/truvity/sluis/releases/download/v1.77.5/sluisctl_1.77.5_nix-flake.tar.gz"
	pathB := "/github/truvity/access-roster/releases/download/v1.39.0/accessctl_1.39.0_nix-flake.tar.gz"

	setup := func(t *testing.T, gitTracked bool, env map[string]string) (*h, string) {
		x := newH(t)
		x.chdir()
		for k, v := range env {
			x.env[k] = v
		}
		_ = os.WriteFile("devbox.json", []byte(devboxJSON()), 0o640)
		_ = os.WriteFile("devbox.lock", []byte(devboxLock()), 0o600)
		x.exec = func(c runcmd.Cmd) error {
			if len(c.Args) > 0 && c.Args[0] == "ls-files" && !gitTracked {
				return errors.New("exit status 1")
			}
			return nil
		}
		return x, filepath.Join(x.dir, "ws")
	}

	t.Run("unset or empty variable: nothing is read, written, run or printed", func(t *testing.T) {
		for _, v := range []string{"", "  ", "not a url"} {
			x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": v})
			if err := MirrorFlakes(ctx, x.e()); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile("devbox.json"); string(b) != devboxJSON() {
				t.Errorf("%q: devbox.json changed", v)
			}
			if b, _ := os.ReadFile("devbox.lock"); string(b) != devboxLock() {
				t.Errorf("%q: devbox.lock changed", v)
			}
			if len(x.calls) != 0 || x.out.Len() != 0 || x.err.Len() != 0 {
				t.Errorf("%q: calls %v out %q", v, x.calls, x.out.String())
			}
		}
	})

	t.Run("a mirror that serves: URLs are rewritten, the rest is byte for byte, files are hidden from git", func(t *testing.T) {
		srv, seen := mirror(t, nil)
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github/"})
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err, x.out.String())
		}
		m := func(u string) string { return srv.URL + "/github/" + strings.TrimPrefix(u, "https://github.com/") }

		wantJSON := strings.NewReplacer(`"`+flakeA+`#`, `"`+m(flakeA)+`#`, `"`+flakeB+`#`, `"`+m(flakeB)+`#`).Replace(devboxJSON())
		gotJSON, _ := os.ReadFile("devbox.json")
		if string(gotJSON) != wantJSON {
			t.Errorf("devbox.json:\n%s\nwant:\n%s", gotJSON, wantJSON)
		}
		// only strings that BEGIN with the URL are package references: the
		// init hook's curl still names github.com
		if !strings.Contains(string(gotJSON), `curl -fsSL `+flakeA+` -o /dev/null`) {
			t.Error("a command that mentions the URL was rewritten")
		}
		if !strings.Contains(string(gotJSON), "// devbox.json is JSONC: a comment must survive") {
			t.Error("the comment was lost")
		}

		gotLock, _ := os.ReadFile("devbox.lock")
		for _, want := range []string{
			`"` + m(flakeA) + `#sluisctl": {`,
			`"resolved": "tarball+` + m(flakeA) + narHashA + `#sluisctl"`,
			`"resolved": "tarball+` + m(flakeB) + `?narHash=sha256-bbbb%3D#accessctl"`,
			`"resolved": "github:NixOS/nixpkgs/05988b07fb05cbcb50be6bce197b4b5f75b5e61b#jq"`,
		} {
			if !strings.Contains(string(gotLock), want) {
				t.Errorf("devbox.lock lacks %s\n%s", want, gotLock)
			}
		}
		if strings.Contains(string(gotLock), "https://github.com/") {
			t.Errorf("a github.com URL is left in the lock:\n%s", gotLock)
		}
		// the narHash survives untouched: that is what nix checks the content against
		if strings.Count(string(gotLock), "narHash=") != 2 || !strings.Contains(string(gotLock), narHashA) {
			t.Error("a narHash changed")
		}
		var parsed map[string]any
		if json.Unmarshal(gotLock, &parsed) != nil {
			t.Error("devbox.lock is not JSON any more")
		}

		for _, f := range []string{"devbox.json", "devbox.lock"} {
			st, _ := os.Stat(f)
			want := map[string]os.FileMode{"devbox.json": 0o640, "devbox.lock": 0o600}[f]
			if st.Mode().Perm() != want {
				t.Errorf("%s mode %v", f, st.Mode().Perm())
			}
		}
		for _, w := range []string{"git update-index --assume-unchanged -- devbox.json", "git update-index --assume-unchanged -- devbox.lock"} {
			if !strings.Contains(strings.Join(x.calls, "|"), w) {
				t.Errorf("missing %q in %v", w, x.calls)
			}
		}
		// each distinct URL is asked of the mirror once, without its query string
		if strings.Join(*seen, ",") != pathA+","+pathB {
			t.Errorf("mirror asked for %v", *seen)
		}
	})

	t.Run("a URL the mirror does not serve stays on github.com; the others move", func(t *testing.T) {
		srv, _ := mirror(t, map[string]int{pathB: 504})
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github"})
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
		lock, _ := os.ReadFile("devbox.lock")
		if !strings.Contains(string(lock), "tarball+"+flakeB+"?narHash=sha256-bbbb") || strings.Contains(string(lock), flakeA) {
			t.Errorf("%s", lock)
		}
		js, _ := os.ReadFile("devbox.json")
		if !strings.Contains(string(js), `"`+flakeB+`#accessctl"`) || strings.Contains(string(js), `"`+flakeA+`#sluisctl"`) {
			t.Errorf("%s", js)
		}
		if !strings.Contains(x.out.String(), "::warning::the GitHub releases mirror did not serve "+flakeB) {
			t.Errorf("%q", x.out.String())
		}
	})

	t.Run("a mirror that serves nothing leaves both files exactly as they were", func(t *testing.T) {
		srv, _ := mirror(t, map[string]int{pathA: 404, pathB: 502})
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github"})
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
		js, _ := os.ReadFile("devbox.json")
		lock, _ := os.ReadFile("devbox.lock")
		if string(js) != devboxJSON() || string(lock) != devboxLock() || len(x.calls) != 0 {
			t.Errorf("changed: calls %v", x.calls)
		}
		if strings.Count(x.out.String(), "::warning::") != 2 {
			t.Errorf("%q", x.out.String())
		}
	})

	t.Run("an unreachable mirror leaves both files exactly as they were", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": url + "/github"})
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile("devbox.lock"); string(b) != devboxLock() {
			t.Error("devbox.lock changed")
		}
	})

	t.Run("files git does not track are rewritten and not marked", func(t *testing.T) {
		srv, _ := mirror(t, nil)
		x, _ := setup(t, false, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github"})
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
		for _, c := range x.calls {
			if strings.Contains(c, "update-index") {
				t.Errorf("an untracked file was marked: %v", x.calls)
			}
		}
		if b, _ := os.ReadFile("devbox.lock"); strings.Contains(string(b), "https://github.com/") {
			t.Error("not rewritten")
		}
	})

	t.Run("if git cannot hide the files they are put back", func(t *testing.T) {
		srv, _ := mirror(t, nil)
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github"})
		x.exec = func(c runcmd.Cmd) error {
			if len(c.Args) > 0 && c.Args[0] == "update-index" {
				return errors.New("exit status 128")
			}
			return nil
		}
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
		js, _ := os.ReadFile("devbox.json")
		lock, _ := os.ReadFile("devbox.lock")
		if string(js) != devboxJSON() || string(lock) != devboxLock() {
			t.Error("the originals were not restored")
		}
		if !strings.Contains(x.out.String(), "::warning::git update-index") {
			t.Errorf("%q", x.out.String())
		}
	})

	t.Run("devbox-config: the second environment's files are the ones rewritten", func(t *testing.T) {
		srv, _ := mirror(t, nil)
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github", "DEVBOX_CONFIG": "tools"})
		_ = os.MkdirAll("tools", 0o755)
		_ = os.WriteFile("tools/devbox.json", []byte(devboxJSON()), 0o644)
		_ = os.WriteFile("tools/devbox.lock", []byte(devboxLock()), 0o644)
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile("tools/devbox.lock"); strings.Contains(string(b), "https://github.com/") {
			t.Error("tools/devbox.lock not rewritten")
		}
		if b, _ := os.ReadFile("devbox.lock"); string(b) != devboxLock() {
			t.Error("the repository's own devbox.lock must not be touched")
		}
		if !strings.Contains(strings.Join(x.calls, "|"), "update-index --assume-unchanged -- tools/devbox.lock") {
			t.Errorf("%v", x.calls)
		}
	})

	t.Run("no flake tarballs: nothing is asked and nothing changes", func(t *testing.T) {
		srv, seen := mirror(t, nil)
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github"})
		plain := `{"packages": ["go@1.26", "just@1.40.0"]}` + "\n"
		_ = os.WriteFile("devbox.json", []byte(plain), 0o644)
		_ = os.Remove("devbox.lock")
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile("devbox.json"); string(b) != plain || len(*seen) != 0 || len(x.calls) != 0 {
			t.Errorf("seen %v calls %v", *seen, x.calls)
		}
	})

	t.Run("no devbox.json at all is not an error", func(t *testing.T) {
		srv, _ := mirror(t, nil)
		x, _ := setup(t, true, map[string]string{"CI_RUNNER_GITHUB_RELEASES": srv.URL + "/github"})
		_ = os.Remove("devbox.json")
		_ = os.Remove("devbox.lock")
		if err := MirrorFlakes(ctx, x.e()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestNixAcceptsAMirroredFlakeTarball is the claim MirrorFlakes rests on, run
// against the real nix when there is one: a flake tarball pinned by ?narHash=
// is accepted from ANY URL that serves the same bytes, because narHash hashes
// the unpacked content, and a different content is refused.
func TestNixAcceptsAMirroredFlakeTarball(t *testing.T) {
	nix, err := exec.LookPath("nix")
	if err != nil {
		t.Skip("nix is not on PATH")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no nix here")
	}
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	flake := []byte("{ outputs = { self }: { x = 1; }; }\n")
	_ = tw.WriteHeader(&tar.Header{Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "pkg/flake.nix", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(flake))})
	_, _ = tw.Write(flake)
	tw.Close()
	gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(tgz.Bytes()) }))
	defer srv.Close()

	run := func(ref string) (string, error) {
		home := t.TempDir()
		cmd := exec.Command(nix, "flake", "prefetch", "--json", "--extra-experimental-features", "nix-command flakes", ref)
		cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+filepath.Join(home, "cache"), "HOME="+home)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// the hash nix computes for the content, from the URL a mirror would not be
	out, err := run("tarball+" + srv.URL + "/github.com/o/r/releases/download/v1/x_nix-flake.tar.gz")
	if err != nil {
		t.Skipf("nix cannot fetch here: %v\n%s", err, out)
	}
	var res struct{ Hash string }
	if i := strings.Index(out, "{"); i < 0 || json.Unmarshal([]byte(out[i:]), &res) != nil || res.Hash == "" {
		t.Skipf("unexpected nix output: %s", out)
	}
	enc := strings.NewReplacer("/", "%2F", "=", "%3D", "+", "%2B").Replace(res.Hash)

	// the same bytes under another path: the pinned hash is accepted
	if out, err := run("tarball+" + srv.URL + "/github/o/r/releases/download/v1/x_nix-flake.tar.gz?narHash=" + enc); err != nil {
		t.Errorf("the same content from another URL was refused: %v\n%s", err, out)
	}
	// and a hash that is not the content's is refused
	bad := "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA%3D"
	if out, err := run("tarball+" + srv.URL + "/github/o/r/releases/download/v1/x_nix-flake.tar.gz?narHash=" + bad); err == nil || !strings.Contains(out, "NAR hash mismatch") {
		t.Errorf("a wrong narHash must be refused: %v\n%s", err, out)
	}
}

// ---- the devbox installer through the mirror --------------------------------

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstallDevboxThroughTheMirror(t *testing.T) {
	const dl = "/jetify-com/devbox/releases/download/1.2.3/"
	// github.com and cache.example are both answered by the release fixture,
	// which keeps its files under /download/<version>/; seen records who was asked.
	setup := func(t *testing.T, mirrorStatus int) (*h, *[]string) {
		fixture := release(t, "1.2.3", false)
		var seen []string
		x := newH(t)
		x.env["DEVBOX_VERSION"] = "1.2.3"
		x.env["DEVBOX_RELEASE_BASE"] = "https://github.com/jetify-com/devbox/releases"
		x.http = &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
			seen = append(seen, r.URL.Host+strings.TrimPrefix(r.URL.Path, ""))
			p := ""
			switch r.URL.Host {
			case "github.com":
				p = strings.Replace(r.URL.Path, "/jetify-com/devbox/releases", "", 1)
			case "cache.example":
				if mirrorStatus != 0 {
					return &http.Response{StatusCode: mirrorStatus, Status: fmt.Sprint(mirrorStatus, " mirror says no"), Body: http.NoBody, Header: http.Header{}, Request: r}, nil
				}
				p = strings.Replace(r.URL.Path, "/github/jetify-com/devbox/releases", "", 1)
			default:
				return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
			}
			return http.Get(fixture.URL + p)
		})}
		return x, &seen
	}
	hosts := func(seen []string) string {
		var out []string
		for _, s := range seen {
			out = append(out, strings.SplitN(s, "/", 2)[0])
		}
		return strings.Join(out, ",")
	}

	t.Run("variable unset: github.com only, exactly as before", func(t *testing.T) {
		x, seen := setup(t, 0)
		if err := InstallDevbox(context.Background(), x.e()); err != nil {
			t.Fatal(err, x.out.String())
		}
		if hosts(*seen) != "github.com,github.com" || strings.Contains(x.out.String(), "::warning::") {
			t.Errorf("%v %q", *seen, x.out.String())
		}
	})
	t.Run("variable set: the archive and checksums.txt come from the mirror", func(t *testing.T) {
		x, seen := setup(t, 0)
		x.env["CI_RUNNER_GITHUB_RELEASES"] = "https://cache.example/github/"
		if err := InstallDevbox(context.Background(), x.e()); err != nil {
			t.Fatal(err, x.out.String())
		}
		if hosts(*seen) != "cache.example,cache.example" || !strings.Contains((*seen)[0], "/github"+dl) {
			t.Errorf("%v", *seen)
		}
		if _, err := os.Stat(filepath.Join(x.env["RUNNER_TEMP"], "bin", "devbox")); err != nil {
			t.Error(err)
		}
	})
	t.Run("the mirror answers an error: github.com serves, with a warning", func(t *testing.T) {
		x, seen := setup(t, 404)
		x.env["CI_RUNNER_GITHUB_RELEASES"] = "https://cache.example/github"
		if err := InstallDevbox(context.Background(), x.e()); err != nil {
			t.Fatal(err, x.out.String())
		}
		if !strings.Contains(hosts(*seen), "github.com") || strings.Count(x.out.String(), "::warning::the GitHub releases mirror did not serve") != 2 {
			t.Errorf("%v %q", *seen, x.out.String())
		}
		if _, err := os.Stat(filepath.Join(x.env["RUNNER_TEMP"], "bin", "devbox")); err != nil {
			t.Error(err)
		}
	})
}
