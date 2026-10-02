package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t     *testing.T
	dir   string
	env   map[string]string
	out   bytes.Buffer
	err   bytes.Buffer
	calls []string
	sleep []time.Duration
	exec  func(c Cmd) error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, env: map[string]string{
		"GITHUB_ENV": filepath.Join(dir, "github_env"), "GITHUB_OUTPUT": filepath.Join(dir, "github_output"),
		"RUNNER_TEMP": filepath.Join(dir, "temp"), "GITHUB_WORKSPACE": filepath.Join(dir, "ws"),
	}}
	for _, d := range []string{"temp", "ws"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	return h
}

func (h *harness) opts() Options {
	return Options{
		Getenv: func(k string) string { return h.env[k] },
		Out:    &h.out, Err: &h.err,
		Exec: func(_ context.Context, c Cmd) error {
			h.calls = append(h.calls, strings.TrimSpace(c.Name+" "+strings.Join(c.Args, " ")))
			if h.exec != nil {
				return h.exec(c)
			}
			return nil
		},
		Sleep: func(d time.Duration) { h.sleep = append(h.sleep, d) },
	}
}

func (h *harness) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.dir, name))
	return string(b)
}

func (h *harness) write(rel, content string) string {
	p := filepath.Join(h.dir, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func code(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestForkGuard(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		missing       bool
		code          int
		out           string
	}{
		{"a fork pull request is refused", `{"pull_request":{"head":{"repo":{"fork":true}}}}`, false, 1, "::error::mode: shared refuses a fork pull request — a fork's code must never receive this repository's cluster credentials, ECR login or kubeconfig. Use mode: kind for pull requests, and mode: shared only for pushes/merges this repository trusts.\n"},
		{"the string true is a fork too (jq -r prints it as true)", `{"pull_request":{"head":{"repo":{"fork":"true"}}}}`, false, 1, ""},
		{"a same-repository pull request proceeds", `{"pull_request":{"head":{"repo":{"fork":false}}}}`, false, 0, "not a fork pull request — mode: shared may proceed\n"},
		{"no fork field is not a fork", `{"pull_request":{"head":{"repo":{}}}}`, false, 0, "not a fork pull request — mode: shared may proceed\n"},
		{"a null fork field is not a fork", `{"pull_request":{"head":{"repo":{"fork":null}}}}`, false, 0, "not a fork pull request — mode: shared may proceed\n"},
		{"a push carries no pull_request", `{"ref":"refs/heads/master","commits":[]}`, false, 0, "not a fork pull request — mode: shared may proceed\n"},
		{"a missing payload proceeds rather than crashing", ``, true, 0, "no event payload — not a pull request, mode: shared may proceed\n"},
		{"a payload that is not JSON is refused: the one guard that fails closed", `<<<`, false, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if !tc.missing {
				h.env["GITHUB_EVENT_PATH"] = h.write("event.json", tc.payload)
			} else {
				h.env["GITHUB_EVENT_PATH"] = filepath.Join(h.dir, "does-not-exist.json")
			}
			err := ForkGuard(h.opts())
			if code(err) != tc.code || (tc.out != "" && h.out.String() != tc.out) {
				t.Errorf("code %d out %q; want %d %q", code(err), h.out.String(), tc.code, tc.out)
			}
		})
	}
	h := newHarness(t) // GITHUB_EVENT_PATH unset altogether
	if err := ForkGuard(h.opts()); err != nil || h.out.String() != "no event payload — not a pull request, mode: shared may proceed\n" {
		t.Errorf("%v %q", err, h.out.String())
	}
}

func (h *harness) outputsOf() string { return h.file("github_output") }
func (h *harness) envOf() string     { return h.file("github_env") }

const fiveOutputs = "kubeconfig=%s\nsnapshot-registry=localhost:5001\ngemaal-tier=kind\ngemaal-namespace=%s\ngemaal-release=%s\n"

func upOK(ports string) func(c Cmd) error {
	return func(c Cmd) error {
		switch c.Name {
		case "./up.sh":
			fmt.Fprintln(c.Stdout, "up: cluster created")
		case "docker":
			fmt.Fprint(c.Stdout, ports)
		}
		return nil
	}
}

func TestKindForeground(t *testing.T) {
	h := newHarness(t)
	h.env["POLICY_VERSION"] = "v0.8.1"
	h.env["NAMESPACE"] = "e2e-test"
	h.env["GITHUB_JOB"], h.env["GITHUB_RUN_ID"], h.env["GITHUB_RUN_ATTEMPT"] = "integration", "77", "2"
	state := filepath.Join(h.env["RUNNER_TEMP"], "cluster-action")
	// the box is already there: nothing is fetched
	h.write("temp/cluster-action/policy-kind-box/up.sh", "#!/bin/sh\n")
	_ = os.Chmod(filepath.Join(state, "policy-kind-box", "up.sh"), 0o755)
	h.exec = upOK("0.0.0.0:5001->5000/tcp\n")
	o := h.opts()
	o.Fetch = func(context.Context, string, string) error {
		t.Error("the box is cached, nothing is fetched")
		return nil
	}

	if err := KindLaunch(context.Background(), o); err != nil {
		t.Fatal(err, h.out.String())
	}
	kc := filepath.Join(state, "kubeconfig")
	wantOut := "up: cluster created\nkind box ready — kubeconfig at " + kc + "\n"
	if h.out.String() != wantOut {
		t.Errorf("out %q, want %q", h.out.String(), wantOut)
	}
	if got := h.outputsOf(); got != fmt.Sprintf(fiveOutputs, kc, "e2e-test", "integration-r77-a2") {
		t.Errorf("outputs %q", got)
	}
	if got := h.envOf(); got != "KUBECONFIG="+kc+"\nSNAPSHOT_REGISTRY=localhost:5001\nGEMAAL_TIER=kind\nGEMAAL_NAMESPACE=e2e-test\nGEMAAL_RELEASE=integration-r77-a2\n" {
		t.Errorf("env %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "log")); string(b) != "up: cluster created\n" {
		t.Errorf("log %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "exit-code")); string(b) != "0\n" {
		t.Errorf("exit-code %q", b)
	}
	if strings.Join(h.calls, "|") != "./up.sh|docker ps --format {{.Ports}}" {
		t.Errorf("calls %v", h.calls)
	}
}

func TestKindDefaultsAndFailures(t *testing.T) {
	// defaults: namespace e2e, release <job>-r<run>-a<attempt> with their own defaults
	h := newHarness(t)
	h.env["POLICY_VERSION"] = "v1"
	h.write("temp/cluster-action/policy-kind-box/up.sh", "")
	_ = os.Chmod(filepath.Join(h.env["RUNNER_TEMP"], "cluster-action", "policy-kind-box", "up.sh"), 0o755)
	h.exec = upOK("0.0.0.0:5001->5000/tcp\n")
	if err := KindLaunch(context.Background(), h.opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.outputsOf(), "gemaal-namespace=e2e\ngemaal-release=e2e-r0-a1\n") {
		t.Errorf("%q", h.outputsOf())
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		exec func(c Cmd) error
		code int
		out  string
	}{
		{"policy-version is required", map[string]string{"POLICY_VERSION": ""}, nil, 1, "::error::policy-version is required for mode: kind\n"},
		{"the box failing exits with the box's code", nil, func(c Cmd) error {
			if c.Name == "./up.sh" {
				fmt.Fprintln(c.Stdout, "up: boom")
				return &exitErr{3}
			}
			return nil
		}, 3, "up: boom\n::error::the kind box failed to come up (exit 3) — see the log above\n"},
		{"a registry on another port fails loudly, naming the version", nil, upOK("0.0.0.0:5002->5000/tcp\n"), 1,
			"up: cluster created\n::error::truvity/policy@v0.8.1's hack/kind/ box did not publish a registry on localhost:5001 — SNAPSHOT_REGISTRY cannot be honoured for this policy-version. Pin a release whose hack/kind/versions.env sets REGISTRY_PORT=5001 (the box is the one owner of what a kind lane provides; this action does not stand up its own).\n::error::the kind box failed to come up (exit 1) — see the log above\n"},
		{"a docker that cannot answer reads as no registry", nil, func(c Cmd) error {
			if c.Name == "docker" {
				return &exitErr{1}
			}
			return nil
		}, 1, "::error::truvity/policy@v0.8.1's hack/kind/ box did not publish a registry on localhost:5001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.env["POLICY_VERSION"] = "v0.8.1"
			for k, v := range tc.env {
				h.env[k] = v
			}
			h.write("temp/cluster-action/policy-kind-box/up.sh", "")
			_ = os.Chmod(filepath.Join(h.env["RUNNER_TEMP"], "cluster-action", "policy-kind-box", "up.sh"), 0o755)
			h.exec = tc.exec
			err := KindLaunch(context.Background(), h.opts())
			if code(err) != tc.code || !strings.Contains(h.out.String(), tc.out) {
				t.Errorf("code %d out %q, want %d containing %q", code(err), h.out.String(), tc.code, tc.out)
			}
		})
	}
	for _, name := range []string{"GITHUB_ENV", "GITHUB_OUTPUT"} {
		h := newHarness(t)
		h.env["POLICY_VERSION"] = "v1"
		h.env[name] = ""
		if err := KindLaunch(context.Background(), h.opts()); code(err) != 1 || !strings.Contains(h.err.String(), name) {
			t.Errorf("%s unset: %v %q", name, err, h.err.String())
		}
	}
	h = newHarness(t)
	h.env["RUNNER_TEMP"] = ""
	h.env["POLICY_VERSION"] = "v1"
	if err := KindLaunch(context.Background(), h.opts()); code(err) != 1 || !strings.Contains(h.err.String(), "RUNNER_TEMP is not set") {
		t.Errorf("%v %q", err, h.err.String())
	}
}

type exitErr struct{ code int }

func (e *exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *exitErr) ExitCode() int { return e.code }

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if strings.HasSuffix(name, "/") {
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
			continue
		}
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func TestKindFetchesTheBox(t *testing.T) {
	archive := tarball(t, map[string]string{
		"policy-0.8.1/":                    "",
		"policy-0.8.1/README.md":           "not the box",
		"policy-0.8.1/hack/kind/":          "",
		"policy-0.8.1/hack/kind/up.sh":     "#!/bin/sh\necho up\n",
		"policy-0.8.1/hack/kind/verify.sh": "#!/bin/sh\n",
		"policy-0.8.1/hack/kind/lib/x.env": "A=1\n",
		"policy-0.8.1/hack/other/y":        "no",
		"policy-0.8.10/hack/kind/up.sh":    "another version, not ours",
	})
	h := newHarness(t)
	h.env["POLICY_VERSION"] = "v0.8.1"
	h.exec = upOK("0.0.0.0:5001->5000/tcp\n")
	o := h.opts()
	var url string
	o.Fetch = func(_ context.Context, u, dest string) error {
		url = u
		return os.WriteFile(dest, archive, 0o644)
	}
	if err := KindLaunch(context.Background(), o); err != nil {
		t.Fatal(err, h.out.String(), h.err.String())
	}
	if url != "https://github.com/truvity/policy/archive/refs/tags/v0.8.1.tar.gz" {
		t.Errorf("url %q", url)
	}
	if !strings.HasPrefix(h.out.String(), "fetching truvity/policy@v0.8.1's hack/kind/ box\n") {
		t.Errorf("out %q", h.out.String())
	}
	box := filepath.Join(h.env["RUNNER_TEMP"], "cluster-action", "policy-kind-box")
	for _, f := range []string{"up.sh", "verify.sh", "lib/x.env"} {
		if _, err := os.Stat(filepath.Join(box, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, f := range []string{"README.md", "y", "hack"} {
		if _, err := os.Stat(filepath.Join(box, f)); err == nil {
			t.Errorf("%s must not be extracted", f)
		}
	}
	if st, _ := os.Stat(filepath.Join(box, "up.sh")); st.Mode()&0o111 == 0 {
		t.Error("the box's scripts are executable")
	}
	// an archive without the subtree, and a failed download, fail the step
	for name, fetch := range map[string]func(context.Context, string, string) error{
		"no hack/kind": func(_ context.Context, _, dest string) error {
			return os.WriteFile(dest, tarball(t, map[string]string{"policy-0.8.1/README.md": "x"}), 0o644)
		},
		"download fails": func(context.Context, string, string) error { return errors.New("HTTP 404") },
	} {
		h := newHarness(t)
		h.env["POLICY_VERSION"] = "v0.8.1"
		o := h.opts()
		o.Fetch = fetch
		if err := KindLaunch(context.Background(), o); code(err) != 1 || h.err.String() == "" {
			t.Errorf("%s: %v %q", name, err, h.err.String())
		}
	}
}

func TestKindBackgroundAndWait(t *testing.T) {
	h := newHarness(t)
	h.env["POLICY_VERSION"] = "v0.8.1"
	state := filepath.Join(h.env["RUNNER_TEMP"], "cluster-action")
	h.write("temp/cluster-action/policy-kind-box/up.sh", "")
	_ = os.Chmod(filepath.Join(state, "policy-kind-box", "up.sh"), 0o755)
	h.write("temp/cluster-action/exit-code", "stale\n")
	h.env["BACKGROUND"] = "true"
	o := h.opts()
	var spawned []string
	o.Spawn = func(_ context.Context, args []string, _ *os.File) (int, error) { spawned = args; return 4242, nil }
	if err := KindLaunch(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if strings.Join(spawned, " ") != "cluster box-run --state-dir "+state+" --box "+filepath.Join(state, "policy-kind-box")+" --kubeconfig "+filepath.Join(state, "kubeconfig")+" --policy-version v0.8.1" {
		t.Errorf("spawned %v", spawned)
	}
	want := "kind box launching in the background — log at " + filepath.Join(state, "log") + "\ncall this action again with mode: wait (same state-dir) before using the cluster\n"
	if h.out.String() != want {
		t.Errorf("out %q", h.out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(state, "pid")); string(b) != "4242\n" {
		t.Errorf("pid %q", b)
	}
	if _, err := os.Stat(filepath.Join(state, "exit-code")); err == nil {
		t.Error("a stale exit-code is removed")
	}
	// the outputs are known before the cluster exists
	if !strings.Contains(h.outputsOf(), "gemaal-tier=kind\n") {
		t.Errorf("%q", h.outputsOf())
	}

	// wait: the process is gone and left a good exit code
	w := newHarness(t)
	w.env["RUNNER_TEMP"] = h.env["RUNNER_TEMP"]
	w.env["GITHUB_ENV"], w.env["GITHUB_OUTPUT"] = filepath.Join(w.dir, "e"), filepath.Join(w.dir, "o")
	wo := w.opts()
	polls := 0
	wo.Alive = func(pid int) bool {
		if pid != 4242 {
			t.Errorf("pid %d", pid)
		}
		polls++
		return polls <= 3
	}
	_ = os.WriteFile(filepath.Join(state, "exit-code"), []byte("0\n"), 0o644)
	if err := KindWait(wo); err != nil {
		t.Fatal(err, w.out.String())
	}
	if w.out.String() != "kind box ready\n" || len(w.sleep) != 3 || w.sleep[0] != 5*time.Second {
		t.Errorf("out %q sleeps %v", w.out.String(), w.sleep)
	}
	if got := w.file("o"); !strings.Contains(got, "kubeconfig="+filepath.Join(state, "kubeconfig")+"\n") || !strings.Contains(got, "gemaal-release=e2e-r0-a1\n") {
		t.Errorf("outputs %q", got)
	}

	// a failed box: the last 200 lines of its log, and the failure
	var log strings.Builder
	for i := 1; i <= 250; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	_ = os.WriteFile(filepath.Join(state, "log"), []byte(log.String()), 0o644)
	_ = os.WriteFile(filepath.Join(state, "exit-code"), []byte("7\n"), 0o644)
	f := newHarness(t)
	f.env["RUNNER_TEMP"] = h.env["RUNNER_TEMP"]
	fo := f.opts()
	fo.Alive = func(int) bool { return false }
	if err := KindWait(fo); code(err) != 1 {
		t.Errorf("%v", err)
	}
	out := f.out.String()
	if !strings.HasPrefix(out, "::error::the kind box failed to come up (exit 7) — log:\nline 51\n") || !strings.HasSuffix(out, "line 250\n") || strings.Contains(out, "line 50\n") {
		t.Errorf("out head %q", out[:min(len(out), 120)])
	}

	// no exit-code file lands: unknown, after the grace period
	_ = os.Remove(filepath.Join(state, "exit-code"))
	g := newHarness(t)
	g.env["RUNNER_TEMP"] = h.env["RUNNER_TEMP"]
	go_ := g.opts()
	go_.Alive = func(int) bool { return false }
	if err := KindWait(go_); code(err) != 1 || !strings.Contains(g.out.String(), "(exit unknown)") || len(g.sleep) != 5 {
		t.Errorf("%v %q %v", err, g.out.String(), g.sleep)
	}

	// still running after 20 minutes
	now := time.Unix(0, 0)
	s := newHarness(t)
	s.env["RUNNER_TEMP"] = h.env["RUNNER_TEMP"]
	so := s.opts()
	so.Alive = func(int) bool { return true }
	so.Now = func() time.Time { return now }
	so.Sleep = func(d time.Duration) { now = now.Add(d) }
	if err := KindWait(so); code(err) != 1 || !strings.HasPrefix(s.out.String(), "::error::kind box did not finish within 20 minutes (pid 4242 still running) — log:\n") {
		t.Errorf("%v %q", err, s.out.String()[:min(len(s.out.String()), 100)])
	}

	// nothing was launched
	n := newHarness(t)
	if err := KindWait(n.opts()); code(err) != 1 || !strings.HasPrefix(n.out.String(), "::error::no background kind launch found under "+filepath.Join(n.env["RUNNER_TEMP"], "cluster-action")+" — call this action with mode: kind and background: true first (same state-dir input, if you set one)\n") {
		t.Errorf("%v %q", err, n.out.String())
	}
}

func TestBoxRunWritesTheExitCode(t *testing.T) {
	h := newHarness(t)
	h.exec = func(c Cmd) error {
		if c.Name == "./up.sh" {
			return &exitErr{9}
		}
		return nil
	}
	state := h.dir
	if got := BoxRun(context.Background(), h.opts(), state, filepath.Join(h.dir, "box"), "kc", "v1"); got != 9 {
		t.Errorf("code %d", got)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "exit-code")); string(b) != "9\n" {
		t.Errorf("%q", b)
	}
	if h.exec = upOK("0.0.0.0:5001->5000/tcp\n"); BoxRun(context.Background(), h.opts(), state, "box", "kc", "v1") != 0 {
		t.Error("a good box exits 0")
	}
	if b, _ := os.ReadFile(filepath.Join(state, "exit-code")); string(b) != "0\n" {
		t.Errorf("%q", b)
	}
}

func TestSharedConnect(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.write("ws/.kube/config", "apiVersion: v1\n")
		h.write("ws/aws.ini", "[default]\n")
		h.env["KUBECONFIG_INPUT"], h.env["AWS_CONFIG_FILE_INPUT"] = ".kube/config", "aws.ini"
		return h
	}
	whoamiJSON := func(user string) string {
		return fmt.Sprintf("{\n  \"kind\": \"SelfSubjectReview\",\n  \"status\": {\"userInfo\": {\"username\": %q}}\n}\n", user)
	}
	for _, tc := range []struct {
		name string
		mut  func(h *harness)
		exec func(h *harness, attempt int, c Cmd) error
		code int
		out  string
	}{
		{"kubeconfig is required", func(h *harness) { h.env["KUBECONFIG_INPUT"] = "" }, nil, 1, "::error::kubeconfig is required for mode: shared\n"},
		{"aws-config-file is required", func(h *harness) { h.env["AWS_CONFIG_FILE_INPUT"] = "" }, nil, 1, "::error::aws-config-file is required for mode: shared\n"},
		{"kubeconfig must exist", func(h *harness) { h.env["KUBECONFIG_INPUT"] = "nope" }, nil, 1, ""},
		{"aws config must exist", func(h *harness) { h.env["AWS_CONFIG_FILE_INPUT"] = "nope" }, nil, 1, ""},
		{"no expected identity skips the proof", nil, nil, 0, "expected-identity is empty — skipping the identity proof\n"},
		{"the identity matches", func(h *harness) { h.env["EXPECTED_IDENTITY"] = "github:acme/app" }, func(h *harness, _ int, c Cmd) error {
			// a banner before the JSON must not matter
			fmt.Fprint(c.Stdout, "Info: banner\n"+whoamiJSON("github:acme/app"))
			return nil
		}, 0, "cluster identity: github:acme/app\n"},
		{"the identity differs", func(h *harness) { h.env["EXPECTED_IDENTITY"] = "github:acme/app" }, func(h *harness, _ int, c Cmd) error {
			fmt.Fprint(c.Stdout, whoamiJSON("github:acme/other"))
			return nil
		}, 1, "cluster identity: github:acme/other\n::error::expected cluster identity github:acme/app, got github:acme/other\n"},
		{"no username reads null", func(h *harness) { h.env["EXPECTED_IDENTITY"] = "x" }, func(h *harness, _ int, c Cmd) error {
			fmt.Fprint(c.Stdout, `{"status":{}}`)
			return nil
		}, 1, "cluster identity: null\n::error::expected cluster identity x, got null\n"},
		{"it works on the second try", func(h *harness) { h.env["EXPECTED_IDENTITY"] = "u" }, func(h *harness, attempt int, c Cmd) error {
			if attempt == 1 {
				fmt.Fprint(c.Stderr, "Unable to connect\n")
				return errors.New("exit 1")
			}
			fmt.Fprint(c.Stdout, whoamiJSON("u"))
			return nil
		}, 0, "attempt 1:\nUnable to connect\ncluster identity: u\n"},
		{"three failures", func(h *harness) { h.env["EXPECTED_IDENTITY"] = "u" }, func(h *harness, attempt int, c Cmd) error {
			fmt.Fprintf(c.Stderr, "refused %d\n", attempt)
			return errors.New("exit 1")
		}, 1, "attempt 1:\nrefused 1\nattempt 2:\nrefused 2\nattempt 3:\nrefused 3\n::error::kubectl auth whoami failed three times\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			if tc.mut != nil {
				tc.mut(h)
			}
			attempt := 0
			var envs [][]string
			h.exec = func(c Cmd) error {
				attempt++
				envs = append(envs, c.Env)
				if tc.exec == nil {
					return nil
				}
				return tc.exec(h, attempt, c)
			}
			err := SharedConnect(context.Background(), h.opts())
			if code(err) != tc.code || (tc.out != "" && h.out.String() != tc.out) {
				t.Errorf("code %d out %q, want %d %q", code(err), h.out.String(), tc.code, tc.out)
			}
			if tc.name == "three failures" && (len(h.sleep) != 2 || h.sleep[0] != 10*time.Second) {
				t.Errorf("sleeps %v", h.sleep)
			}
			if tc.code == 0 || tc.name == "the identity differs" {
				kc := filepath.Join(h.env["GITHUB_WORKSPACE"], ".kube/config")
				if got := h.envOf(); got != "KUBECONFIG="+kc+"\nAWS_CONFIG_FILE="+filepath.Join(h.env["GITHUB_WORKSPACE"], "aws.ini")+"\n" {
					t.Errorf("GITHUB_ENV %q", got)
				}
				for _, e := range envs {
					if len(e) != 2 || e[0] != "KUBECONFIG="+kc {
						t.Errorf("kubectl env %v", e)
					}
				}
			}
		})
	}
	h := newHarness(t)
	h.env["GITHUB_WORKSPACE"] = ""
	if err := SharedConnect(context.Background(), h.opts()); code(err) != 1 || !strings.Contains(h.err.String(), "GITHUB_WORKSPACE") {
		t.Errorf("%v %q", err, h.err.String())
	}
}

func TestSharedFinish(t *testing.T) {
	for _, tc := range []struct{ name, ecr, ns, release, want string }{
		{"two registries: the first wins", "111.dkr.ecr.eu.amazonaws.com,222.dkr.ecr.eu.amazonaws.com", "ns1", "rel1", "snapshot-registry=111.dkr.ecr.eu.amazonaws.com\n"},
		{"no login leaves it empty", "", "", "", "snapshot-registry=\n"},
	} {
		h := newHarness(t)
		h.env["KUBECONFIG"], h.env["ECR_REGISTRY"], h.env["NAMESPACE"], h.env["RELEASE"] = "/w/kc", tc.ecr, tc.ns, tc.release
		h.env["GITHUB_JOB"], h.env["GITHUB_RUN_ID"], h.env["GITHUB_RUN_ATTEMPT"] = "lane", "5", "3"
		if err := SharedFinish(h.opts()); err != nil {
			t.Fatal(err)
		}
		ns, rel := orDefault(tc.ns, "e2e"), orDefault(tc.release, "lane-r5-a3")
		want := "kubeconfig=/w/kc\n" + tc.want + "gemaal-tier=shared\ngemaal-namespace=" + ns + "\ngemaal-release=" + rel + "\n"
		if got := h.outputsOf(); got != want {
			t.Errorf("%s: %q, want %q", tc.name, got, want)
		}
		if !strings.HasPrefix(h.envOf(), "KUBECONFIG=/w/kc\nSNAPSHOT_REGISTRY="+strings.TrimPrefix(tc.want, "snapshot-registry=")) {
			t.Errorf("%s: env %q", tc.name, h.envOf())
		}
	}
	h := newHarness(t)
	if err := SharedFinish(h.opts()); code(err) != 1 || !strings.Contains(h.err.String(), "shared-connect.sh should have set this") {
		t.Errorf("%v %q", err, h.err.String())
	}
}
