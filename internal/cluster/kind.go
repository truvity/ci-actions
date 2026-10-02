package cluster

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func osExec(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	return cmd.Run()
}

// KindLaunch (mode: kind) fetches truvity/policy's hack/kind/ box AT A
// PINNED RELEASE, runs it (up.sh already runs verify.sh at its own last
// step), and exports the five things every caller reads regardless of tier.
//
// The box is the ONE owner of what a kind lane provides, registry included:
// up.sh already stands up `kind-registry` on `localhost:${REGISTRY_PORT}`,
// wires it into containerd on every node, and verify.sh already proves a push
// and a pull through it. This does not create a second registry, or any other
// infrastructure the box did not ask for; it only asserts the box's own claim
// (see runBox) and reports SNAPSHOT_REGISTRY as whatever `localhost:5001`
// means for the pinned version.
//
// The box is fetched from a release TARBALL rather than vendored here or
// published as a release asset of its own: a tag's tarball is a stable
// artifact GitHub serves for any public repository with no token, and
// "download the archive, keep hack/kind/" needs no new publishing step.
func KindLaunch(ctx context.Context, o Options) error {
	o.defaults()
	if err := o.required("GITHUB_ENV", "GITHUB_OUTPUT"); err != nil {
		return err
	}
	state, err := o.stateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	namespace := orDefault(o.Getenv("NAMESPACE"), "e2e")
	release := o.release()
	version := o.Getenv("POLICY_VERSION")
	if version == "" {
		return o.fail("policy-version is required for mode: kind")
	}

	box := filepath.Join(state, "policy-kind-box")
	if st, err := os.Stat(filepath.Join(box, "up.sh")); err != nil || st.Mode()&0o111 == 0 {
		o.printf("fetching truvity/policy@%s's hack/kind/ box\n", version)
		if err := o.fetchBox(ctx, version, box); err != nil {
			fmt.Fprintln(o.Err, err)
			return &ExitError{Code: 1}
		}
	}

	// Never the default ~/.kube/config: a caller's own job may run other
	// kubectl/helm commands against a different cluster (mode: shared,
	// earlier or later in the same workflow, or its own tooling), and a box
	// that quietly repoints the default context would step on that.
	kubeconfig := filepath.Join(state, "kubeconfig")

	// localhost:5001 is the box's own convention (hack/kind/versions.env:
	// REGISTRY_PORT), not a port this action picked; runBox fails loudly,
	// naming the pinned version, if a release's box does not publish one there.
	outputs := filepath.Join(state, "outputs.env")
	content := "KUBECONFIG=" + kubeconfig + "\nSNAPSHOT_REGISTRY=localhost:5001\nGEMAAL_TIER=kind\nGEMAAL_NAMESPACE=" + namespace + "\nGEMAAL_RELEASE=" + release + "\n"
	if err := os.WriteFile(outputs, []byte(content), 0o644); err != nil {
		return err
	}
	// Emitted immediately, background or not: every value above is a path or
	// a fixed string, known before the cluster exists. What background: true
	// defers is the cluster being USABLE, not what it will be called.
	if err := o.emitFromFile(outputs); err != nil {
		return err
	}

	logPath := filepath.Join(state, "log")
	pidFile := filepath.Join(state, "pid")
	exitFile := filepath.Join(state, "exit-code")
	_ = os.Remove(exitFile)

	if o.Getenv("BACKGROUND") == "true" {
		// A new session and full redirection: the box must keep running after
		// THIS step's process exits, which is the whole point of background:
		// true.
		lf, err := os.Create(logPath)
		if err != nil {
			return err
		}
		defer lf.Close()
		spawn := o.Spawn
		if spawn == nil {
			spawn = spawnSelf
		}
		pid, err := spawn(ctx, []string{"cluster", "box-run", "--state-dir", state, "--box", box, "--kubeconfig", kubeconfig, "--policy-version", version}, lf)
		if err != nil {
			return err
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
			return err
		}
		o.printf("kind box launching in the background — log at %s\n", logPath)
		o.printf("call this action again with mode: wait (same state-dir) before using the cluster\n")
		return nil
	}

	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	code := o.runBox(ctx, box, kubeconfig, version, io.MultiWriter(o.Out, lf))
	if err := os.WriteFile(exitFile, []byte(strconv.Itoa(code)+"\n"), 0o644); err != nil {
		return err
	}
	if code != 0 {
		o.printf("::error::the kind box failed to come up (exit %d) — see the log above\n", code)
		return &ExitError{Code: code}
	}
	o.printf("kind box ready — kubeconfig at %s\n", kubeconfig)
	return nil
}

// BoxRun is what a background launch runs, detached: the box itself, then
// the exit-code file a later wait reads. Its stdout and stderr are the log.
func BoxRun(ctx context.Context, o Options, stateDir, box, kubeconfig, version string) int {
	o.defaults()
	code := o.runBox(ctx, box, kubeconfig, version, o.Out)
	_ = os.WriteFile(filepath.Join(stateDir, "exit-code"), []byte(strconv.Itoa(code)+"\n"), 0o644)
	return code
}

var portRe = regexp.MustCompile(`:5001->`)

