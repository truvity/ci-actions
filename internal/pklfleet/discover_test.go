package pklfleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discover(t *testing.T, f *fakeAPI, step func(context.Context, DiscoverOptions) error, o DiscoverOptions) (string, string, error) {
	t.Helper()
	var b bytes.Buffer
	out := filepath.Join(t.TempDir(), "gh_out")
	o.Common = Common{Out: &b, Output: out}
	o.Token, o.API = "secret-token", f.srv.URL
	err := step(context.Background(), o)
	return b.String(), read(out), err
}

func TestTarget(t *testing.T) {
	f := newAPI(t)
	f.resp = map[string]string{
		"GET /repos/o/lib/releases/latest":           `{"tag_name":"v0.4.1"}`,
		"GET /repos/o/lib/releases/tags/v0.4.1":      `{}`,
		"GET /repos/o/lib/releases/tags/v0.3.0":      `{}`,
		"GET /repos/o/lib/releases/tags/v1.0.0-rc.1": `{}`,
	}
	for _, tc := range []struct{ name, in, want string }{
		{"empty: the latest release", "", "0.4.1"},
		{"explicit, with a v", "v0.3.0", "0.3.0"},
		{"explicit, without a v", "0.3.0", "0.3.0"},
		{"a prerelease, explicitly", "1.0.0-rc.1", "1.0.0-rc.1"},
	} {
		out, outputs, err := discover(t, f, Target, DiscoverOptions{Source: "o/lib", Version: tc.in})
		if err != nil || outputs != "version="+tc.want+"\n" || out != "moving consumers of o/lib to v"+tc.want+"\n" {
			t.Errorf("%s: err %v outputs %q log %q", tc.name, err, outputs, out)
		}
	}
	for _, tc := range []struct{ name, in, source, want string }{
		{"explicit but never released", "0.9.9", "o/lib", "::error::o/lib has no published release v0.9.9 — nothing to move to\n"},
		{"not a version", "1.2", "o/lib", "::error::'1.2' is not a version (X.Y.Z, optionally -prerelease)\n"},
		{"a branch name", "main", "o/lib", "::error::'main' is not a version (X.Y.Z, optionally -prerelease)\n"},
		{"a source that is not owner/name", "", "o/lib; rm", "::error::source 'o/lib; rm' is not owner/name\n"},
	} {
		out, outputs, err := discover(t, f, Target, DiscoverOptions{Source: tc.source, Version: tc.in})
		if !errors.Is(err, ErrFailed) || out != tc.want || outputs != "" {
			t.Errorf("%s: err %v log %q outputs %q", tc.name, err, out, outputs)
		}
	}
	delete(f.resp, "GET /repos/o/lib/releases/latest")
	if out, _, err := discover(t, f, Target, DiscoverOptions{Source: "o/lib"}); !errors.Is(err, ErrFailed) || out != "::error::could not read the latest release of o/lib\n" {
		t.Errorf("no latest release: %v %q", err, out)
	}
	f.resp["GET /repos/o/lib/releases/latest"] = `{"tag_name":null}`
	if out, _, err := discover(t, f, Target, DiscoverOptions{Source: "o/lib"}); !errors.Is(err, ErrFailed) || out != "::error::o/lib has no release\n" {
		t.Errorf("latest without a tag: %v %q", err, out)
	}
	f.resp["GET /repos/o/lib/releases/latest"] = `not json`
	if out, _, err := discover(t, f, Target, DiscoverOptions{Source: "o/lib"}); !errors.Is(err, ErrFailed) || out != "::error::could not read the latest release of o/lib\n" {
		t.Errorf("latest that is not JSON: %v %q", err, out)
	}
	// The headers the shell's curl sent.
	if f.count("auth=") != 0 {
		t.Log("headers are checked in the shell comparison")
	}
}

const libURL = "package://github.com/o/lib/releases/download"

func pklAt(v string) string {
	return fmt.Sprintf("amends \"pkl:Project\"\ndependencies {\n  [\"vocab\"] { uri = \"%[1]s/v%[2]s/contracts.vocab@%[2]s\" }\n}\n", libURL, v)
}

func (f *fakeAPI) repo(name string, paths []string, files map[string]string, truncated bool) {
	f.resp["GET /repos/o/"+name] = `{"default_branch":"main"}`
	type e struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	tree := []e{{"somedir", "tree"}}
	for _, p := range paths {
		tree = append(tree, e{p, "blob"})
	}
	b, _ := json.Marshal(map[string]any{"truncated": truncated, "tree": tree})
	f.resp[fmt.Sprintf("GET /repos/o/%s/git/trees/main?recursive=1", name)] = string(b)
	for p, body := range files {
		segs := strings.Split(p, "/")
		for i := range segs {
			segs[i] = strings.ReplaceAll(url.QueryEscape(segs[i]), "+", "%20")
		}
		f.resp[fmt.Sprintf("GET /repos/o/%s/contents/%s?ref=main", name, strings.Join(segs, "/"))] = body
	}
}

