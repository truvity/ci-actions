package remotebuilders

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truvity/ci-actions/internal/runcmd"
)

type h struct {
	t     *testing.T
	dir   string
	calls []string
	out   strings.Builder
	errs  strings.Builder // diagnostics, among them which source named the builders
	o     Options
	fn    func(c runcmd.Cmd) error
}

func newH(t *testing.T, nodes string) *h {
	dir := t.TempDir()
	x := &h{t: t, dir: dir}
	x.o = Options{Nodes: nodes, Home: filepath.Join(dir, "home"), RunnerTemp: dir, GithubEnv: filepath.Join(dir, "env"), Dir: filepath.Join(dir, "ws")}
	_ = os.MkdirAll(x.o.Dir, 0o755)
	x.o.Out = &x.out
	x.o.Err = &x.errs
	x.o.Exec = func(_ context.Context, c runcmd.Cmd) error {
		x.calls = append(x.calls, c.Name+" "+strings.Join(c.Args, " "))
		if x.fn != nil {
			return x.fn(c)
		}
		return nil
	}
	return x
}

// inspect answers `docker buildx inspect --bootstrap ci` with these platforms.
func inspect(platforms string) func(c runcmd.Cmd) error {
	return func(c runcmd.Cmd) error {
		a := strings.Join(c.Args, " ")
		switch {
		case strings.Contains(a, "command -v docker-buildx"):
			fmt.Fprintln(c.Stdout, "/opt/buildx/docker-buildx")
		case strings.Contains(a, "inspect --bootstrap ci"):
			fmt.Fprintf(c.Stdout, "Name: ci\nNodes:\nPlatforms: %s\n", platforms)
		}
		return nil
	}
}

func TestRegisters(t *testing.T) {
	x := newH(t, "linux/arm64=tcp://a:1234 linux/amd64,linux/386=tcp://b:1234\n")
	x.fn = inspect("linux/arm64, linux/amd64, linux/386")
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err, x.out.String())
	}
	want := []string{
		"sh -c command -v docker-buildx",
		"docker buildx rm ci",
		"docker buildx create --name ci --driver remote tcp://a:1234 --platform linux/arm64",
		"docker buildx create --append --name ci --driver remote tcp://b:1234 --platform linux/amd64,linux/386",
		"docker buildx use ci",
		"docker buildx inspect --bootstrap ci",
	}
	if strings.Join(x.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls:\n%v", x.calls)
	}
	if got := x.out.String(); got != "Name: ci\nNodes:\nPlatforms: linux/arm64, linux/amd64, linux/386\n" {
		t.Errorf("the inspect output is shown: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(x.dir, "buildx-ci.txt")); !strings.Contains(string(b), "linux/386") {
		t.Errorf("and kept: %q", b)
	}
	if b, _ := os.ReadFile(x.o.GithubEnv); string(b) != "BUILDX_BUILDER=ci\n" {
		t.Errorf("env %q", b)
	}
	if l, err := os.Readlink(filepath.Join(x.o.Home, ".docker", "cli-plugins", "docker-buildx")); err != nil || l != "/opt/buildx/docker-buildx" {
		t.Errorf("the plugin is linked: %q %v", l, err)
	}
	// a second run replaces the link (ln -sf)
	x2 := newH(t, "linux/arm64=tcp://a:1")
	x2.o.Home = x.o.Home
	x2.fn = func(c runcmd.Cmd) error {
		if strings.Contains(strings.Join(c.Args, " "), "command -v") {
			fmt.Fprintln(c.Stdout, "/other/docker-buildx")
		}
		if strings.Contains(strings.Join(c.Args, " "), "inspect") {
			fmt.Fprintln(c.Stdout, "linux/arm64")
		}
		return nil
	}
	if err := Run(context.Background(), x2.o); err != nil {
		t.Fatal(err)
	}
	if l, _ := os.Readlink(filepath.Join(x.o.Home, ".docker", "cli-plugins", "docker-buildx")); l != "/other/docker-buildx" {
		t.Errorf("link %q", l)
	}
}