// runBox runs up.sh, then CHECKS the claim SNAPSHOT_REGISTRY=localhost:5001
// makes about the PINNED VERSION's own box, rather than assuming it: whether
// a kind lane gets a registry, and on which port, is entirely up.sh's
// decision, and this must not silently paper over a future release that
// changes it. A version that does not provide one on 5001 fails loudly,
// naming itself, instead of a caller finding an unreachable registry three
// steps later. The exit status is the first failing command's.
func (o Options) runBox(ctx context.Context, box, kubeconfig, version string, w io.Writer) int {
	if err := o.Exec(ctx, Cmd{Dir: box, Env: []string{"KUBECONFIG=" + kubeconfig}, Name: "./up.sh", Stdout: w, Stderr: w}); err != nil {
		return exitCode(err)
	}
	var ports strings.Builder
	// A docker that cannot answer is "no registry published" too: the shell
	// version's `if ! docker ps | grep -q` read a failed pipeline the same way.
	if err := o.Exec(ctx, Cmd{Name: "docker", Args: []string{"ps", "--format", "{{.Ports}}"}, Stdout: &ports, Stderr: w}); err != nil || !portRe.MatchString(ports.String()) {
		fmt.Fprintf(w, "::error::truvity/policy@%s's hack/kind/ box did not publish a registry on localhost:5001 — SNAPSHOT_REGISTRY cannot be honoured for this policy-version. Pin a release whose hack/kind/versions.env sets REGISTRY_PORT=5001 (the box is the one owner of what a kind lane provides; this action does not stand up its own).\n", version)
		return 1
	}
	return 0
}

func exitCode(err error) int {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 1
}

// fetchBox downloads the release tarball and keeps hack/kind/ only: this
// never sees the rest of policy's checkout, so it cannot start depending on
// anything outside hack/kind/ without the extraction itself failing to find
// it. `tar xzf --strip-components=3 policy-<version without v>/hack/kind`.
func (o Options) fetchBox(ctx context.Context, version, box string) error {
	tmp, err := os.MkdirTemp("", "cluster-box-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "policy.tgz")
	url := "https://github.com/truvity/policy/archive/refs/tags/" + version + ".tar.gz"
	fetch := o.Fetch
	if fetch == nil {
		fetch = download
	}
	if err := fetch(ctx, url, archive); err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	if err := os.MkdirAll(box, 0o755); err != nil {
		return err
	}
	n, err := untarSubtree(archive, "policy-"+strings.TrimPrefix(version, "v")+"/hack/kind", box)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("policy-%s/hack/kind: not found in archive", strings.TrimPrefix(version, "v"))
	}
	scripts, _ := filepath.Glob(filepath.Join(box, "*.sh"))
	for _, s := range scripts {
		if err := os.Chmod(s, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func download(ctx context.Context, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// untarSubtree extracts the entries under prefix into dest, stripping the
// prefix (three leading components for hack/kind), and returns how many files
// it wrote. Entries that would land outside dest are an error.
func untarSubtree(archive, prefix, dest string) (int, error) {
	f, err := os.Open(archive)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name != prefix && !strings.HasPrefix(name, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(name, prefix), "/")
		target := filepath.Join(dest, rel)
		if target != filepath.Clean(dest) && !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return n, fmt.Errorf("unsafe path %q in archive", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return n, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return n, err
			}
			if err := out.Close(); err != nil {
				return n, err
			}
			n++
		}
	}
}

// spawnSelf re-executes this binary detached, in its own session, with stdin
// closed and stdout and stderr on the log.
func spawnSelf(ctx context.Context, args []string, log *os.File) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// KindWait (mode: wait) blocks on a `mode: kind, background: true` launch
// started earlier in the SAME job, then re-emits the same five outputs.
//
// A background launch and its wait are two separate invocations of this
// action: two separate steps, two separate processes, neither with any memory
// of the other's inputs. What connects them is state-dir: the launch writes
// its pid, its log and the resolved outputs under it, and wait takes no
// inputs of its own beyond that.
func KindWait(o Options) error {
	o.defaults()
	if err := o.required("GITHUB_ENV", "GITHUB_OUTPUT"); err != nil {
		return err
	}
	state, err := o.stateDir()
	if err != nil {
		return err
	}
	pidFile := filepath.Join(state, "pid")
	exitFile := filepath.Join(state, "exit-code")
	logPath := filepath.Join(state, "log")
	outputs := filepath.Join(state, "outputs.env")

	b, err := os.ReadFile(pidFile)
	if err != nil {
		return o.fail("no background kind launch found under %s — call this action with mode: kind and background: true first (same state-dir input, if you set one)", state)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	alive := o.Alive
	if alive == nil {
		alive = pidAlive
	}

	// 20 minutes: the box itself measures 3-4 on a hosted runner with
	// nothing cached, so this is generous headroom rather than a number tuned
	// to the happy path.
	start := o.Now()
	for alive(pid) {
		if o.Now().Sub(start) >= 20*time.Minute {
			o.printf("::error::kind box did not finish within 20 minutes (pid %d still running) — log:\n", pid)
			o.tail(logPath)
			return ErrReported
		}
		o.Sleep(5 * time.Second)
	}

	// The process is gone; give the exit-code file an instant to land (it is
	// written by the same background wrapper right after the process exits,
	// so there is a small window rather than a guarantee of simultaneity).
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(exitFile); err == nil {
			break
		}
		o.Sleep(time.Second)
	}
	code := "unknown"
	if b, err := os.ReadFile(exitFile); err == nil {
		code = strings.TrimRight(string(b), "\n")
	}
	if code != "0" {
		o.printf("::error::the kind box failed to come up (exit %s) — log:\n", code)
		o.tail(logPath)
		return ErrReported
	}
	if err := o.emitFromFile(outputs); err != nil {
		return err
	}
	o.printf("kind box ready\n")
	return nil
}

// tail is `tail -n 200`, silent when the log is not there.
func (o Options) tail(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.SplitAfter(string(b), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	io.WriteString(o.Out, strings.Join(lines, ""))
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
