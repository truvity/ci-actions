package devboxparity

import (
	"context"
	"io"
	"os/exec"
	"strings"
)

// Cmd is one executed command. devbox, git and gh stay what they were in
// the shell version: programs this package runs, never reimplements.
type Cmd struct {
	Dir    string
	Env    []string // the full environment of the child
	Name   string
	Args   []string
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a command to completion. Tests substitute a fake.
type Exec func(ctx context.Context, c Cmd) error

// OSExec is the real one.
func OSExec(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	return cmd.Run()
}

// withoutEnv returns env minus the named variables, and with `add` set.
func withEnv(env []string, drop []string, add ...string) []string {
	out := make([]string, 0, len(env)+len(add))
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		skip := false
		for _, d := range drop {
			if k == d {
				skip = true
			}
		}
		for _, a := range add {
			if strings.HasPrefix(a, k+"=") {
				skip = true
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return append(out, add...)
}

func envLookup(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], key+"=") {
			return env[i][len(key)+1:]
		}
	}
	return ""
}