func TestViaDevbox(t *testing.T) {
	x := newH(t, "linux/arm64=tcp://a:1")
	_ = os.WriteFile(filepath.Join(x.o.Dir, "devbox.json"), []byte("{}"), 0o644)
	x.fn = func(c runcmd.Cmd) error {
		if strings.Contains(strings.Join(c.Args, " "), "inspect") {
			fmt.Fprintln(c.Stdout, "linux/arm64")
		}
		return nil
	}
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err)
	}
	if x.calls[0] != "devbox run -- sh -c command -v docker-buildx" || x.calls[1] != "devbox run -- docker buildx rm ci" || x.calls[2] != "devbox run -- docker buildx create --name ci --driver remote tcp://a:1 --platform linux/arm64" {
		t.Errorf("calls %v", x.calls)
	}
	// no plugin found: nothing is linked and that is fine
	if _, err := os.Lstat(filepath.Join(x.o.Home, ".docker", "cli-plugins", "docker-buildx")); err == nil {
		t.Error("no plugin, no link")
	}
}

func TestFailures(t *testing.T) {
	// empty (only spaces are removed by the shell's test: a newline is not empty)
	for _, n := range []string{"", "   "} {
		x := newH(t, n)
		if err := Run(context.Background(), x.o); !errors.Is(err, ErrFailed) || x.out.String() != "::error::remote-builders is empty — set the org variable, export CI_RUNNER_REMOTE_BUILDERS from the runner, or skip this step\n" || len(x.calls) != 0 {
			t.Errorf("%q: %v %q", n, err, x.out.String())
		}
	}
	// a builder that does not offer a platform
	x := newH(t, "linux/arm64=tcp://a:1 linux/amd64=tcp://b:1")
	x.fn = inspect("linux/arm64")
	if err := Run(context.Background(), x.o); !errors.Is(err, ErrFailed) || !strings.HasSuffix(x.out.String(), "::error::builder ci does not offer linux/amd64\n") {
		t.Errorf("%v %q", err, x.out.String())
	}
	if _, err := os.Stat(x.o.GithubEnv); err == nil {
		t.Error("BUILDX_BUILDER is not exported for a builder that is missing a platform")
	}
	// rm is the only tolerated failure; a registration that fails fails the step
	for _, failing := range []string{"create", "use", "inspect"} {
		x := newH(t, "linux/arm64=tcp://a:1")
		x.fn = func(c runcmd.Cmd) error {
			a := strings.Join(c.Args, " ")
			if strings.Contains(a, "buildx "+failing) {
				return errors.New("exit status 3")
			}
			return nil
		}
		err := Run(context.Background(), x.o)
		if err == nil || errors.Is(err, ErrFailed) {
			t.Errorf("%s failing: %v", failing, err)
		}
	}
	x = newH(t, "linux/arm64=tcp://a:1")
	x.fn = func(c runcmd.Cmd) error {
		a := strings.Join(c.Args, " ")
		if strings.Contains(a, "buildx rm") {
			return errors.New("no such builder")
		}
		if strings.Contains(a, "inspect") {
			fmt.Fprintln(c.Stdout, "linux/arm64")
		}
		return nil
	}
	if err := Run(context.Background(), x.o); err != nil {
		t.Errorf("an absent builder is fine: %v", err)
	}
}

func TestSplitEntry(t *testing.T) {
	for in, want := range map[string][2]string{
		"linux/arm64=tcp://h:1": {"linux/arm64", "tcp://h:1"},
		"a,b=tcp://h:1?x=y":     {"a,b", "tcp://h:1?x=y"},
		"bare":                  {"bare", "bare"},
	} {
		if p, e := splitEntry(in); p != want[0] || e != want[1] {
			t.Errorf("%s: %q %q", in, p, e)
		}
	}
}

// The runner's own list wins over the caller's, and the log says which won.
func TestRunnerNodesWin(t *testing.T) {
	x := newH(t, "linux/arm64=tcp://caller:1234")
	x.o.RunnerNodes = "linux/arm64=tcp://runner:1234\n"
	x.fn = inspect("linux/arm64")
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err, x.out.String())
	}
	joined := strings.Join(x.calls, "\n")
	if !strings.Contains(joined, "tcp://runner:1234") || strings.Contains(joined, "caller") {
		t.Errorf("the runner's node must be registered, not the caller's:\n%s", joined)
	}
	if !strings.Contains(x.errs.String(), "(from the runner's CI_RUNNER_REMOTE_BUILDERS)") {
		t.Errorf("the log must name the winning source:\n%s", x.errs.String())
	}
}

