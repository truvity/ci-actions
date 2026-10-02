package setupdevbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/truvity/ci-actions/internal/runcmd"
)

type h struct {
	t      *testing.T
	dir    string
	env    map[string]string
	out    bytes.Buffer
	err    bytes.Buffer
	calls  []string
	sleeps []time.Duration
	exec   func(c runcmd.Cmd) error
	clock  time.Time
}

func newH(t *testing.T) *h {
	dir := t.TempDir()
	x := &h{t: t, dir: dir, clock: time.Unix(1000, 0), env: map[string]string{
		"HOME": filepath.Join(dir, "home"), "RUNNER_TEMP": filepath.Join(dir, "temp"), "GITHUB_WORKSPACE": filepath.Join(dir, "ws"),
		"GITHUB_ENV": filepath.Join(dir, "github_env"), "GITHUB_OUTPUT": filepath.Join(dir, "github_output"),
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"), "GITHUB_PATH": filepath.Join(dir, "github_path"),
		"PATH": "",
	}}
	for _, d := range []string{"home", "temp", "ws"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	return x
}

func (x *h) e() Env {
	return Env{
		Getenv: func(k string) string { return x.env[k] },
		Lookup: func(k string) (string, bool) { v, ok := x.env[k]; return v, ok },
		Out:    &x.out, Err: &x.err,
		Exec: func(_ context.Context, c runcmd.Cmd) error {
			x.calls = append(x.calls, strings.TrimSpace(c.Name+" "+strings.Join(c.Args, " ")))
			if x.exec != nil {
				return x.exec(c)
			}
			return nil
		},
		Sleep: func(d time.Duration) { x.sleeps = append(x.sleeps, d); x.clock = x.clock.Add(d) },
		Now:   func() time.Time { return x.clock },
	}
}

func (x *h) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(x.dir, name))
	return string(b)
}

// chdir into a fresh working directory for the steps that read ./files.
func (x *h) chdir() {
	wd, _ := os.Getwd()
	ws := x.env["GITHUB_WORKSPACE"]
	if err := os.Chdir(ws); err != nil {
		x.t.Fatal(err)
	}
	x.t.Cleanup(func() { os.Chdir(wd) })
}

// ---- preflight --------------------------------------------------------------

func preflightEnv(x *h, extra map[string]string) Env {
	status := filepath.Join(x.dir, "status")
	_ = os.WriteFile(status, []byte("Name:\tx\nNoNewPrivs:\t1\n"), 0o644)
	x.env["PREFLIGHT_PROC_STATUS"] = status
	x.env["PREFLIGHT_SH_PATH"] = filepath.Join(x.dir, "sh-is-bash")
	bash, _ := filepath.EvalSymlinks(mustLookPath(x.t, "bash"))
	_ = os.Symlink(bash, filepath.Join(x.dir, "sh-is-bash"))
	x.env["PATH"] = filepath.Dir(bash)
	x.env["PREFLIGHT_EXPECT_NIX"] = "false"
	x.env["PREFLIGHT_NIX_BIN"] = "/stub/nix"
	x.env["PREFLIGHT_DEVBOX_BIN"] = "/stub/devbox"
	for k, v := range extra {
		x.env[k] = v
	}
	return x.e()
}

func mustLookPath(t *testing.T, name string) string {
	p := pathLookup(name, os.Getenv("PATH"))
	if p == "" {
		t.Skip(name + " is not on PATH")
	}
	return p
}

