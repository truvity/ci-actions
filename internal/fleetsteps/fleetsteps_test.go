package fleetsteps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type fake struct {
	t     *testing.T
	srv   *httptest.Server
	resp  map[string]string // "METHOD /path?query" -> body
	decs  map[int]string    // GraphQL review decision by PR number ("" is null)
	calls []string
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, resp: map[string]string{}, decs: map[int]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, fmt.Sprintf("%s %s %s | auth=%s", r.Method, r.URL.RequestURI(), b, r.Header.Get("Authorization")))
		if r.URL.Path == "/graphql" {
			m := regexp.MustCompile(`"n":(\d+)`).FindSubmatch(b)
			n := 0
			fmt.Sscan(string(m[1]), &n)
			d := "null"
			if v := f.decs[n]; v != "" {
				d = `"` + v + `"`
			}
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"reviewDecision":%s}}}}`, d)
			return
		}
		if body, ok := f.resp[r.Method+" "+r.URL.RequestURI()]; ok {
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

func (f *fake) count(sub string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func read(p string) string { b, _ := os.ReadFile(p); return string(b) }

func common(t *testing.T) (Common, *bytes.Buffer, string, string) {
	d := t.TempDir()
	var b bytes.Buffer
	return Common{Out: &b, Output: filepath.Join(d, "out"), Summary: filepath.Join(d, "sum")}, &b, filepath.Join(d, "out"), filepath.Join(d, "sum")
}

func TestCommitAuthor(t *testing.T) {
	f := newFake(t)
	f.resp["GET /users/fleet%5Bbot%5D"] = `{"id":12345}`
	f.resp["GET /users/a%20b%2Fc"] = `{"id":7}`
	for _, tc := range []struct{ login, email, want, out string }{
		{"fleet[bot]", "", "git-email=12345+fleet[bot]@users.noreply.github.com\n", "commits as fleet[bot] <12345+fleet[bot]@users.noreply.github.com>\n"},
		{"fleet[bot]", "x@y", "git-email=x@y\n", "commits as fleet[bot] <x@y>\n"},
		{"a b/c", "", "git-email=7+a b/c@users.noreply.github.com\n", "commits as a b/c <7+a b/c@users.noreply.github.com>\n"},
	} {
		c, log, out, _ := common(t)
		if err := CommitAuthor(context.Background(), AuthorOptions{Common: c, Token: "t", UserLogin: tc.login, Email: tc.email, API: f.srv.URL}); err != nil || read(out) != tc.want || log.String() != tc.out {
			t.Errorf("%+v: err %v out %q log %q", tc, err, read(out), log.String())
		}
	}
	if f.count("GET /users/") != 2 || f.count("auth=Bearer t") != 2 {
		t.Errorf("a given email makes no lookup: %v", f.calls)
	}
	c, _, out, _ := common(t)
	if err := CommitAuthor(context.Background(), AuthorOptions{Common: c, Token: "t", UserLogin: "nobody", API: f.srv.URL}); err == nil || read(out) != "" {
		t.Errorf("an unknown user fails the step: %v", err)
	}
}

func TestParitySettings(t *testing.T) {
	f := newFake(t)
	set := func(repo, content string) {
		f.resp["GET /repos/"+repo] = `{"default_branch":"main"}`
		if content != "" {
			f.resp["GET /repos/"+repo+"/contents/.devbox-parity.json?ref=main"] = fmt.Sprintf(`{"content":%q}`, base64.StdEncoding.EncodeToString([]byte(content))[:4]+"\n"+base64.StdEncoding.EncodeToString([]byte(content))[4:])
		}
	}
	for i, tc := range []struct{ content, mode, out, log string }{
		{`{"mode":"align","module-dirs":["a","b/c"]}`, "auto", "base=main\nmodule-dirs=[\"a\",\"b/c\"]\nmode=align\n", "o/r0: mode align from .devbox-parity.json\no/r0: default branch main, module-dirs [\"a\",\"b/c\"], mode align\n"},
		{`{"mode":"full"}`, "auto", "base=main\nmodule-dirs=[]\nmode=auto\n", "::warning::o/r1: .devbox-parity.json mode 'full' is not auto or align — using this run's mode auto\no/r1: default branch main, module-dirs [], mode auto\n"},
		{`{"mode":5}`, "align", "base=main\nmodule-dirs=[]\nmode=align\n", "::warning::o/r2: .devbox-parity.json mode '5' is not auto or align — using this run's mode align\no/r2: default branch main, module-dirs [], mode align\n"},
		{`{}`, "auto", "base=main\nmodule-dirs=[]\nmode=auto\n", "o/r3: default branch main, module-dirs [], mode auto\n"},
		{`not json`, "auto", "base=main\nmodule-dirs=[]\nmode=auto\n", "o/r4: default branch main, module-dirs [], mode auto\n"},
		{`[1,2]`, "auto", "base=main\nmodule-dirs=[]\nmode=auto\n", "o/r5: default branch main, module-dirs [], mode auto\n"},
		{`{"module-dirs":null,"mode":"auto"}`, "align", "base=main\nmodule-dirs=[]\nmode=auto\n", "o/r6: mode auto from .devbox-parity.json\no/r6: default branch main, module-dirs [], mode auto\n"},
		{`{"module-dirs":["é<>&"]}`, "auto", "base=main\nmodule-dirs=[\"é<>&\"]\nmode=auto\n", "o/r7: default branch main, module-dirs [\"é<>&\"], mode auto\n"},
		{``, "auto", "base=main\nmodule-dirs=[]\nmode=auto\n", "o/r8: default branch main, module-dirs [], mode auto\n"}, // no file: a 404
	} {
		repo := fmt.Sprintf("o/r%d", i)
		set(repo, tc.content)
		c, log, out, _ := common(t)
		if err := ParitySettings(context.Background(), SettingsOptions{Common: c, Token: "t", Repo: repo, API: f.srv.URL, RunMode: tc.mode}); err != nil || read(out) != tc.out || log.String() != tc.log {
			t.Errorf("%q: err %v out %q log %q", tc.content, err, read(out), log.String())
		}
	}
	c, _, out, _ := common(t)
	if err := ParitySettings(context.Background(), SettingsOptions{Common: c, Token: "t", Repo: "o/gone", API: f.srv.URL, RunMode: "auto"}); err == nil || read(out) != "" {
		t.Errorf("an unreadable repository fails the step: %v", err)
	}
	if f.count("X-GitHub") != 0 {
		t.Log("headers are checked in the shell comparison")
	}
}

func pr(n int, login, typ string, labels ...string) string {
	ls := []map[string]string{}
	for _, l := range labels {
		ls = append(ls, map[string]string{"name": l})
	}
	b, _ := json.Marshal(map[string]any{"number": n, "user": map[string]string{"login": login, "type": typ}, "labels": ls})
	return string(b)
}

func TestApproveRenovate(t *testing.T) {
	f := newFake(t)
	f.resp["GET /repos/o/r/pulls?state=open&per_page=100"] = "[" + strings.Join([]string{
		pr(1, "renovate[bot]", "Bot", "dependencies"), pr(2, "renovate[bot]", "Bot", "major"), pr(3, "alice", "User"), pr(4, "renovate[bot]", "Bot"),
		pr(5, "Renovate-x", "Bot"), pr(6, "renovate[bot]", "Bot", "dependencies", "major"), pr(7, "renovate[bot]", "Bot"), pr(8, "renovate-fork", "User"), pr(9, "renovate[bot]", "Bot"),
	}, ",") + "]"
	f.decs = map[int]string{1: "REVIEW_REQUIRED", 4: "APPROVED", 7: "REVIEW_REQUIRED", 9: ""}
	f.resp["GET /repos/o/r/pulls/1/reviews"] = `[{"state":"COMMENTED","user":{"login":"x"}},{"state":"APPROVED","user":{"login":"human"}}]`
	f.resp["GET /repos/o/r/pulls/7/reviews"] = `[{"state":"APPROVED","user":{"login":"ci[bot]"}}]`
	c, log, _, sum := common(t)
	if err := ApproveRenovate(context.Background(), ApproveOptions{Common: c, Token: "t", Repo: "o/r", API: f.srv.URL}); err != nil {
		t.Fatal(err)
	}
	want := "#1: approved\n#2: major — left for a human\n#4: review APPROVED — nothing to add\n#6: major — left for a human\n#7: already approved by a bot\n#9: review NONE — nothing to add\n"
	if log.String() != want || read(sum) != "approved 1 renovate PR(s)\n" {
		t.Errorf("log:\n%q\nwant:\n%q (summary %q)", log.String(), want, read(sum))
	}
	if f.count(`POST /repos/o/r/pulls/1/reviews {"event":"APPROVE","body":"Non-major dependency update from renovate. Approved by the fleet job so auto-merge can proceed; GitHub still holds the merge until every required check passes."}`) != 1 || f.count("/reviews {") != 1 {
		t.Errorf("exactly one approval: %v", f.calls)
	}
	// Only bots named renovate are looked at; the GraphQL query carries owner, name and number.
	if f.count(`{"query":"query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewDecision}}}","variables":{"o":"o","r":"r","n":1}}`) != 1 || f.count("/graphql") != 4 {
		t.Errorf("graphql: %v", f.calls)
	}
	for _, p := range []string{"pulls/3/", "pulls/5/", "pulls/8/"} {
		if f.count(p) != 0 {
			t.Errorf("touched %s", p)
		}
	}
	c, log, _, sum = common(t)
	f.resp["GET /repos/o/q/pulls?state=open&per_page=100"] = "[]"
	if err := ApproveRenovate(context.Background(), ApproveOptions{Common: c, Token: "t", Repo: "o/q", API: f.srv.URL}); err != nil || log.String() != "" || read(sum) != "approved 0 renovate PR(s)\n" {
		t.Errorf("nothing open: %v %q %q", err, log.String(), read(sum))
	}
	c, _, _, sum = common(t)
	if err := ApproveRenovate(context.Background(), ApproveOptions{Common: c, Token: "t", Repo: "o/none", API: f.srv.URL}); err == nil || read(sum) != "" {
		t.Errorf("an unreadable list fails the step: %v", err)
	}
}

func jwt(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	return "h." + strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=") + ".sig"
}

func TestOIDCClaims(t *testing.T) {
	f := newFake(t)
	tok := jwt(map[string]any{"repository": "o/r", "ref": "refs/heads/main", "ref_type": "branch", "event_name": "push", "workflow_ref": "o/r/.github/workflows/x.yaml@refs/heads/main",
		"job_workflow_ref": "o/c/.github/workflows/y.yaml@v1", "sha": "abc", "extra": "not printed", "iss": "x"})
	f.resp["GET /oidc?x=1&audience=debug-oidc-claims"] = fmt.Sprintf(`{"value":%q}`, tok)
	c, log, _, _ := common(t)
	if err := OIDCClaims(context.Background(), ClaimsOptions{Common: c, RequestURL: f.srv.URL + "/oidc?x=1", RequestToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	want := "::add-mask::" + tok + "\n{\n  \"repository\": \"o/r\",\n  \"ref\": \"refs/heads/main\",\n  \"ref_type\": \"branch\",\n  \"event_name\": \"push\",\n  \"workflow_ref\": \"o/r/.github/workflows/x.yaml@refs/heads/main\",\n  \"job_workflow_ref\": \"o/c/.github/workflows/y.yaml@v1\",\n  \"sha\": \"abc\"\n}\n"
	if log.String() != want || f.count("auth=Bearer tok") != 1 {
		t.Errorf("log %q", log.String())
	}
	// A missing claim is null, a non-ASCII string is printed as is, a number as it was.
	f.resp["GET /oidc?x=1&audience=debug-oidc-claims"] = fmt.Sprintf(`{"value":%q}`, jwt(map[string]any{"repository": "o/é<>&", "sha": 1.5}))
	c, log, _, _ = common(t)
	_ = OIDCClaims(context.Background(), ClaimsOptions{Common: c, RequestURL: f.srv.URL + "/oidc?x=1", RequestToken: "tok"})
	if !strings.Contains(log.String(), "  \"repository\": \"o/é<>&\",\n  \"ref\": null,") || !strings.HasSuffix(log.String(), "  \"sha\": 1.5\n}\n") {
		t.Errorf("log %q", log.String())
	}
	c, log, _, _ = common(t)
	if err := OIDCClaims(context.Background(), ClaimsOptions{Common: c}); err != nil || log.String() != "::warning::no id-token permission in this job; nothing to print\n" {
		t.Errorf("no id-token permission: %v %q", err, log.String())
	}
}

func TestAvailableMajors(t *testing.T) {
	c, log, _, sum := common(t)
	if err := AvailableMajors(context.Background(), MajorsOptions{Common: c, Dir: "testdata/majors"}); err != nil {
		t.Fatal(err)
	}
	// The golden is the output of the step's jq program on these reports.
	want := read("testdata/majors/golden.md")
	if log.String() != want || read(sum) != want {
		t.Errorf("log:\n%s\nwant:\n%s", log.String(), want)
	}
	c, log, _, sum = common(t)
	empty := t.TempDir()
	if err := AvailableMajors(context.Background(), MajorsOptions{Common: c, Dir: empty}); err != nil || log.String() != "no reports — every repository job failed before renovate wrote one\n" || read(sum) != log.String() {
		t.Errorf("no reports: %v %q", err, log.String())
	}
	// Nothing major: the table says so.
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "reports"), 0o755)
	os.WriteFile(filepath.Join(d, "reports", "r.json"), []byte(`{"repositories":{"o/a":{"packageFiles":{"npm":[{"deps":[{"depName":"x","updates":[{"updateType":"minor","newVersion":"1.1.0"}]},{"depName":"y"}]}]}}}}`), 0o644)
	c, log, _, _ = common(t)
	if err := AvailableMajors(context.Background(), MajorsOptions{Common: c, Dir: d}); err != nil || log.String() != "## Available majors\n\nNone: every enrolled repository is on the newest major of everything it uses.\n" {
		t.Errorf("none: %v %q", err, log.String())
	}
}

func TestCmpMajor(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"v10.0.0", "9.9.9", 1}, {"2.0.0", "v2.0.0", 0}, {"1.10", "1.9", 1}, {"4", "3.9.9", 1}, {"1.2", "1.2.0", -1}, {"?", "1", -1}} {
		if got := cmpMajor(tc.a, tc.b); (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
			t.Errorf("cmpMajor(%q,%q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}