// With no runner list nothing changes: the caller's wins and the log says so.
func TestCallerNodesWithoutRunner(t *testing.T) {
	x := newH(t, "linux/arm64=tcp://caller:1234")
	x.fn = inspect("linux/arm64")
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err, x.out.String())
	}
	if !strings.Contains(strings.Join(x.calls, "\n"), "tcp://caller:1234") || !strings.Contains(x.errs.String(), "(from the caller's input or organisation variable)") {
		t.Errorf("calls %v log %q", x.calls, x.errs.String())
	}
}

// The runner may supply the list when the caller passes none.
func TestRunnerNodesWithoutCaller(t *testing.T) {
	x := newH(t, "")
	x.o.RunnerNodes = "linux/arm64=tcp://runner:1234"
	x.fn = inspect("linux/arm64")
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err, x.out.String())
	}
}

// tlsDir is a directory holding the three files a runner provides.
func tlsDir(t *testing.T, files ...string) string {
	dir := t.TempDir()
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// With a client certificate directory every node, the first and the appended
// ones alike, is registered with it; the server name is added when given.
func TestMutualTLS(t *testing.T) {
	dir := tlsDir(t, "ca.pem", "cert.pem", "key.pem")
	x := newH(t, "linux/arm64=tcp://a:443 linux/amd64=tcp://b:443")
	x.o.TLSDir = dir
	x.o.TLSServername = "builders.example"
	x.fn = inspect("linux/arm64, linux/amd64")
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err, x.out.String())
	}
	opt := "--driver-opt cacert=" + dir + "/ca.pem,cert=" + dir + "/cert.pem,key=" + dir + "/key.pem,servername=builders.example"
	want := []string{
		"docker buildx create --name ci --driver remote tcp://a:443 --platform linux/arm64 " + opt,
		"docker buildx create --append --name ci --driver remote tcp://b:443 --platform linux/amd64 " + opt,
	}
	joined := strings.Join(x.calls, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing %q in:\n%s", w, joined)
		}
	}
	if !strings.Contains(x.errs.String(), "mutual TLS") {
		t.Errorf("the log names the transport: %q", x.errs.String())
	}
}

// Without a server name only the three files are passed.
func TestMutualTLSWithoutServername(t *testing.T) {
	dir := tlsDir(t, "ca.pem", "cert.pem", "key.pem")
	x := newH(t, "linux/arm64=tcp://a:443")
	x.o.TLSDir = dir
	x.fn = inspect("linux/arm64")
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err, x.out.String())
	}
	joined := strings.Join(x.calls, "\n")
	if !strings.Contains(joined, "--driver-opt cacert="+dir+"/ca.pem,cert="+dir+"/cert.pem,key="+dir+"/key.pem\n") || strings.Contains(joined, "servername") {
		t.Errorf("calls:\n%s", joined)
	}
}

// Without a directory the registration is exactly what it always was, even
// when a server name is exported.
func TestPlainTCPUnchangedWithoutTLSDir(t *testing.T) {
	x := newH(t, "linux/arm64=tcp://a:1234")
	x.o.TLSServername = "builders.example"
	x.fn = inspect("linux/arm64")
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err, x.out.String())
	}
	joined := strings.Join(x.calls, "\n")
	if strings.Contains(joined, "driver-opt") || !strings.Contains(joined, "docker buildx create --name ci --driver remote tcp://a:1234 --platform linux/arm64\n") {
		t.Errorf("calls:\n%s", joined)
	}
	if !strings.Contains(x.errs.String(), "plain TCP") {
		t.Errorf("the log names the transport: %q", x.errs.String())
	}
}

func TestMutualTLSRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		dir, servername, msg string
	}{
		"relative dir":   {"relative/dir", "", "CI_BUILDKIT_TLS_DIR is not a plain absolute path"},
		"odd characters": {"/tmp/a b", "", "CI_BUILDKIT_TLS_DIR is not a plain absolute path"},
		"missing file":   {tlsDir(t, "ca.pem", "cert.pem"), "", "key.pem is not readable"},
		"bad servername": {tlsDir(t, "ca.pem", "cert.pem", "key.pem"), "a,b=c", "CI_BUILDKIT_TLS_SERVERNAME is not a plain DNS name"},
	} {
		x := newH(t, "linux/arm64=tcp://a:443")
		x.o.TLSDir, x.o.TLSServername = tc.dir, tc.servername
		if err := Run(context.Background(), x.o); !errors.Is(err, ErrFailed) || !strings.Contains(x.out.String(), tc.msg) || len(x.calls) != 0 {
			t.Errorf("%s: %v %q calls %v", name, err, x.out.String(), x.calls)
		}
	}
}

