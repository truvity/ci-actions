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

const sha40 = "59d0647c5f9f635e26cf1bbb787f910197976310"

func action(t *testing.T, uses, with, retiredIf string, extraRun string, dropGuard string) string {
	root := t.TempDir()
	steps := fmt.Sprintf(`runs:
  using: composite
  steps:
    - name: Wire the fleet caches
      uses: %s
      with:
%s
    - name: Warn on the retired cache-server input
      if: %s
      shell: bash
      run: bash x retired-cache-server
    - name: Guard GOPROXY against devbox.json
      shell: bash
      run: bash x guard-goproxy
    - name: Guard AWS config against devbox.json
      shell: bash
      run: bash x guard-aws
    - name: Other
      shell: bash
      run: %s
`, uses, with, retiredIf, extraRun)
	if dropGuard != "" {
		steps = strings.Replace(steps, "    - name: "+dropGuard+"\n      shell: bash\n", "    - name: gone\n      shell: bash\n", 1)
	}
	_ = os.MkdirAll(filepath.Join(root, "setup-devbox"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "setup-devbox", "action.yaml"), []byte(steps), 0o644)
	return root
}

const goodWith = `        bucket: ${{ inputs.go-cache-bucket }}
        region: ${{ inputs.go-cache-region }}
        goproxy: ${{ inputs.goproxy }}`

func downstreamServer(t *testing.T, inputs ...string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+sha40+"/setup-cache/action.yaml" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "inputs:")
		for _, i := range inputs {
			fmt.Fprintf(w, "  %s:\n    description: x\n", i)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func run(t *testing.T, root, base string) (string, error) {
	var b strings.Builder
	err := CacheSeam(context.Background(), CacheSeamOptions{Root: root, BaseURL: base, Out: &b})
	return b.String(), err
}

func TestCacheSeam(t *testing.T) {
	srv := downstreamServer(t, "bucket", "region", "goproxy", "languages", "client-version", "bazel-remote", "npm-registry")
	good := func() string {
		return action(t, "truvity/ci-actions/setup-cache@"+sha40+" # v0.2.0", goodWith, "inputs.go-cache-server != ''", "echo hi", "")
	}
	if out, err := run(t, good(), srv.URL); err != nil || out != "cache seam holds (6 cases checked)\n" {
		t.Errorf("a good seam: %v %q", err, out)
	}
	for _, tc := range []struct {
		name string
		root func() string
		base string
		want string
	}{
		{"a tag is not a pin", func() string {
			return action(t, "truvity/ci-actions/setup-cache@v0.2.0", goodWith, "inputs.go-cache-server != ''", "echo", "")
		}, srv.URL, `FAIL [pin]: the cache step does not use a SHA-pinned truvity/ci-actions/setup-cache; it uses "truvity/ci-actions/setup-cache@v0.2.0"`},
		{"a short sha is not a full one", func() string {
			return action(t, "truvity/ci-actions/setup-cache@59d0647", goodWith, "inputs.go-cache-server != ''", "echo", "")
		}, srv.URL, `FAIL [pin]: ci-actions/setup-cache is pinned to "59d0647", which is not a full 40-character SHA`},
		{"an input the downstream declares and this does not pass", func() string {
			return action(t, "truvity/ci-actions/setup-cache@"+sha40, "        bucket: ${{ inputs.go-cache-bucket }}", "inputs.go-cache-server != ''", "echo", "")
		}, srv.URL, `FAIL [inputs]: ci-actions/setup-cache declares "goproxy" and this action does not pass it`},
		{"an input wired to a literal does not cross the seam", func() string {
			return action(t, "truvity/ci-actions/setup-cache@"+sha40, "        bucket: x\n        region: ${{ inputs.r }}\n        goproxy: ${{ inputs.g }}", "inputs.go-cache-server != ''", "echo", "")
		}, srv.URL, `(wired to "x"); pass it, or exempt it with a reason`},
		{"a downstream that cannot be fetched is a failure, not a skip", good, "http://127.0.0.1:1", "could not fetch http://127.0.0.1:1/" + sha40 + "/setup-cache/action.yaml — the seam went unchecked"},
		{"a retired warning that does not fire on the input", func() string {
			return action(t, "truvity/ci-actions/setup-cache@"+sha40, goodWith, "always()", "echo", "")
		}, srv.URL, `FAIL [retired]: the warning step's condition is "always()"`},
		{"a run step that writes GOCACHEPROG", func() string {
			return action(t, "truvity/ci-actions/setup-cache@"+sha40, goodWith, "inputs.go-cache-server != ''", "echo GOCACHEPROG=x", "")
		}, srv.URL, "FAIL [ownership]: a run: step in setup-devbox writes GOCACHEPROG"},
		{"the GOPROXY guard gone", func() string {
			return action(t, "truvity/ci-actions/setup-cache@"+sha40, goodWith, "inputs.go-cache-server != ''", "echo", "Guard GOPROXY against devbox.json")
		}, srv.URL, `FAIL [guard]: the "Guard GOPROXY against devbox.json" step is gone`},
		{"the AWS guard gone", func() string {
			return action(t, "truvity/ci-actions/setup-cache@"+sha40, goodWith, "inputs.go-cache-server != ''", "echo", "Guard AWS config against devbox.json")
		}, srv.URL, `FAIL [aws-guard]: the "Guard AWS config against devbox.json" step is gone`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(t, tc.root(), tc.base)
			if err != ErrFailed || !strings.Contains(out, tc.want) || !strings.Contains(out, "case(s) failed of") {
				t.Errorf("%v\n%s", err, out)
			}
		})
	}
	if _, err := run(t, t.TempDir(), srv.URL); err == nil {
		t.Error("no action file is an error")
	}
}

// The real action.yaml of this repository satisfies the seam, against a
// downstream that declares the inputs the real one passes.
func TestRealAction(t *testing.T) {
	b, err := os.ReadFile("../../setup-devbox/action.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "truvity/ci-actions/setup-cache@") {
		t.Skip("no cache delegation in this action")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "inputs:\n  bucket: {}\n  region: {}\n  endpoint: {}\n  path-style: {}\n  goproxy: {}\n  languages: {}\n  client-version: {}")
	}))
	defer srv.Close()
	var out strings.Builder
	if err := CacheSeam(context.Background(), CacheSeamOptions{Root: "../..", BaseURL: srv.URL, Out: &out}); err != nil {
		t.Errorf("%v\n%s", err, out.String())
	}
}