func TestConsumers(t *testing.T) {
	f := newAPI(t)
	f.repo("r1", []string{"devbox.json", "PklProject", "README.md"}, map[string]string{"PklProject": pklAt("0.2.0")}, false)
	f.repo("r2", []string{"devbox.json", "svc/PklProject", "svc/PklProject.deps.json"}, map[string]string{"svc/PklProject": pklAt("0.2.0")}, false)
	f.repo("r3", []string{"devbox.json", "README.md"}, nil, false)
	f.repo("r4", []string{"devbox.json", "PklProject"}, map[string]string{"PklProject": "amends \"pkl:Project\"\ndependencies { [\"x\"] { uri = \"package://github.com/other/lib/releases/download/v1.0.0/x@1.0.0\" } }"}, false)
	f.repo("r5", []string{"PklProject"}, map[string]string{"PklProject": pklAt("0.2.0")}, false)
	f.resp["GET /repos/o/r6"] = `{"default_branch":"main"}` // no tree: unreadable
	f.repo("r7", []string{"devbox.json", "PklProject"}, map[string]string{"PklProject": pklAt("0.3.0")}, false)
	f.repo("r8", []string{"devbox.json", "a/PklProject", "b/PklProject"}, map[string]string{"a/PklProject": pklAt("0.3.0"), "b/PklProject": pklAt("0.2.0")}, false)
	f.repo("r9", []string{"devbox.json", "PklProject"}, map[string]string{"PklProject": pklAt("0.1.0")}, true)
	f.repo("r10", []string{"devbox.json", "sp ace/PklProject", "x/PklProject"}, map[string]string{"sp ace/PklProject": pklAt("0.2.0")}, false) // x/PklProject unreadable

	out, outputs, err := discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.3.0", Repos: `["o/r1","o/r2","o/r3","o/r4","o/r5","o/r6","o/r7","o/r8"]`})
	if err != nil {
		t.Fatal(err)
	}
	wantLog := "o/r1: 1 of 1 dependency URIs to move\n" +
		"o/r2: 1 of 1 dependency URIs to move\n" +
		"o/r3: no PklProject\n" +
		"o/r4: PklProject files do not depend on o/lib\n" +
		"::error::o/r5: depends on o/lib but has no devbox.json to run its recipes in\n" +
		"::error::o/r6: could not read the tree of main\n" +
		"o/r7: already at v0.3.0\n" +
		"o/r8: 1 of 2 dependency URIs to move\n"
	if out != wantLog || outputs != "repositories=[\"o/r1\",\"o/r2\",\"o/r8\"]\ncount=3\n" {
		t.Errorf("log:\n%s\nwant:\n%s\noutputs %q", out, wantLog, outputs)
	}

	if out, outputs, err = discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.2.0", Repos: `["o/r1"]`}); err != nil || outputs != "repositories=[]\ncount=0\n" || out != "o/r1: already at v0.2.0\n" {
		t.Errorf("at the target everywhere: %v %q %q", err, out, outputs)
	}
	if out, _, err = discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.3.0", Repos: `["o/r9"]`}); err != nil || !strings.HasPrefix(out, "::warning::o/r9: the tree of main is truncated; a PklProject may be missed\n") {
		t.Errorf("a truncated tree warns: %v %q", err, out)
	}
	// A path that needs encoding is requested encoded; an unreadable file is an error annotation and the repository is skipped.
	out, outputs, err = discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.3.0", Repos: `["o/r10"]`})
	if err != nil || out != "::error::o/r10: could not read x/PklProject\n" || outputs != "repositories=[]\ncount=0\n" || f.count("/contents/sp%20ace/PklProject?ref=main") != 1 {
		t.Errorf("unreadable file: %v %q %q", err, out, outputs)
	}
	// An unreadable repository (404, no branch, not JSON) is an error annotation and the rest still run; empty names are skipped.
	f.resp["GET /repos/o/r12"] = `{"default_branch":null}`
	f.resp["GET /repos/o/r13"] = `not json`
	out, outputs, err = discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.3.0", Repos: `["o/r12","o/r13","o/gone","","o/r1"]`})
	if err != nil || out != "::error::o/r12: could not read the repository\n::error::o/r13: could not read the repository\n::error::o/gone: could not read the repository\no/r1: 1 of 1 dependency URIs to move\n" || outputs != "repositories=[\"o/r1\"]\ncount=1\n" {
		t.Errorf("unreadable repositories: %v %q %q", err, out, outputs)
	}
	if _, outputs, _ = discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.3.0", Repos: `[]`}); outputs != "repositories=[]\ncount=0\n" {
		t.Errorf("no repositories: %q", outputs)
	}
	// The raw media type is asked for contents, JSON for the rest.
	f.calls = nil
	_, _, _ = discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.3.0", Repos: `["o/r1"]`})
	if len(f.calls) != 3 || !strings.Contains(f.calls[0], "GET /repos/o/r1 ") || !strings.Contains(f.calls[2], "/contents/PklProject?ref=main") {
		t.Errorf("calls %v", f.calls)
	}
	if _, err := os.Stat("/"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := discover(t, f, Consumers, DiscoverOptions{Source: "o/lib", Version: "0.3.0", Repos: `not json`}); err == nil {
		t.Error("REPOS that is not a JSON array fails the step")
	}
}
