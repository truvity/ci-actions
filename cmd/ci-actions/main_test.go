package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const token = "ghs_do_not_print_me"

// fakeGitHub serves one organisation with one repository that pins a
// reusable workflow, whose own setup-devbox pin is the version under test.
func fakeGitHub(t *testing.T, setupTag, setupSHA string) *httptest.Server {
	t.Helper()
	const wfSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/repos/truvity/ci-actions/tags":
			fmt.Fprintf(w, `[{"name":%q,"commit":{"sha":%q}}]`, setupTag, setupSHA)
		case "/repos/truvity/ci-workflows/tags":
			fmt.Fprintf(w, `[{"name":"v3.18.1","commit":{"sha":%q}}]`, wfSHA)
		case "/orgs/acme/repos":
			fmt.Fprint(w, `[{"name":"app","full_name":"acme/app","default_branch":"main"}]`)
		case "/repos/acme/app/contents/.github/workflows":
			fmt.Fprint(w, `[{"name":"ci.yaml","path":".github/workflows/ci.yaml","type":"file"}]`)
		case "/repos/acme/app/contents/.github/workflows/ci.yaml":
			fmt.Fprintf(w, "jobs:\n  c:\n    uses: truvity/ci-workflows/.github/workflows/check.yaml@%s\n", wfSHA)
		case "/repos/truvity/ci-workflows/contents/.github/workflows/check.yaml":
			fmt.Fprintf(w, "jobs:\n  a:\n    steps:\n      - uses: truvity/ci-actions/setup-devbox@%s\n", setupSHA)
		default:
			http.NotFound(w, r)
		}
	}))
}

func runCLI(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func TestFleetPinsCLI(t *testing.T) {
	sha := strings.Repeat("1", 40)
	srv := fakeGitHub(t, "v1.4.0", sha)
	defer srv.Close()
	env := map[string]string{"GITHUB_TOKEN": token, "GITHUB_API_URL": srv.URL}
	jsonPath := filepath.Join(t.TempDir(), "pins.json")

	for _, tc := range []struct {
		name     string
		args     []string
		env      map[string]string
		wantCode int
		contains []string
	}{
		{"no gate", []string{"fleet", "pins", "--org", "acme"}, env, 0, []string{"acme/app", "v1.4.0", "v3.18.1"}},
		{"gate above", []string{"fleet", "pins", "--org", "acme", "--min-setup-devbox", "v1.6.1"}, env, 1, []string{"acme/app", "below v1.6.1"}},
		{"gate at or below", []string{"fleet", "pins", "--org", "acme", "--min-setup-devbox", "v1.4.0"}, env, 0, []string{"at or above v1.4.0"}},
		{"bad gate", []string{"fleet", "pins", "--org", "acme", "--min-setup-devbox", "new"}, env, 2, nil},
		{"runner filter", []string{"fleet", "pins", "--org", "acme", "--runner-filter", "self-hosted"}, env, 0, []string{"RUNNERS"}},
		{"bad runner filter", []string{"fleet", "pins", "--org", "acme", "--runner-filter", "big"}, env, 2, []string{"--runner-filter"}},
		{"no org", []string{"fleet", "pins"}, env, 2, []string{"--org"}},
		{"no token", []string{"fleet", "pins", "--org", "acme"}, map[string]string{"GITHUB_API_URL": srv.URL}, 2, []string{"GITHUB_TOKEN"}},
		{"json file", []string{"fleet", "pins", "--org", "acme", "--json", jsonPath}, env, 0, nil},
		{"unknown command", []string{"nope"}, env, 2, []string{"unknown command"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, tc.env, tc.args...)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.wantCode, out, errb)
			}
			for _, c := range tc.contains {
				if !strings.Contains(out+errb, c) {
					t.Errorf("output lacks %q:\n%s\n%s", c, out, errb)
				}
			}
			if strings.Contains(out+errb, token) {
				t.Errorf("the token was printed")
			}
		})
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil || !strings.Contains(string(data), `"lowest": "v1.4.0"`) || strings.Contains(string(data), token) {
		t.Errorf("json report: %v\n%s", err, data)
	}

	// '-' sends the JSON to stdout and the table to stderr
	code, out, errb := runCLI(t, env, "fleet", "pins", "--org", "acme", "--json", "-")
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "{") || !strings.Contains(errb, "REPOSITORY") {
		t.Errorf("json to stdout: code %d\n%s\n%s", code, out, errb)
	}
}

func TestFleetPinsScanErrorFailsTheGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/truvity/ci-actions/tags", "/repos/truvity/ci-workflows/tags":
			fmt.Fprint(w, `[]`)
		case "/orgs/acme/repos":
			fmt.Fprint(w, `[{"name":"app","full_name":"acme/app","default_branch":"main"}]`)
		default:
			http.Error(w, "boom "+r.Header.Get("Authorization"), 403)
		}
	}))
	defer srv.Close()
	code, out, errb := runCLI(t, map[string]string{"GITHUB_TOKEN": token, "GITHUB_API_URL": srv.URL}, "fleet", "pins", "--org", "acme", "--min-setup-devbox", "v1.6.1")
	if code != 3 {
		t.Errorf("a repository that cannot be read must not pass: exit %d\n%s%s", code, out, errb)
	}
	if strings.Contains(out+errb, token) {
		t.Errorf("the token was printed:\n%s%s", out, errb)
	}
}

func TestVersionAndUsage(t *testing.T) {
	if code, out, _ := runCLI(t, nil, "version"); code != 0 || strings.TrimSpace(out) != version {
		t.Errorf("version: %d %q", code, out)
	}
	if code, _, errb := runCLI(t, nil); code != 2 || !strings.Contains(errb, "tagged-pins") {
		t.Errorf("usage: %d %q", code, errb)
	}
}
