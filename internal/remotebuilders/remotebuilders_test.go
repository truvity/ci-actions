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
	o     Options
	fn    func(c runcmd.Cmd) error
}

func newH(t *testing.T, nodes string) *h {
	dir := t.TempDir()
	x := &h{t: t, dir: dir}
	x.o = Options{Nodes: nodes, Home: filepath.Join(dir, "home"), RunnerTemp: dir, GithubEnv: filepath.Join(dir, "env"), Dir: filepath.Join(dir, "ws")}
	_ = os.MkdirAll(x.o.Dir, 0o755)
	x.o.Out = &x.out
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
		if err := Run(context.Background(), x.o); !errors.Is(err, ErrFailed) || x.out.String() != "::error::remote-builders is empty — set the org variable or skip this step\n" || len(x.calls) != 0 {
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
