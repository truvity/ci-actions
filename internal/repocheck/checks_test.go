package repocheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var word = "su" + "do" // assembled: the scanner would flag this file's source otherwise

func tree(t *testing.T, files map[string]string) string {
	root := t.TempDir()
	for p, c := range files {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A scanner that finds nothing anywhere cannot be told apart from one that
// looks at nothing: plant the call in each shape it must catch.
func TestNoEscalation(t *testing.T) {
	root := tree(t, map[string]string{
		"act/action.yaml":                       "runs:\n  steps:\n    - shell: bash\n      run: " + word + " ln -sf /bin/bash /bin/sh\n",
		"act/x.sh":                              "#!/usr/bin/env bash\n# " + word + " apt-get update\n",
		"Justfile":                              "recipe:\n    " + word + " true\n",
		"hack/restricted-sim/Dockerfile":        "FROM x\nRUN " + word + " true\n",
		"CHANGELOG.md":                          "we removed " + word + "\n",
		"node_modules/pkg/index.js":             word + " x\n",
		".git/hooks/pre-commit":                 word + " x\n",
		"act/ok.sh":                             "pseudo-" + word + "x and pseudocode, " + strings.ToUpper(word) + "\n",
		"bin/blob":                              "\x00\x01 " + word + " \x02",
		"deep/restricted-sim-not/Dockerfile.sh": word + " is flagged: only the directory restricted-sim is exempt\n",
	})
	var out strings.Builder
	err := NoEscalation(root, &out)
	if err != ErrFailed {
		t.Fatalf("err = %v", err)
	}
	o := out.String()
	for _, want := range []string{"act/action.yaml:4:", "act/x.sh:2:", "Justfile:2:", "deep/restricted-sim-not/Dockerfile.sh:1:"} {
		if !strings.Contains(o, want) {
			t.Errorf("a call is found: %s\n%s", want, o)
		}
	}
	for _, not := range []string{"restricted-sim/Dockerfile", "CHANGELOG", "node_modules", ".git/", "ok.sh", "bin/blob"} {
		if strings.Contains(o, not+":") {
			t.Errorf("%s is exempt or not a hit:\n%s", not, o)
		}
	}
	if !strings.HasSuffix(o, "::error::the no-privilege contract is broken: no "+word+" in action files or scripts\n") {
		t.Errorf("tail:\n%s", o)
	}

	// a clean tree
	clean := tree(t, map[string]string{"a.sh": "echo hi\n", "README.md": word + "\n"})
	out.Reset()
	if err := NoEscalation(clean, &out); err != nil || out.String() != "ok    this repository is free of "+word+"\n" {
		t.Errorf("%v %q", err, out.String())
	}
}

// This repository is held to it.
func TestThisRepositoryIsFree(t *testing.T) {
	var out strings.Builder
	if err := NoEscalation("../..", &out); err != nil {
		t.Errorf("%s", out.String())
	}
}

const kitBody = "depguard:\n  rules:\n    main:\n      deny: []\n"

func kitTree(t *testing.T, source, body string) string {
	manifest := "golangci-depguard.yaml:\n  path: .golangci.yaml\n"
	if source != "" {
		manifest += "  source: " + source + "\n"
	}
	return tree(t, map[string]string{
		"caller-parity/kits/kits.yaml":              manifest,
		"caller-parity/kits/golangci-depguard.yaml": body,
	})
}

func TestPolicyKit(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/acme/policy/v1.2.3/lint/golangci-depguard.yaml" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, kitBody)
	}))
	defer srv.Close()
	run := func(root string) (string, error) {
		var b strings.Builder
		err := PolicyKit(context.Background(), PolicyKitOptions{Root: root, BaseURL: srv.URL, Out: &b})
		return b.String(), err
	}

	if out, err := run(kitTree(t, "acme/policy@v1.2.3", kitBody)); err != nil || out != "policy-kit-current: caller-parity/kits/golangci-depguard.yaml matches acme/policy@v1.2.3 verbatim\n" {
		t.Errorf("a verbatim copy: %v %q", err, out)
	}
	if gotPath != "/acme/policy/v1.2.3/lint/golangci-depguard.yaml" {
		t.Errorf("the pinned tag's blob is read: %q", gotPath)
	}

	out, err := run(kitTree(t, "acme/policy@v1.2.3", strings.Replace(kitBody, "[]", "[x]", 1)))
	if err != ErrFailed || !strings.HasPrefix(out, "::error::caller-parity/kits/golangci-depguard.yaml has drifted from acme/policy@v1.2.3 — update the kit, or bump the pin if acme/policy moved on purpose\n--- acme/policy@v1.2.3:lint/golangci-depguard.yaml\n+++ caller-parity/kits/golangci-depguard.yaml\n") ||
		!strings.Contains(out, "-      deny: []\n+      deny: [x]\n") {
		t.Errorf("drift: %v\n%s", err, out)
	}

	for name, tc := range map[string]struct{ source, want string }{
		"no source":         {"", "::error::kits.yaml has no source: for golangci-depguard.yaml\n"},
		"no tag":            {"acme/policy", "::error::kits.yaml's source (acme/policy) is not owner/repo@tag\n"},
		"an empty tag":      {"acme/policy@", "::error::kits.yaml's source (acme/policy@) is not owner/repo@tag\n"},
		"an unreadable tag": {"acme/policy@v9", "::error::could not read " + srv.URL + "/acme/policy/v9/lint/golangci-depguard.yaml — is the tag right, and is lint/golangci-depguard.yaml still there?\n"},
	} {
		out, err := run(kitTree(t, tc.source, kitBody))
		if err != ErrFailed || out != tc.want {
			t.Errorf("%s: %v %q", name, err, out)
		}
	}
}
