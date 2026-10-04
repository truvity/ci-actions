package pklfleet

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAPI is a GitHub API in a map: "METHOD /path?query" to a body. A missing
// GET is a 404; a missing write answers {}. Every call is recorded.
type fakeAPI struct {
	t     *testing.T
	srv   *httptest.Server
	resp  map[string]string
	calls []string
}

func newAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, resp: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer secret-token" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			http.Error(w, "headers", 401)
			return
		}
		key := r.Method + " " + r.URL.RequestURI()
		f.calls = append(f.calls, key+" "+string(b))
		if body, ok := f.resp[key]; ok {
			io.WriteString(w, body)
			return
		}
		if r.Method == "GET" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "{}")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) count(sub string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func (f *fakeAPI) prFakes() {
	f.resp = map[string]string{
		"GET /repos/o/r/pulls?state=open&head=o:pkl-contracts-0.3.0&per_page=100": "[]",
		"GET /repos/o/r/pulls?state=open&per_page=100":                            "[]",
		"POST /repos/o/r/pulls":                                                   `{"number":7,"html_url":"https://example.test/o/r/pull/7","node_id":"PR_node7"}`,
		"POST /graphql":                                                           `{"data":{"enablePullRequestAutoMerge":{"clientMutationId":null}}}`,
	}
	f.calls = nil
}

type pubRig struct {
	t       *testing.T
	api     *fakeAPI
	origin  string
	clone   string
	dirs    string
	summary string
	out     string
}

func newRig(t *testing.T) *pubRig {
	d := t.TempDir()
	r := &pubRig{t: t, api: newAPI(t), origin: filepath.Join(d, "origin.git"), clone: filepath.Join(d, "clone"), dirs: filepath.Join(d, "dirs"), summary: filepath.Join(d, "summary"), out: filepath.Join(d, "gh_out")}
	git(t, d, "init", "-q", "--bare", "-b", "main", r.origin)
	git(t, d, "init", "-q", "-b", "main", r.clone)
	git(t, r.clone, "remote", "add", "origin", r.origin)
	write(t, r.clone+"/PklProject", "v1\n")
	git(t, r.clone, "add", "-A")
	git(t, r.clone, "commit", "-q", "-m", "base")
	git(t, r.clone, "push", "-q", "origin", "main")
	write(t, r.dirs, ".\nsvc\n")
	r.api.prFakes()
	return r
}

func (r *pubRig) change(v string) { write(r.t, r.clone+"/PklProject", v+"\n") }

func (r *pubRig) run(over func(*PublishOptions)) (string, error) {
	var b bytes.Buffer
	os.Remove(r.out)
	o := PublishOptions{Common: Common{Out: &b, Dir: r.clone, Output: r.out, Summary: r.summary}, Token: "secret-token", Repo: "o/r", Base: "main", API: r.api.srv.URL,
		Source: "o/lib", Version: "0.3.0", From: "0.2.0", Breaking: "false", DirsFile: r.dirs, BranchPrefix: "pkl-contracts-", DryRun: "false", AutoMerge: "true",
		GitUser: "fleet[bot]", GitEmail: "bot@example"}
	if over != nil {
		over(&o)
	}
	err := Publish(context.Background(), o)
	return b.String(), err
}

func (r *pubRig) remoteRef() string {
	cmd := exec.Command("git", "rev-parse", "--quiet", "--verify", "refs/heads/pkl-contracts-0.3.0")
	cmd.Dir = r.origin
	b, err := cmd.Output()
	if err != nil {
		return "none"
	}
	return strings.TrimSpace(string(b))
}

func TestPublishDryRun(t *testing.T) {
	r := newRig(t)
	r.change("v2")
	out, err := r.run(func(o *PublishOptions) { o.DryRun = "true" })
	if err != nil || r.remoteRef() != "none" || len(r.api.calls) != 0 {
		t.Errorf("dry run acted: err %v ref %s calls %v", err, r.remoteRef(), r.api.calls)
	}
	if !strings.HasPrefix(out, "dry run: would push pkl-contracts-0.3.0 and open a pull request with:\n M PklProject\n") || !strings.Contains(out, "PklProject | 2 +-") {
		t.Errorf("log %q", out)
	}
	if got := read(r.summary); got != "- o/r: dry run, 0.2.0 -> 0.3.0\n" {
		t.Errorf("summary %q", got)
	}
	// An empty FROM prints a question mark.
	_, _ = r.run(func(o *PublishOptions) { o.DryRun = "true"; o.From = "" })
	if got := read(r.summary); !strings.HasSuffix(got, "- o/r: dry run, ? -> 0.3.0\n") {
		t.Errorf("summary %q", got)
	}
}

func TestPublishNothingChanged(t *testing.T) {
	r := newRig(t)
	out, err := r.run(nil)
	if err != nil || out != "the tree is unchanged — nothing to propose\n" || r.remoteRef() != "none" || len(r.api.calls) != 0 {
		t.Errorf("err %v out %q", err, out)
	}
}

func TestPublishABump(t *testing.T) {
	r := newRig(t)
	r.change("v2")
	r.api.resp["GET /repos/o/r/pulls?state=open&per_page=100"] = `[{"number":3,"head":{"ref":"pkl-contracts-0.2.1","repo":{"full_name":"o/r"}}},{"number":4,"head":{"ref":"renovate/x","repo":{"full_name":"o/r"}}},{"number":5,"head":{"ref":"pkl-contracts-0.0.1","repo":{"full_name":"fork/r"}}},{"number":6,"head":{"ref":"pkl-contracts-0.1.0","repo":null}},{"number":7,"head":{"ref":"pkl-contracts-0.3.0","repo":{"full_name":"o/r"}}}]`
	out, err := r.run(nil)
	if err != nil {
		t.Fatal(err, out)
	}
	if r.remoteRef() == "none" {
		t.Error("branch not pushed")
	}
	if got := git(t, r.origin, "log", "-1", "--format=%an:%s", "refs/heads/pkl-contracts-0.3.0"); got != "fleet[bot]:chore(deps): update lib to v0.3.0" {
		t.Errorf("commit %q", got)
	}
	if body := git(t, r.origin, "log", "-1", "--format=%b", "refs/heads/pkl-contracts-0.3.0"); body != "Moves the dependencies on o/lib from v0.2.0 to v0.3.0, resolves PklProject.deps.json and regenerates what the repository generates." {
		t.Errorf("commit body %q", body)
	}
	wantBody := "Moves the Pkl contracts packages (o/lib) from v0.2.0 to **v0.3.0** in:\\n\\n- `.`\\n- `svc`\\n\\n`PklProject.deps.json` is resolved again, and the repository's `generate` recipe has run where it has one, so committed generated files follow the new contract.\\n\\nRelease notes: https://github.com/o/lib/releases/tag/v0.3.0\\n\\nNot breaking: auto-merge is armed and GitHub still holds the merge until every required check passes.\\n\\nOpened by the fleet's pkl job."
	wantCalls := []string{
		`POST /repos/o/r/labels {"name":"dependencies","color":"0366d6","description":"Dependency updates"}`,
		`GET /repos/o/r/pulls?state=open&head=o:pkl-contracts-0.3.0&per_page=100 `,
		`POST /repos/o/r/pulls {"title":"chore(deps): update lib to v0.3.0","body":"` + wantBody + `","head":"pkl-contracts-0.3.0","base":"main"}`,
		`POST /repos/o/r/issues/7/labels {"labels":["dependencies"]}`,
		`POST /graphql {"query":"mutation($id:ID!){enablePullRequestAutoMerge(input:{pullRequestId:$id,mergeMethod:REBASE}){clientMutationId}}","variables":{"id":"PR_node7"}}`,
		`GET /repos/o/r/pulls?state=open&per_page=100 `,
		`POST /repos/o/r/issues/3/comments {"body":"Superseded by #7 (v0.3.0)."}`,
		`PATCH /repos/o/r/pulls/3 {"state":"closed"}`,
		`DELETE /repos/o/r/git/refs/heads/pkl-contracts-0.2.1 `,
	}
	if strings.Join(r.api.calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(r.api.calls, "\n"), strings.Join(wantCalls, "\n"))
	}
	wantOut := "::add-mask::eC1hY2Nlc3MtdG9rZW46c2VjcmV0LXRva2Vu\npull request opened: https://example.test/o/r/pull/7\nauto-merge armed on #7\nclosed #3 (pkl-contracts-0.2.1), superseded\n"
	if out != wantOut {
		t.Errorf("log:\n%q\nwant:\n%q", out, wantOut)
	}
	if read(r.out) != "url=https://example.test/o/r/pull/7\n" || read(r.summary) != "- o/r: https://example.test/o/r/pull/7\n" {
		t.Errorf("output %q summary %q", read(r.out), read(r.summary))
	}
	// An unrelated, a fork's, a deleted fork's and the new pull request are not touched.
	for _, n := range []string{"issues/4", "issues/5", "issues/6", "pulls/4", "pulls/5", "pulls/6", "pulls/7 "} {
		if r.api.count(n) != 0 {
			t.Errorf("touched %s: %v", n, r.api.calls)
		}
	}
	// The token is in no git config of the clone, and not in the log.
	cfg, _ := os.ReadFile(filepath.Join(r.clone, ".git", "config"))
	if strings.Contains(string(cfg), "secret-token") || strings.Contains(out, "secret-token") {
		t.Error("the token leaked")
	}
}