func TestPreflight(t *testing.T) {
	status := func(x *h, nnp string) string {
		p := filepath.Join(x.dir, "status-"+nnp)
		_ = os.WriteFile(p, []byte("Name:\tx\nNoNewPrivs:\t"+nnp+"\n"), 0o644)
		return p
	}
	t.Run("a restricted-shaped environment passes", func(t *testing.T) {
		x := newH(t)
		err := Preflight(context.Background(), preflightEnv(x, nil))
		o := x.out.String()
		if err != nil || !strings.Contains(o, "no_new_privs=1") || !strings.Contains(o, "uid=") || !strings.Contains(o, "(bash)") || !strings.HasSuffix(o, "preflight: ok\n") {
			t.Errorf("%v\n%s", err, o)
		}
	})
	t.Run("sh not being bash, or no_new_privs unset, is not a failure", func(t *testing.T) {
		x := newH(t)
		dash := mustLookPath(t, "ls") // any non-bash binary
		_ = os.Symlink(dash, filepath.Join(x.dir, "sh-is-other"))
		e := preflightEnv(x, map[string]string{"PREFLIGHT_SH_PATH": filepath.Join(x.dir, "sh-is-other"), "PREFLIGHT_PROC_STATUS": status(x, "0")})
		if err := Preflight(context.Background(), e); err != nil || !strings.Contains(x.out.String(), "not bash") || !strings.Contains(x.out.String(), "no_new_privs=0") {
			t.Errorf("%v\n%s", err, x.out.String())
		}
	})
	t.Run("no_new_privs unreadable is reported", func(t *testing.T) {
		x := newH(t)
		e := preflightEnv(x, map[string]string{"PREFLIGHT_PROC_STATUS": filepath.Join(x.dir, "nope")})
		_ = Preflight(context.Background(), e)
		if !strings.Contains(x.out.String(), "no_new_privs=unknown (could not read NoNewPrivs from "+filepath.Join(x.dir, "nope")+")") {
			t.Errorf("%s", x.out.String())
		}
	})
	for _, v := range []struct{ env, label string }{{"HOME", "HOME"}, {"RUNNER_TEMP", "RUNNER_TEMP"}, {"GITHUB_WORKSPACE", "workdir"}} {
		t.Run("an unwritable "+v.label+" fails with ONE ::error:: naming it", func(t *testing.T) {
			x := newH(t)
			e := preflightEnv(x, map[string]string{v.env: filepath.Join(x.dir, "does-not-exist")})
			err := Preflight(context.Background(), e)
			if !errors.Is(err, ErrFailed) || strings.Count(x.out.String(), "::error::") != 1 || !strings.Contains(x.out.String(), v.label+" (") {
				t.Errorf("%v\n%s", err, x.out.String())
			}
		})
	}
	t.Run("several problems are reported in a single ::error::", func(t *testing.T) {
		x := newH(t)
		e := preflightEnv(x, map[string]string{"HOME": filepath.Join(x.dir, "n1"), "RUNNER_TEMP": filepath.Join(x.dir, "n2")})
		_ = Preflight(context.Background(), e)
		if n := strings.Count(x.out.String(), "::error::"); n != 1 || !strings.Contains(x.out.String(), "HOME") || !strings.Contains(x.out.String(), "RUNNER_TEMP") {
			t.Errorf("%d\n%s", n, x.out.String())
		}
	})
	t.Run("unset variables are named", func(t *testing.T) {
		x := newH(t)
		e := preflightEnv(x, nil)
		delete(x.env, "HOME")
		if err := Preflight(context.Background(), e); !errors.Is(err, ErrFailed) || !strings.Contains(x.out.String(), "preflight: HOME is not set\n") || !strings.Contains(x.out.String(), "HOME is not set; ") && !strings.Contains(x.out.String(), "failed: HOME is not set") {
			t.Errorf("%v\n%s", err, x.out.String())
		}
	})
	t.Run("an expected nix that is absent fails", func(t *testing.T) {
		x := newH(t)
		e := preflightEnv(x, map[string]string{"PREFLIGHT_EXPECT_NIX": "true", "PREFLIGHT_NIX_BIN": ""})
		if err := Preflight(context.Background(), e); !errors.Is(err, ErrFailed) || !strings.Contains(x.out.String(), "nix expected but not on PATH") {
			t.Errorf("%v\n%s", err, x.out.String())
		}
	})
	t.Run("no nix on a runner that can escalate: the root step is named, not a failure", func(t *testing.T) {
		x := newH(t)
		e := preflightEnv(x, map[string]string{"PREFLIGHT_NIX_BIN": "", "PREFLIGHT_PROC_STATUS": status(x, "0")})
		o := ""
		err := Preflight(context.Background(), e)
		o = x.out.String()
		if err != nil || !strings.Contains(o, "nix is NOT baked: the install step will run the nix installer, which needs root") ||
			!strings.Contains(o, "the one root step in this action, used only when nix is not baked") {
			t.Errorf("%v\n%s", err, o)
		}
	})
	t.Run("no nix on a restricted runner fails with one error naming the fix", func(t *testing.T) {
		x := newH(t)
		e := preflightEnv(x, map[string]string{"PREFLIGHT_NIX_BIN": "", "PREFLIGHT_PROC_STATUS": status(x, "1")})
		err := Preflight(context.Background(), e)
		o := x.out.String()
		if !errors.Is(err, ErrFailed) || strings.Count(o, "::error::") != 1 || !strings.Contains(o, "nix is not on PATH and this runner cannot escalate privilege") ||
			!strings.Contains(o, "use a runner image that bakes nix") || !strings.Contains(o, "GitHub-hosted runners only") {
			t.Errorf("%v\n%s", err, o)
		}
	})
	t.Run("baked nix and devbox", func(t *testing.T) {
		x := newH(t)
		_ = Preflight(context.Background(), preflightEnv(x, nil))
		if !strings.Contains(x.out.String(), "nix is baked (/stub/nix); the nix installer will not run") || !strings.Contains(x.out.String(), "devbox is baked (/stub/devbox); nothing to install") {
			t.Errorf("%s", x.out.String())
		}
	})
	t.Run("no devbox: it is installed without privilege, not a failure", func(t *testing.T) {
		x := newH(t)
		err := Preflight(context.Background(), preflightEnv(x, map[string]string{"PREFLIGHT_DEVBOX_BIN": ""}))
		if err != nil || !strings.Contains(x.out.String(), "devbox is not baked; the install step will fetch the release binary into $RUNNER_TEMP/bin (checksum-verified, no privilege)") {
			t.Errorf("%v\n%s", err, x.out.String())
		}
	})
}

