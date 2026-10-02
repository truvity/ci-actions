// Package runcmd is the seam through which an action's Go logic runs the
// programs it does not reimplement (devbox, git, docker): one small type, so
// a test can answer for them.
package runcmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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

// OS is the real one.
func OS(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	return cmd.Run()
}

// ExitCode is the exit status a failed command carried, 1 for any other error
// and 0 for none: what a shell step running it under `set -e` would exit with.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		if c := ec.ExitCode(); c > 0 {
			return c
		}
	}
	return 1
}