func TestPublishRerun(t *testing.T) {
	r := newRig(t)
	r.change("v2")
	if _, err := r.run(nil); err != nil {
		t.Fatal(err)
	}
	sha := r.remoteRef()
	git(t, r.clone, "checkout", "-q", "main")
	git(t, r.clone, "branch", "-q", "-D", "pkl-contracts-0.3.0")
	r.change("v2")
	r.api.prFakes()
	r.api.resp["GET /repos/o/r/pulls?state=open&head=o:pkl-contracts-0.3.0&per_page=100"] = `[{"number":7,"html_url":"https://example.test/o/r/pull/7","node_id":"PR_node7"}]`
	out, err := r.run(nil)
	if err != nil || r.remoteRef() != sha || !strings.Contains(out, "pkl-contracts-0.3.0 already holds exactly this change — not pushed again\n") {
		t.Errorf("identical content is not pushed again: err %v out %q", err, out)
	}
	if r.api.count("POST /repos/o/r/pulls ") != 0 || r.api.count("PATCH /repos/o/r/pulls/7 ") != 1 || !strings.Contains(out, "pull request #7 for pkl-contracts-0.3.0 updated\n") {
		t.Errorf("the pull request is updated, not opened again: %v", r.api.calls)
	}
	git(t, r.clone, "checkout", "-q", "main")
	git(t, r.clone, "branch", "-q", "-D", "pkl-contracts-0.3.0")
	r.change("v3")
	if _, err := r.run(nil); err != nil || r.remoteRef() == sha {
		t.Errorf("different content is force-pushed: err %v", err)
	}
}