func TestRunnerBuildxCacheWinsAndIsExported(t *testing.T) {
	x := newH(t, "linux/arm64=tcp://a:1")
	x.fn = inspect("linux/arm64")
	x.o.BuildxCache = "registry-a.example.test/ci/buildkit-cache"
	x.o.RunnerBuildxCache = "registry-b.example.test/ci/buildkit-cache\n"
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(x.o.GithubEnv); string(b) != "BUILDX_BUILDER=ci\nCI_BUILDX_CACHE_FROM=registry-b.example.test/ci/buildkit-cache\nCI_BUILDX_CACHE=registry-b.example.test/ci/buildkit-cache\n" {
		t.Errorf("env %q", b)
	}
	if !strings.Contains(x.errs.String(), "buildkit cache: registry-b.example.test/ci/buildkit-cache (from the runner's CI_RUNNER_BUILDX_CACHE)") {
		t.Errorf("log %q", x.errs.String())
	}
}

func TestCallersBuildxCacheIsLeftWhereTheWorkflowPutIt(t *testing.T) {
	x := newH(t, "linux/arm64=tcp://a:1")
	x.fn = inspect("linux/arm64")
	x.o.BuildxCache = "registry-a.example.test/ci/buildkit-cache"
	if err := Run(context.Background(), x.o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(x.o.GithubEnv); string(b) != "BUILDX_BUILDER=ci\nCI_BUILDX_CACHE_FROM=registry-a.example.test/ci/buildkit-cache\n" {
		t.Errorf("env %q", b)
	}
}

func TestReleaseCredentialsRunnerExportsOnlyTheReadReference(t *testing.T) {
	for name, tc := range map[string]struct{ runner, caller, writes, want string }{
		"runner cache, writes need release credentials": {"registry-b.example.test/ci/buildkit-cache", "", "release-credentials",
			"BUILDX_BUILDER=ci\nCI_BUILDX_CACHE_FROM=registry-b.example.test/ci/buildkit-cache\n"},
		"runner cache over the caller's": {"registry-b.example.test/ci/buildkit-cache", "registry-a.example.test/ci/buildkit-cache", " release-credentials\n",
			"BUILDX_BUILDER=ci\nCI_BUILDX_CACHE_FROM=registry-b.example.test/ci/buildkit-cache\n"},
		"an unknown value narrows nothing": {"registry-b.example.test/ci/buildkit-cache", "", "something-else",
			"BUILDX_BUILDER=ci\nCI_BUILDX_CACHE_FROM=registry-b.example.test/ci/buildkit-cache\nCI_BUILDX_CACHE=registry-b.example.test/ci/buildkit-cache\n"},
		"no variable, runner cache": {"registry-b.example.test/ci/buildkit-cache", "", "",
			"BUILDX_BUILDER=ci\nCI_BUILDX_CACHE_FROM=registry-b.example.test/ci/buildkit-cache\nCI_BUILDX_CACHE=registry-b.example.test/ci/buildkit-cache\n"},
		"the caller's cache only": {"", "registry-a.example.test/ci/buildkit-cache", "release-credentials",
			"BUILDX_BUILDER=ci\nCI_BUILDX_CACHE_FROM=registry-a.example.test/ci/buildkit-cache\n"},
		"no cache at all": {"", "", "release-credentials", "BUILDX_BUILDER=ci\n"},
	} {
		x := newH(t, "linux/arm64=tcp://a:1")
		x.fn = inspect("linux/arm64")
		x.o.RunnerBuildxCache, x.o.BuildxCache, x.o.RunnerCacheWrites = tc.runner, tc.caller, tc.writes
		if err := Run(context.Background(), x.o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b, _ := os.ReadFile(x.o.GithubEnv); string(b) != tc.want {
			t.Errorf("%s: env %q, want %q", name, b, tc.want)
		}
	}
}

func TestMalformedRunnerBuildxCacheIsRefusedBeforeAnythingRuns(t *testing.T) {
	for _, v := range []string{"has space/x", "no-slash", "reg.example/repo:tag", "reg.example/repo\nX=1", "https://reg.example/repo"} {
		x := newH(t, "linux/arm64=tcp://a:1")
		x.o.RunnerBuildxCache = v
		if err := Run(context.Background(), x.o); !errors.Is(err, ErrFailed) || len(x.calls) != 0 {
			t.Errorf("%q: err %v calls %v", v, err, x.calls)
		}
	}
}
