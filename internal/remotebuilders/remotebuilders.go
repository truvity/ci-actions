// Package remotebuilders is the port of the setup-remote-builders action:
// register the CI plane's warm BuildKit builders as one buildx builder.
//
// WHICH nodes is the caller's, an org variable and never a literal here (this
// repository is public; internal DNS is a particular). One entry per node,
// `platforms=endpoint`, whitespace-separated. Each platform builds on the node
// that declares it, natively, on a hot layer cache. docker and buildx stay the
// executed commands (through devbox when the project has one).
//
// Transport comes from the runner, not from the caller: when the runner
// provides CI_BUILDKIT_TLS_DIR (a directory holding ca.pem, cert.pem and
// key.pem, which setup-buildkit-tls writes from the PEM variables) every node
// is dialled with mutual TLS; without it the nodes are plain TCP.
package remotebuilders

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/truvity/ci-actions/internal/runcmd"
	"github.com/truvity/ci-actions/internal/runnerenv"
)

// builder is a literal: it is written to GITHUB_ENV, and an input there is
// exactly what CodeQL's environment-injection query objects to.
const builder = "ci"

// Options are the action's input and the runner's environment.
type Options struct {
	Nodes string // whitespace-separated `platforms=endpoint` entries
	// RunnerNodes is what the RUNNER exports as CI_RUNNER_REMOTE_BUILDERS, same
	// format. When set it wins over Nodes: a pool that reaches the builders by
	// other names than the caller's variable carries its own.
	RunnerNodes string
	Home        string // where ~/.docker/cli-plugins lives
	RunnerTemp  string
	// TLSDir is CI_BUILDKIT_TLS_DIR. Set, every node is registered with the
	// client certificate in it (ca.pem, cert.pem, key.pem); empty, plain TCP.
	TLSDir string
	// TLSServername is CI_BUILDKIT_TLS_SERVERNAME: the certificate name the
	// builders are verified against when it is not the endpoint host. Only
	// read when TLSDir is set.
	TLSServername string
	// BuildxCache is the caller's BuildKit registry-cache reference (the organisation
	// variable CI_BUILDX_CACHE / CI_BUILDX_CACHE_STABLE) and RunnerBuildxCache what
	// the RUNNER exports as CI_RUNNER_BUILDX_CACHE: the cache of the runner's own tier.
	// The runner's wins, and is then exported as CI_BUILDX_CACHE for the recipes'
	// `--cache-from/--cache-to type=registry`; the caller's is left where the
	// workflow put it.
	BuildxCache       string
	RunnerBuildxCache string
	// RunnerCacheWrites is CI_RUNNER_CACHE_WRITES. With "release-credentials" the
	// runner's identity only pulls the BuildKit cache, so CI_BUILDX_CACHE (which
	// consumers turn into both --cache-from and --cache-to) is NOT exported: the
	// release workflow sets it after it assumed its role. CI_BUILDX_CACHE_FROM,
	// the read reference, is always exported.
	RunnerCacheWrites string
	GithubEnv         string
	Dir               string // where devbox.json is looked for; "" is the working directory
	Out               io.Writer
	Err               io.Writer
	Exec              runcmd.Exec
}