func TestPublishBreakingAndAutoMerge(t *testing.T) {
	r := newRig(t)
	r.change("v2")
	out, err := r.run(func(o *PublishOptions) { o.Breaking = "true" })
	if err != nil || r.api.count(`"labels":["dependencies","major"]`) != 1 || r.api.count("/graphql") != 0 || r.api.count("POST /repos/o/r/labels") != 2 ||
		!strings.Contains(out, "auto-merge not armed on #7\n") || !strings.Contains(r.api.calls[len(r.api.calls)-4], "This bump is **breaking**") && r.api.count("This bump is **breaking** (a new major, or a new minor below 1.0): it is not auto-merged. Read the release notes' breaking entries, fix what they name, and merge by hand.") != 1 {
		t.Errorf("breaking: err %v out %q calls %v", err, out, r.api.calls)
	}

	r = newRig(t)
	r.change("v2")
	out, err = r.run(func(o *PublishOptions) { o.AutoMerge = "false" })
	if err != nil || r.api.count("/graphql") != 0 || r.api.count("Not breaking, but auto-merge is not armed on this run: merge when the checks are green.") != 1 {
		t.Errorf("auto-merge off: err %v out %q", err, out)
	}

	r = newRig(t)
	r.change("v2")
	r.api.resp["POST /graphql"] = `{"errors":[{"message":"Auto merge is not allowed for this repository"}]}`
	out, err = r.run(nil)
	if err != nil || !strings.Contains(out, `::warning::o/r#7: auto-merge could not be armed: "Auto merge is not allowed for this repository"`+"\n") {
		t.Errorf("a refusal is a warning, not a failure: err %v out %q", err, out)
	}
}

func TestPublishAPIFailure(t *testing.T) {
	r := newRig(t)
	r.change("v2")
	delete(r.api.resp, "GET /repos/o/r/pulls?state=open&head=o:pkl-contracts-0.3.0&per_page=100")
	if _, err := r.run(nil); err == nil || strings.Contains(read(r.out), "url=") {
		t.Errorf("an unreadable pull request list fails the step: %v", err)
	}
}
