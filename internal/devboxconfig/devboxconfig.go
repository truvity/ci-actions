// Package devboxconfig is the one place that turns the optional devbox-config
// input into devbox arguments, shared by the recipe and setup-devbox actions.
//
// devbox-config names a directory that holds its own devbox.json (and
// devbox.lock): a second, smaller environment than the repository's own. It is
// passed to devbox as `-c <dir>`, which makes that directory devbox's project
// root. Empty means the repository's own devbox.json, and the arguments are
// exactly what they were before the input existed.
package devboxconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Validate checks that dir, when non-empty, is a relative path that stays
// inside workspace and names a directory holding a devbox.json. Empty is valid.
func Validate(workspace, dir string) error {
	if dir == "" {
		return nil
	}
	if filepath.IsAbs(dir) || strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, `\`) {
		return fmt.Errorf("devbox-config %q must be a path relative to the workspace, not an absolute one", dir)
	}
	clean := filepath.Clean(dir)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("devbox-config %q escapes the workspace; it must stay inside it", dir)
	}
	if workspace == "" {
		workspace = "."
	}
	full := filepath.Join(workspace, clean)
	// A symlink must not lead out of the workspace either.
	if root, err := filepath.EvalSymlinks(workspace); err == nil {
		if real, err := filepath.EvalSymlinks(full); err == nil {
			if rel, err := filepath.Rel(root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("devbox-config %q resolves outside the workspace; it must stay inside it", dir)
			}
		}
	}
	st, err := os.Stat(full)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("devbox-config %q is not a directory in the workspace", dir)
	}
	if st, err := os.Stat(filepath.Join(full, "devbox.json")); err != nil || st.IsDir() {
		return fmt.Errorf("devbox-config %q has no devbox.json; point it at the directory that holds the second environment's devbox.json", dir)
	}
	return nil
}

// RunArgs returns the arguments of `devbox run` up to and including the `--`.
// Empty dir gives {"run", "--"}; otherwise {"run", "-c", dir, "--"}.
func RunArgs(dir string) []string {
	if dir == "" {
		return []string{"run", "--"}
	}
	return []string{"run", "-c", dir, "--"}
}

// JSONPath is the devbox.json that devbox reads for dir ("" is the
// repository's own, in the working directory).
func JSONPath(dir string) string {
	if dir == "" {
		return "devbox.json"
	}
	return filepath.Join(dir, "devbox.json")
}