var (
	plainDirPath    = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	plainServername = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	// registryRef is <registry host>[:port]/<repository>, no tag, no space.
	registryRef = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?/[a-z0-9][a-z0-9._/-]*$`)
)

// ErrFailed marks a failure whose ::error:: line is already written.
var ErrFailed = errors.New("remote-builders: failed")

// Run registers the builder and verifies it offers every platform asked for.
func Run(ctx context.Context, o Options) error {
	if o.Exec == nil {
		o.Exec = runcmd.OS
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	fail := func(format string, a ...any) error {
		fmt.Fprintf(o.Out, "::error::"+format+"\n", a...)
		return ErrFailed
	}
	nodes, src, err := runnerenv.Pick(runnerenv.RemoteBuilder, o.RunnerNodes, o.Nodes, true)
	if err != nil {
		return fail("%v", err)
	}
	o.Nodes = nodes
	if strings.TrimSpace(o.Nodes) == "" {
		return fail("remote-builders is empty — set the org variable, export " + runnerenv.RemoteBuilder + " from the runner, or skip this step")
	}
	fmt.Fprintf(o.Err, "%s\n", runnerenv.Describe("remote builders", src, strings.Join(strings.Fields(nodes), " "), runnerenv.RemoteBuilder))

	// The BuildKit cache is the runner's when it names one. A value that is no registry
	// reference never reaches GITHUB_ENV.
	cache, csrc, err := runnerenv.Pick(runnerenv.BuildxCache, o.RunnerBuildxCache, o.BuildxCache, false)
	if err != nil {
		return fail("%v", err)
	}
	if csrc == runnerenv.FromRunner && !registryRef.MatchString(cache) {
		return fail("%s is not a registry reference (<registry>/<repository>, no tag)", runnerenv.BuildxCache)
	}
	if csrc != runnerenv.Unset {
		fmt.Fprintf(o.Err, "%s\n", runnerenv.Describe("buildkit cache", csrc, cache, runnerenv.BuildxCache))
	}

	// The driver options that make a node mutual TLS. They are the same for
	// every node, and every node needs them (buildx keeps them per node).
	var tlsOpts []string
	if o.TLSDir != "" {
		if !plainDirPath.MatchString(o.TLSDir) {
			return fail("CI_BUILDKIT_TLS_DIR is not a plain absolute path")
		}
		for _, f := range []string{"ca.pem", "cert.pem", "key.pem"} {
			if _, err := os.Stat(filepath.Join(o.TLSDir, f)); err != nil {
				return fail("CI_BUILDKIT_TLS_DIR is set but %s is not readable", filepath.Join(o.TLSDir, f))
			}
		}
		opts := []string{"cacert=" + o.TLSDir + "/ca.pem", "cert=" + o.TLSDir + "/cert.pem", "key=" + o.TLSDir + "/key.pem"}
		if o.TLSServername != "" {
			if !plainServername.MatchString(o.TLSServername) {
				return fail("CI_BUILDKIT_TLS_SERVERNAME is not a plain DNS name")
			}
			opts = append(opts, "servername="+o.TLSServername)
		}
		tlsOpts = []string{"--driver-opt", strings.Join(opts, ",")}
		fmt.Fprintf(o.Err, "remote builders: mutual TLS, credentials from %s\n", o.TLSDir)
	} else {
		fmt.Fprintf(o.Err, "remote builders: plain TCP (CI_BUILDKIT_TLS_DIR is not set)\n")
	}

	// docker + buildx resolve from the job's PATH; through devbox when the
	// project has one (a devbox docker only finds the plugins it can see,
	// hence the symlink), and the runner image's system-wide plugin path
	// covers the rest.
	_, derr := os.Stat(filepath.Join(o.Dir, "devbox.json"))
	viaDevbox := derr == nil
	run := func(stdout io.Writer, stderr io.Writer, args ...string) error {
		name, a := args[0], args[1:]
		if viaDevbox {
			a = append([]string{"run", "--", name}, a...)
			name = "devbox"
		}
		return o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: name, Args: a, Stdout: stdout, Stderr: stderr})
	}

	var plugin strings.Builder
	if err := run(&plugin, io.Discard, "sh", "-c", "command -v docker-buildx"); err == nil {
		if p := strings.TrimRight(plugin.String(), "\n"); p != "" {
			dir := filepath.Join(o.Home, ".docker", "cli-plugins")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			link := filepath.Join(dir, "docker-buildx")
			_ = os.Remove(link) // ln -sf
			if err := os.Symlink(p, link); err != nil {
				return err
			}
		}
	}

	// rm is the ONLY tolerated failure (builder absent on a fresh ephemeral
	// runner); registrations must fail the step: a builder missing a platform
	// surfaces later as a baffling "no match for platform" build error.
	_ = run(o.Out, io.Discard, "docker", "buildx", "rm", builder)

	entries := strings.Fields(o.Nodes)
	for i, entry := range entries {
		platforms, endpoint := splitEntry(entry)
		args := []string{"docker", "buildx", "create"}
		if i > 0 {
			args = append(args, "--append")
		}
		args = append(args, "--name", builder, "--driver", "remote", endpoint, "--platform", platforms)
		args = append(args, tlsOpts...)
		if err := run(o.Out, o.Err, args...); err != nil {
			return fmt.Errorf("docker buildx create: %w", err)
		}
	}
	if err := run(o.Out, o.Err, "docker", "buildx", "use", builder); err != nil {
		return fmt.Errorf("docker buildx use: %w", err)
	}
	report := filepath.Join(o.RunnerTemp, "buildx-"+builder+".txt")
	f, err := os.Create(report)
	if err != nil {
		return err
	}
	err = run(io.MultiWriter(o.Out, f), o.Err, "docker", "buildx", "inspect", "--bootstrap", builder)
	f.Close()
	if err != nil {
		return fmt.Errorf("docker buildx inspect: %w", err)
	}
	b, _ := os.ReadFile(report)
	for _, entry := range entries {
		platforms, _ := splitEntry(entry)
		for _, p := range strings.Fields(strings.ReplaceAll(platforms, ",", " ")) {
			if !strings.Contains(string(b), p) {
				return fail("builder %s does not offer %s", builder, p)
			}
		}
	}
	if o.GithubEnv != "" {
		ef, err := os.OpenFile(o.GithubEnv, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintf(ef, "BUILDX_BUILDER=%s\n", builder)
		// Validated above (a runner's value): one line, no whitespace. The
		// caller's own value is the workflow's business, as before.
		if from := strings.TrimSpace(cache); csrc != runnerenv.Unset && registryRef.MatchString(from) {
			fmt.Fprintf(ef, "CI_BUILDX_CACHE_FROM=%s\n", from)
		}
		if csrc == runnerenv.FromRunner && !runnerenv.WritesNeedJobCredentials(o.RunnerCacheWrites) {
			fmt.Fprintf(ef, "CI_BUILDX_CACHE=%s\n", cache)
		}
		return ef.Close()
	}
	return nil
}

// splitEntry is `${entry%%=*}` and `${entry#*=}`: with no `=` both are the
// entry.
func splitEntry(entry string) (platforms, endpoint string) {
	if i := strings.Index(entry, "="); i >= 0 {
		return entry[:i], entry[i+1:]
	}
	return entry, entry
}