// ---- install-devbox ---------------------------------------------------------

func release(t *testing.T, version string, tamper bool) *httptest.Server {
	t.Helper()
	var os_, arch string
	os_ = runtime.GOOS
	arch = map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" || (os_ != "linux" && os_ != "darwin") {
		t.Skip("no release binary for this platform")
	}
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho devbox " + version + "\n")
	_ = tw.WriteHeader(&tar.Header{Name: "devbox", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(tgz.Bytes())
	name := fmt.Sprintf("devbox_%s_%s_%s.tar.gz", version, os_, arch)
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n" + strings.Repeat("f", 64) + "  devbox_" + version + "_linux_riscv64.tar.gz\n"
	payload := tgz.Bytes()
	if tamper {
		payload = append(append([]byte(nil), payload...), []byte("tampered")...)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/download/"+version+"/"+name, func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("/download/"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sums) })
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/"+version, http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallDevbox(t *testing.T) {
	t.Run("the published binary is installed into RUNNER_TEMP/bin, executable, on PATH", func(t *testing.T) {
		srv := release(t, "1.2.3", false)
		x := newH(t)
		x.env["DEVBOX_RELEASE_BASE"], x.env["DEVBOX_VERSION"] = srv.URL, "1.2.3"
		if err := InstallDevbox(context.Background(), x.e()); err != nil {
			t.Fatal(err, x.out.String())
		}
		bin := filepath.Join(x.env["RUNNER_TEMP"], "bin", "devbox")
		if st, err := os.Stat(bin); err != nil || st.Mode()&0o111 == 0 {
			t.Errorf("%v", err)
		}
		if got := x.file("github_path"); got != filepath.Join(x.env["RUNNER_TEMP"], "bin")+"\n" {
			t.Errorf("GITHUB_PATH %q", got)
		}
		if !regexp.MustCompile(`^devbox 1\.2\.3 installed to .*/bin/devbox \(sha256 [0-9a-f]{64} verified, no privilege\)\n$`).MatchString(x.out.String()) {
			t.Errorf("%q", x.out.String())
		}
		if ents, _ := os.ReadDir(x.env["RUNNER_TEMP"]); len(ents) != 1 {
			t.Errorf("the download directory is cleaned up: %v", ents)
		}
	})
	t.Run("an archive that does not match checksums.txt is refused, and nothing is installed", func(t *testing.T) {
		srv := release(t, "1.2.3", true)
		x := newH(t)
		x.env["DEVBOX_RELEASE_BASE"], x.env["DEVBOX_VERSION"] = srv.URL, "1.2.3"
		err := InstallDevbox(context.Background(), x.e())
		name := fmt.Sprintf("devbox_1.2.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
		if !errors.Is(err, ErrFailed) || x.out.String() != "::error::checksum mismatch for "+name+"; refusing to install it\n" {
			t.Errorf("%v %q", err, x.out.String())
		}
		if _, err := os.Stat(filepath.Join(x.env["RUNNER_TEMP"], "bin", "devbox")); err == nil {
			t.Error("a tampered archive must never be installed")
		}
	})
	t.Run("an asset the checksums do not list is refused", func(t *testing.T) {
		srv := release(t, "1.2.3", false)
		x := newH(t)
		x.env["DEVBOX_RELEASE_BASE"], x.env["DEVBOX_VERSION"] = srv.URL, "1.2.3"
		x.env["DEVBOX_UNAME_M"] = map[string]string{"amd64": "aarch64", "arm64": "x86_64"}[runtime.GOARCH] // the other arch has no asset
		err := InstallDevbox(context.Background(), x.e())
		if !errors.Is(err, ErrFailed) || !strings.Contains(x.out.String(), "::error::could not download ") {
			t.Errorf("%v %q", err, x.out.String())
		}
	})
	t.Run("a release that does not exist fails with one ::error::", func(t *testing.T) {
		srv := release(t, "1.2.3", false)
		x := newH(t)
		x.env["DEVBOX_RELEASE_BASE"], x.env["DEVBOX_VERSION"] = srv.URL, "9.9.9"
		err := InstallDevbox(context.Background(), x.e())
		if !errors.Is(err, ErrFailed) || strings.Count(x.out.String(), "::error::") != 1 {
			t.Errorf("%v %q", err, x.out.String())
		}
	})
	t.Run("an architecture with no release binary is named", func(t *testing.T) {
		x := newH(t)
		x.env["DEVBOX_UNAME_M"], x.env["DEVBOX_VERSION"] = "riscv64", "1.2.3"
		if err := InstallDevbox(context.Background(), x.e()); !errors.Is(err, ErrFailed) || x.out.String() != "::error::cannot install devbox for riscv64: no release binary for it\n" {
			t.Errorf("%v %q", err, x.out.String())
		}
	})
	t.Run("latest is resolved to the tag the releases page redirects to", func(t *testing.T) {
		srv := release(t, "1.2.3", false)
		x := newH(t)
		x.env["DEVBOX_RELEASE_BASE"] = srv.URL
		if err := InstallDevbox(context.Background(), x.e()); err != nil || !strings.Contains(x.out.String(), "devbox 1.2.3 installed") {
			t.Errorf("%v %q", err, x.out.String())
		}
	})
	t.Run("a redirect that names no tag is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "no redirect") }))
		defer srv.Close()
		x := newH(t)
		x.env["DEVBOX_RELEASE_BASE"] = srv.URL
		if err := InstallDevbox(context.Background(), x.e()); !errors.Is(err, ErrFailed) || !strings.Contains(x.out.String(), "named no tag ("+srv.URL+"/latest)") {
			t.Errorf("%v %q", err, x.out.String())
		}
	})
}

// ---- the small steps --------------------------------------------------------

var heredocRe = regexp.MustCompile(`(?s)^([A-Z_]+)<<(EOF_[0-9a-f]{32})\n(.*)\n(EOF_[0-9a-f]{32})\n$`)

func envBlock(t *testing.T, s string) (name, value string) {
	m := heredocRe.FindStringSubmatch(s)
	if m == nil || m[2] != m[4] {
		t.Fatalf("not a random-delimiter heredoc: %q", s)
	}
	if strings.Contains(m[3], m[2]) {
		t.Fatal("the value contains its own delimiter")
	}
	return m[1], m[3]
}

func TestAWSConfig(t *testing.T) {
	x := newH(t)
	x.env["AWS_CONFIG_REL"] = "aws-ci.ini"
	if err := AWSConfig(x.e()); err != nil {
		t.Fatal(err)
	}
	if n, v := envBlock(t, x.file("github_env")); n != "AWS_CONFIG_FILE" || v != x.env["GITHUB_WORKSPACE"]+"/aws-ci.ini" {
		t.Errorf("%s=%s", n, v)
	}
	// a value with a newline cannot define a second variable: it stays inside its own block
	x = newH(t)
	x.env["AWS_CONFIG_REL"] = "x\nPATH=/evil"
	_ = AWSConfig(x.e())
	n, v := envBlock(t, x.file("github_env"))
	if n != "AWS_CONFIG_FILE" || !strings.HasSuffix(v, "x\nPATH=/evil") || strings.Count(x.file("github_env"), "\n") != 4 {
		t.Errorf("%q", x.file("github_env"))
	}
}

func TestDetectBaked(t *testing.T) {
	x := newH(t)
	bin := filepath.Join(x.dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "devbox"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(bin, "nix"), []byte("not executable"), 0o644)
	x.env["PATH"] = bin
	x.chdir()
	if err := DetectBaked(x.e()); err != nil {
		t.Fatal(err)
	}
	if got := x.file("github_output"); got != "nix=false\ndevbox=true\nprototools=false\n" {
		t.Errorf("%q", got)
	}
	_ = os.WriteFile(".prototools", []byte("node = \"22\"\n"), 0o644)
	_ = os.Remove(filepath.Join(x.dir, "github_output"))
	_ = DetectBaked(x.e())
	if got := x.file("github_output"); !strings.HasSuffix(got, "prototools=true\n") {
		t.Errorf("%q", got)
	}
}

func TestStripLocalTools(t *testing.T) {
	x := newH(t)
	x.chdir()
	if err := StripLocalTools(context.Background(), x.e()); err != nil || x.out.String() != "no .prototools; nothing to strip\n" || len(x.calls) != 0 {
		t.Errorf("%v %q %v", err, x.out.String(), x.calls)
	}
	_ = os.WriteFile(".prototools", []byte("node = \"22\"\n\"npm:@cubic-dev-ai/cli\" = \"1\"\ngo = \"1.25\""), 0o640)
	if err := StripLocalTools(context.Background(), x.e()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(".prototools")
	if string(b) != "node = \"22\"\ngo = \"1.25\"" {
		t.Errorf("%q", b)
	}
	if st, _ := os.Stat(".prototools"); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", st.Mode())
	}
	if strings.Join(x.calls, "|") != "git update-index --assume-unchanged .prototools" {
		t.Errorf("%v", x.calls)
	}
}

func TestMaterializeAndProto(t *testing.T) {
	x := newH(t)
	n := 0
	x.exec = func(c runcmd.Cmd) error {
		n++
		x.clock = x.clock.Add(7 * time.Second)
		if n == 1 {
			return errors.New("flaky")
		}
		return nil
	}
	if err := Materialize(context.Background(), x.e()); err != nil {
		t.Fatal(err)
	}
	if x.out.String() != "attempt 1 failed; sleeping\n" || len(x.sleeps) != 1 || x.sleeps[0] != 20*time.Second {
		t.Errorf("%q %v", x.out.String(), x.sleeps)
	}
	if got := x.file("summary"); got != "- devbox closure: 34s\n" {
		t.Errorf("%q", got)
	}
	if strings.Join(x.calls, "|") != "devbox run -- true|devbox run -- true" {
		t.Errorf("%v", x.calls)
	}
	// two failures then a final attempt whose status propagates
	x = newH(t)
	x.exec = func(c runcmd.Cmd) error { return errors.New("down") }
	if err := Materialize(context.Background(), x.e()); err == nil || len(x.calls) != 3 || len(x.sleeps) != 2 {
		t.Errorf("%v %v %v", err, x.calls, x.sleeps)
	}
	if x.file("summary") != "" {
		t.Error("no summary line for a failed closure")
	}

	// proto: output is shown and kept, the summary counts install lines
	x = newH(t)
	x.env["RUNNER_ARCH"] = "ARM64"
	x.exec = func(c runcmd.Cmd) error {
		fmt.Fprintln(c.Stdout, "node installed")
		fmt.Fprintln(c.Stdout, "go installed")
		fmt.Fprintln(c.Stdout, "already there")
		x.clock = x.clock.Add(3 * time.Second)
		return nil
	}
	if err := Proto(context.Background(), x.e()); err != nil {
		t.Fatal(err)
	}
	if got := x.file("summary"); got != "### Bootstrap (ARM64)\n- proto use: 3s (2 install lines; warm cache = near-zero)\n" {
		t.Errorf("%q", got)
	}
	if b := x.file("temp/bootstrap-proto.log"); b != "node installed\ngo installed\nalready there\n" || x.out.String() != b {
		t.Errorf("log %q out %q", b, x.out.String())
	}
	if x.calls[0] != "devbox run -- proto use" {
		t.Errorf("%v", x.calls)
	}
}

func TestExposeToken(t *testing.T) {
	x := newH(t)
	x.env["MODULE_TOKEN"] = "ghs_abc.def_123"
	if err := ExposeToken(x.e()); err != nil {
		t.Fatal(err)
	}
	if x.out.String() != "::add-mask::ghs_abc.def_123\n" {
		t.Errorf("%q", x.out.String())
	}
	if n, v := envBlock(t, x.file("github_env")); n != "CI_CONTENTS_TOKEN" || v != "ghs_abc.def_123" {
		t.Errorf("%s=%s", n, v)
	}
	for name, tok := range map[string]string{"empty": "", "a newline": "ghs_a\nPATH=/evil", "a carriage return": "ghs_a\rb"} {
		x := newH(t)
		x.env["MODULE_TOKEN"] = tok
		err := ExposeToken(x.e())
		if !errors.Is(err, ErrFailed) || !strings.HasSuffix(x.out.String(), "::error::the minted token is empty or contains a newline; refusing to write it to GITHUB_ENV\n") {
			t.Errorf("%s: %v %q", name, err, x.out.String())
		}
		if x.file("github_env") != "" {
			t.Errorf("%s: nothing is written", name)
		}
	}
	// a real installation token is 383 characters with dots: never rejected on its shape
	x = newH(t)
	x.env["MODULE_TOKEN"] = "ghs_" + strings.Repeat("aB.3_", 75)
	if err := ExposeToken(x.e()); err != nil {
		t.Errorf("%v", err)
	}
}

func TestGoPrivate(t *testing.T) {
	x := newH(t)
	x.env["MODULE_TOKEN"], x.env["GO_PRIVATE"] = "ghs_tok", "github.com/example,example.com/*"
	if err := GoPrivate(context.Background(), x.e()); err != nil {
		t.Fatal(err)
	}
	if n, v := envBlock(t, x.file("github_env")); n != "GOPRIVATE" || v != "github.com/example,example.com/*" {
		t.Errorf("%s=%s", n, v)
	}
	if x.out.String() != "::add-mask::ghs_tok\n" {
		t.Errorf("masked first: %q", x.out.String())
	}
	if strings.Join(x.calls, "|") != "git config --global --replace-all url.https://x-access-token:ghs_tok@github.com/.insteadOf https://github.com/" {
		t.Errorf("%v", x.calls)
	}
	for name, v := range map[string]string{
		"a newline injects a second variable": "github.com/example\nPATH=/evil",
		"a space":                             "github.com/a b",
		"a dollar":                            "github.com/$X",
		"empty":                               "",
		"a quote":                             `github.com/"x`,
	} {
		x := newH(t)
		x.env["MODULE_TOKEN"], x.env["GO_PRIVATE"] = "ghs_tok", v
		err := GoPrivate(context.Background(), x.e())
		if !errors.Is(err, ErrFailed) || !strings.HasSuffix(x.out.String(), "::error::go-private must be a comma-separated list of module path patterns; got something outside [A-Za-z0-9._~/*,-]\n") || x.file("github_env") != "" || len(x.calls) != 0 {
			t.Errorf("%s: %v %q", name, err, x.out.String())
		}
	}
	x = newH(t)
	x.env["GO_PRIVATE"] = "github.com/example"
	if err := GoPrivate(context.Background(), x.e()); !errors.Is(err, ErrFailed) || x.out.String() != "::add-mask::\n::error::go-private is set but no module token was minted — check module-app-client-id and module-app-private-key\n" {
		t.Errorf("%v %q", err, x.out.String())
	}
}

func TestCodeArtifact(t *testing.T) {
	setup := func(t *testing.T) *h {
		x := newH(t)
		x.env["CA_DOMAIN"], x.env["CA_OWNER"], x.env["CA_REGION"] = "dom", "111122223333", "eu-west-1"
		return x
	}
	awsArgs := "codeartifact get-authorization-token --domain dom --domain-owner 111122223333 --region eu-west-1 --query authorizationToken --output text"
	t.Run("aws on PATH", func(t *testing.T) {
		x := setup(t)
		bin := filepath.Join(x.dir, "bin")
		_ = os.MkdirAll(bin, 0o755)
		_ = os.WriteFile(filepath.Join(bin, "aws"), []byte("#!/bin/sh\n"), 0o755)
		x.env["PATH"] = bin
		x.exec = func(c runcmd.Cmd) error { fmt.Fprintln(c.Stdout, "tok.en-value"); return nil }
		if err := CodeArtifact(context.Background(), x.e()); err != nil {
			t.Fatal(err)
		}
		if x.calls[0] != "aws "+awsArgs || x.out.String() != "::add-mask::tok.en-value\n" {
			t.Errorf("%v %q", x.calls, x.out.String())
		}
		if n, v := envBlock(t, x.file("github_env")); n != "CODEARTIFACT_AUTH_TOKEN" || v != "tok.en-value" {
			t.Errorf("%s=%s", n, v)
		}
	})
	t.Run("through devbox: only the last line is the token", func(t *testing.T) {
		x := setup(t)
		x.exec = func(c runcmd.Cmd) error {
			fmt.Fprint(c.Stdout, "Info: Installing\nInfo: ok\ntoken-from-devbox\r\n")
			return nil
		}
		if err := CodeArtifact(context.Background(), x.e()); err != nil {
			t.Fatal(err)
		}
		if x.calls[0] != "devbox run -- aws "+awsArgs {
			t.Errorf("%v", x.calls)
		}
		if _, v := envBlock(t, x.file("github_env")); v != "token-from-devbox" {
			t.Errorf("%q", v)
		}
	})
	t.Run("a failed login says what is missing", func(t *testing.T) {
		x := setup(t)
		x.exec = func(c runcmd.Cmd) error { return errors.New("exit 255") }
		err := CodeArtifact(context.Background(), x.e())
		if !errors.Is(err, ErrFailed) || x.out.String() != "::error::CodeArtifact login failed for domain dom. The identity this build runs as needs codeartifact:GetAuthorizationToken plus sts:GetServiceBearerToken — see <cluster>-codeartifact-read.\n" {
			t.Errorf("%v %q", err, x.out.String())
		}
	})
	for _, empty := range []string{"", "None", "None\n"} {
		x := setup(t)
		x.exec = func(c runcmd.Cmd) error { fmt.Fprint(c.Stdout, empty); return nil }
		err := CodeArtifact(context.Background(), x.e())
		if !errors.Is(err, ErrFailed) || x.out.String() != "::error::CodeArtifact returned an empty token for domain dom; refusing to continue, because an empty token fetches anonymously and fails later as an npm authentication error.\n" || x.file("github_env") != "" {
			t.Errorf("%q: %v %q", empty, err, x.out.String())
		}
	}
}

func TestGuards(t *testing.T) {
	x := newH(t)
	x.chdir()
	// no devbox.json: nothing to say
	_ = GuardGoproxy(x.e())
	_ = GuardAWS(x.e())
	if x.out.String() != "" {
		t.Errorf("%q", x.out.String())
	}
	run := func(content string, f func(Env) error) string {
		x.out.Reset()
		_ = os.WriteFile("devbox.json", []byte(content), 0o644)
		_ = f(x.e())
		return x.out.String()
	}
	if got := run(`{"env":{"GOPROXY":"https://p.example"}}`, GuardGoproxy); got != "::warning file=devbox.json::devbox.json sets env.GOPROXY (https://p.example). devbox run re-applies that env block, so it overrides the CI module proxy passed as the goproxy input for every recipe. Remove GOPROXY from devbox.json; laptops fall back to Go's default proxy.\n" {
		t.Errorf("%q", got)
	}
	for _, clean := range []string{`{"packages":[]}`, `{"env":{"GOFLAGS":"-x"}}`, `{"env":{"GOPROXY":""}}`, `{"env":null}`} {
		if got := run(clean, GuardGoproxy); got != "" {
			t.Errorf("warned on %s: %q", clean, got)
		}
	}
	if got := run("{ // comment\n}", GuardGoproxy); got != "devbox.json is not plain JSON; skipping the GOPROXY check\n" {
		t.Errorf("%q", got)
	}
	for _, key := range []string{"AWS_CONFIG_FILE", "AWS_PROFILE"} {
		pinned := `{"env":{"` + key + `":"x/aws.ini"}}`
		got := run(pinned, GuardAWS)
		want := "::warning file=devbox.json::devbox.json sets env." + key + " (x/aws.ini). devbox run re-applies that env block, so it overrides the CI AWS config and identity (OIDC) and build tooling such as the Go cache plugin loses its credentials. Set it in shell.init_hook with a fallback instead, e.g. export " + key + "=\"${" + key + ":-<default>}\".\n"
		if got != want {
			t.Errorf("%s:\n%q\nwant\n%q", key, got, want)
		}
	}
	for _, clean := range []string{`{"env":{"GOFLAGS":"-x"},"shell":{"init_hook":["export AWS_CONFIG_FILE=\"${AWS_CONFIG_FILE:-a}\""]}}`, `{"packages":[]}`} {
		if got := run(clean, GuardAWS); got != "" {
			t.Errorf("warned on a clean devbox.json: %q", got)
		}
	}
	if got := run("not json", GuardAWS); got != "devbox.json is not plain JSON; skipping the AWS config check\n" {
		t.Errorf("%q", got)
	}
}

func TestRetiredCacheServer(t *testing.T) {
	x := newH(t)
	if err := RetiredCacheServer(x.e()); err != nil || x.out.String() != "::warning::go-cache-server is retired and ignored; the Go build cache goes straight to go-cache-bucket. Remove the input and the CI_GOCACHE_SERVER org variable.\n" || x.file("github_env") != "" {
		t.Errorf("%v %q", err, x.out.String())
	}
}
