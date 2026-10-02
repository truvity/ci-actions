// Package setupdevbox is the logic of the setup-devbox composite action: the
// bootstrap every job runs first (preflight, AWS config, nix and devbox,
// proto, tokens, CodeArtifact, the devbox.json guards), now a set of steps of
// the ci-actions binary. The composite keeps the structure (third-party
// actions, conditions, caching); each of its own steps is a thin wrapper that
// runs one of these.
//
// It runs with NO privilege, by contract: no root, no privilege escalation, no
// relinking of /bin/sh. The one exception is the nix installer, a third-party
// action that needs root and runs only when nix is not baked into the runner.
package setupdevbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// Env carries the environment and the seams of every step.
type Env struct {
	Getenv func(string) string
	// Lookup distinguishes an unset variable from an empty one (os.LookupEnv);
	// nil treats empty as unset.
	Lookup func(string) (string, bool)
	Out    io.Writer
	Err    io.Writer
	Exec   runcmd.Exec
	Sleep  func(time.Duration)
	Now    func() time.Time
}

// ErrFailed marks a failure whose ::error:: line is already written.
var ErrFailed = errors.New("setup-devbox: failed")

func (e *Env) defaults() {
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Out == nil {
		e.Out = io.Discard
	}
	if e.Err == nil {
		e.Err = io.Discard
	}
	if e.Exec == nil {
		e.Exec = runcmd.OS
	}
	if e.Sleep == nil {
		e.Sleep = time.Sleep
	}
	if e.Now == nil {
		e.Now = time.Now
	}
}

func (e Env) printf(format string, a ...any) { fmt.Fprintf(e.Out, format, a...) }

func (e Env) fail(format string, a ...any) error {
	e.printf("::error::"+format+"\n", a...)
	return ErrFailed
}

// appendTo appends text to a file named by an environment variable of the
// runner (GITHUB_ENV, GITHUB_OUTPUT, GITHUB_STEP_SUMMARY); unset means none.
func appendTo(path, text string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// exportEnv writes NAME=value to GITHUB_ENV with a RANDOM-DELIMITER heredoc,
// the multi-line form GitHub documents. GITHUB_ENV is line-oriented, so a
// value carrying a newline in the NAME=VALUE form writes a second variable of
// the caller's choosing (CodeQL envvar-injection). 32 hex characters of
// randomness make the delimiter unguessable, so the value cannot terminate the
// block early either.
func (e Env) exportEnv(name, value string) error {
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	d := "EOF_" + hex.EncodeToString(r[:])
	return appendTo(e.Getenv("GITHUB_ENV"), fmt.Sprintf("%s<<%s\n%s\n%s\n", name, d, value, d))
}

// retry is `for i in 1 2; do "$@" && return 0; echo "attempt $i failed;
// sleeping"; sleep 20; done; "$@"`: two attempts with a pause, then a last one
// whose status propagates.
func (e Env) retry(ctx context.Context, out io.Writer, run func(io.Writer) error) error {
	for i := 1; i <= 2; i++ {
		if run(out) == nil {
			return nil
		}
		fmt.Fprintf(out, "attempt %d failed; sleeping\n", i)
		e.Sleep(20 * time.Second)
	}
	return run(out)
}

func (e Env) devbox(ctx context.Context, out, errw io.Writer, args ...string) error {
	return e.Exec(ctx, runcmd.Cmd{Name: "devbox", Args: append([]string{"run", "--"}, args...), Stdout: out, Stderr: errw})
}

// trimNL is what a command substitution does to its result.
func trimNL(s string) string { return strings.TrimRight(s, "\n") }
