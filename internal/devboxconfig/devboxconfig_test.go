package devboxconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunArgs(t *testing.T) {
	if got := RunArgs(""); !reflect.DeepEqual(got, []string{"run", "--"}) {
		t.Errorf("empty: %v", got)
	}
	if got := RunArgs("ci/devbox"); !reflect.DeepEqual(got, []string{"run", "-c", "ci/devbox", "--"}) {
		t.Errorf("set: %v", got)
	}
}

func TestValidate(t *testing.T) {
	ws := t.TempDir()
	ok := filepath.Join(ws, "ci", "devbox")
	_ = os.MkdirAll(ok, 0o755)
	_ = os.WriteFile(filepath.Join(ok, "devbox.json"), []byte("{}"), 0o644)
	_ = os.MkdirAll(filepath.Join(ws, "empty"), 0o755)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "devbox.json"), []byte("{}"), 0o644)
	_ = os.Symlink(outside, filepath.Join(ws, "link"))

	for _, tc := range []struct{ name, dir, want string }{
		{"empty is fine", "", ""},
		{"a relative directory", "ci/devbox", ""},
		{"with a dot prefix", "./ci/devbox", ""},
		{"absolute", ok, "relative"},
		{"parent escape", "../x", "escapes"},
		{"sneaky escape", "ci/../../x", "escapes"},
		{"missing", "nope", "not a directory"},
		{"no devbox.json", "empty", "no devbox.json"},
		{"symlink out", "link", "outside the workspace"},
	} {
		err := Validate(ws, tc.dir)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestJSONPath(t *testing.T) {
	if JSONPath("") != "devbox.json" || JSONPath("a/b") != "a/b/devbox.json" {
		t.Error("JSONPath")
	}
}
