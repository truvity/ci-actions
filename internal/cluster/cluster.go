// Package cluster is the port of the cluster action's scripts: stand up the
// end-to-end test cluster for the caller's tier and export the same five
// things (KUBECONFIG, SNAPSHOT_REGISTRY, GEMAAL_TIER, GEMAAL_NAMESPACE,
// GEMAAL_RELEASE) whichever tier it is.
//
//	mode: kind    a disposable box on the runner itself: fetch truvity/policy's
//	              hack/kind/ at a pinned release, run it, check its claim.
//	mode: wait    block on a `background: true` kind launch started earlier.
//	mode: shared  the estate's own development cluster: refuse a fork pull
//	              request FIRST, prove the cluster identity, then emit.
//
// docker, kind (through the box's own up.sh), kubectl stay executed commands.
// Log, summary and output lines are the shell scripts' byte for byte.
package cluster

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cmd is one executed command.
type Cmd struct {
	Dir    string
	Env    []string // extra KEY=VALUE entries added to the inherited environment
	Name   string
	Args   []string
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a command to completion. Tests substitute a fake.
type Exec func(ctx context.Context, c Cmd) error

// Options carries the environment and the seams.
type Options struct {
	Getenv func(string) string
	Out    io.Writer
	Err    io.Writer
	Exec   Exec
	Sleep  func(time.Duration)
	// Since is the time elapsed since a start, so tests need not wait.
	Now func() time.Time
	// Fetch downloads the policy release tarball to a file. Nil does a GET.
	Fetch func(ctx context.Context, url, dest string) error
	// Spawn starts the box detached and returns its pid. Nil re-executes
	// this binary as `cluster box-run` in its own session.
	Spawn func(ctx context.Context, args []string, log *os.File) (int, error)
	// Alive reports whether a pid still runs. Nil sends signal 0.
	Alive func(pid int) bool
}

// ExitError is a failure that must end the step with this exit status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// ErrReported marks a failure whose ::error:: line is already written.
var ErrReported = &ExitError{Code: 1}

func (o *Options) defaults() {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.Exec == nil {
		o.Exec = osExec
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

func (o Options) required(names ...string) error {
	for _, n := range names {
		if o.Getenv(n) == "" {
			fmt.Fprintf(o.Err, "%s: parameter null or not set\n", n)
			return &ExitError{Code: 1}
		}
	}
	return nil
}

func (o Options) printf(format string, a ...any) { fmt.Fprintf(o.Out, format, a...) }

func (o Options) fail(format string, a ...any) error {
	o.printf("::error::"+format+"\n", a...)
	return ErrReported
}

// stateDir is the directory a background launch and its wait share: a later
// step has none of this step's env, only files. The default is stable for the
// whole job, which is the point.
func (o Options) stateDir() (string, error) {
	if d := o.Getenv("STATE_DIR"); d != "" {
		return d, nil
	}
	rt := o.Getenv("RUNNER_TEMP")
	if rt == "" {
		fmt.Fprintln(o.Err, "RUNNER_TEMP: RUNNER_TEMP is not set")
		return "", &ExitError{Code: 1}
	}
	return filepath.Join(rt, "cluster-action"), nil
}

// emit writes one output BOTH ways: $GITHUB_ENV, because the rest of a job's
// steps are plain shell that reads KUBECONFIG etc. directly, and
// $GITHUB_OUTPUT, because a step that calls this action wants
// `steps.<id>.outputs.*` without depending on env leaking across a `uses:`
// boundary the way it does across `run:` steps.
func (o Options) emit(envName, value, outputName string) error {
	if err := appendLine(o.Getenv("GITHUB_ENV"), envName+"="+value); err != nil {
		return err
	}
	return appendLine(o.Getenv("GITHUB_OUTPUT"), outputName+"="+value)
}

var emitNames = map[string]string{
	"KUBECONFIG":        "kubeconfig",
	"SNAPSHOT_REGISTRY": "snapshot-registry",
	"GEMAAL_TIER":       "gemaal-tier",
	"GEMAAL_NAMESPACE":  "gemaal-namespace",
	"GEMAAL_RELEASE":    "gemaal-release",
}

// emitFromFile reads a KEY=VALUE file (as written by the launch for the wait
// to pick back up) and emits the five outputs from it.
func (o Options) emitFromFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		key, value, _ := strings.Cut(sc.Text(), "=")
		if key == "" {
			continue
		}
		if out, ok := emitNames[key]; ok {
			if err := o.emit(key, value, out); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func appendLine(path, line string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
