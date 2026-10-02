// Package recipe is the port of the recipe action: run one task-runner recipe
// inside devbox and prove it left the working tree alone.
//
// It exists so check.yaml can carry a NAMED STEP per recipe (the log shows
// "build", "test", "lint" with their own durations and ticks) without
// repeating the body ten times. devbox and git stay the executed commands.
package recipe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// Options are the action's inputs.
type Options struct {
	Recipe  string
	Command string // the task runner in front of it: just, moon
	Out     io.Writer
	Err     io.Writer
	Exec    runcmd.Exec
}

// ErrFailed marks a failure whose ::error:: line is already written.
var ErrFailed = errors.New("recipe: failed")

// Run runs the recipe, then checks the tree.
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
	if err := o.Exec(ctx, runcmd.Cmd{Name: "devbox", Args: []string{"run", "--", o.Command, o.Recipe}, Stdout: o.Out, Stderr: o.Err}); err != nil {
		fmt.Fprintf(o.Out, "::error::%s failed\n", o.Recipe)
		return ErrFailed
	}

	// Nothing verified this before: a recipe that writes a generated file, or
	// a linter invoked with --fix, changed the tree and nobody noticed.
	// --porcelain respects .gitignore, and the setup action hides its own
	// .prototools mutation with assume-unchanged, so a clean repository reads
	// clean. A git that cannot answer reads as clean, as it did.
	var out strings.Builder
	_ = o.Exec(ctx, runcmd.Cmd{Name: "git", Args: []string{"status", "--porcelain"}, Stdout: &out, Stderr: o.Err})
	if dirty := strings.TrimRight(out.String(), "\n"); dirty != "" {
		fmt.Fprintf(o.Out, "::error::%s modified the working tree:\n", o.Recipe)
		fmt.Fprintf(o.Out, "%s\n", dirty)
		fmt.Fprintln(o.Out, "Commit the result, or make the recipe read-only.")
		return ErrFailed
	}
	return nil
}
