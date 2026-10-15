package recipe

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

func run(recipe, command string, fn func(c runcmd.Cmd) error) (string, error, []string) {
	var out strings.Builder
	var calls []string
	err := Run(context.Background(), Options{Recipe: recipe, Command: command, Out: &out, Exec: func(_ context.Context, c runcmd.Cmd) error {
		calls = append(calls, c.Name+" "+strings.Join(c.Args, " "))
		return fn(c)
	}})
	return out.String(), err, calls
}

func TestRecipe(t *testing.T) {
	clean := func(c runcmd.Cmd) error { return nil }
	for _, tc := range []struct {
		name, recipe, command string
		fn                    func(c runcmd.Cmd) error
		out                   string
		fail                  bool
		calls                 string
	}{
		{"a recipe that leaves the tree clean", "build", "just", clean, "", false, "devbox run -- just build|git status --porcelain"},
		{"another task runner", "lint", "moon", clean, "", false, "devbox run -- moon lint|git status --porcelain"},
		{"a failing recipe says so and never looks at the tree", "test", "just", func(c runcmd.Cmd) error {
			if c.Name == "devbox" {
				fmt.Fprintln(c.Stdout, "boom")
				return errors.New("exit status 2")
			}
			return nil
		}, "boom\n::error::test failed\n", true, "devbox run -- just test"},
		{"a recipe that modifies the tree", "gen", "just", func(c runcmd.Cmd) error {
			if c.Name == "git" {
				fmt.Fprint(c.Stdout, " M api.pb.go\n?? new.txt\n")
			}
			return nil
		}, "::error::gen modified the working tree:\n M api.pb.go\n?? new.txt\nCommit the result, or make the recipe read-only.\n", true, "devbox run -- just gen|git status --porcelain"},
		{"a git that cannot answer reads as clean, as it did", "build", "just", func(c runcmd.Cmd) error {
			if c.Name == "git" {
				return errors.New("not a repository")
			}
			return nil
		}, "", false, "devbox run -- just build|git status --porcelain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err, calls := run(tc.recipe, tc.command, tc.fn)
			if (err != nil) != tc.fail || (err != nil && !errors.Is(err, ErrFailed)) {
				t.Errorf("err = %v, fail want %v", err, tc.fail)
			}
			if out != tc.out {
				t.Errorf("out %q, want %q", out, tc.out)
			}
			if strings.Join(calls, "|") != tc.calls {
				t.Errorf("calls %v", calls)
			}
		})
	}
}

func TestDevboxConfig(t *testing.T) {
	ws := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ws, "ci"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "ci", "devbox.json"), []byte("{}"), 0o644)
	do := func(cfg string) (string, error, []string) {
		var out strings.Builder
		var calls []string
		err := Run(context.Background(), Options{Recipe: "build", Command: "just", DevboxConfig: cfg, Workspace: ws, Out: &out, Exec: func(_ context.Context, c runcmd.Cmd) error {
			calls = append(calls, c.Name+" "+strings.Join(c.Args, " "))
			return nil
		}})
		return out.String(), err, calls
	}
	if _, err, calls := do(""); err != nil || strings.Join(calls, "|") != "devbox run -- just build|git status --porcelain" {
		t.Errorf("empty: %v %v", err, calls)
	}
	if _, err, calls := do("ci"); err != nil || strings.Join(calls, "|") != "devbox run -c ci -- just build|git status --porcelain" {
		t.Errorf("set: %v %v", err, calls)
	}
	for _, bad := range []string{"/etc", "../x", "missing", "."} {
		out, err, calls := do(bad)
		if !errors.Is(err, ErrFailed) || len(calls) != 0 || !strings.HasPrefix(out, "::error::devbox-config ") {
			t.Errorf("%q: %v %q %v", bad, err, out, calls)
		}
	}
}
