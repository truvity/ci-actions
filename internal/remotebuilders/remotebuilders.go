// Package remotebuilders is the port of the setup-remote-builders action:
// register the CI plane's warm BuildKit builders as one buildx builder.
//
// WHICH nodes is the caller's, an org variable and never a literal here (this
// repository is public; internal DNS is a particular). One entry per node,
// `platforms=endpoint`, whitespace-separated. Each platform builds on the node
// that declares it, natively, on a hot layer cache. docker and buildx stay the
// executed commands (through devbox when the project has one).
package remotebuilders

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// builder is a literal: it is written to GITHUB_ENV, and an input there is
// exactly what CodeQL's environment-injection query objects to.
const builder = "ci"

// Options are the action's input and the runner's environment.
type Options struct {
	Nodes      string // whitespace-separated `platforms=endpoint` entries
	Home       string // where ~/.docker/cli-plugins lives
	RunnerTemp string
	GithubEnv  string
	Dir        string // where devbox.json is looked for; "" is the working directory
	Out        io.Writer
	Err        io.Writer
	Exec       runcmd.Exec
}

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
	if strings.ReplaceAll(o.Nodes, " ", "") == "" {
		return fail("remote-builders is empty — set the org variable or skip this step")
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
