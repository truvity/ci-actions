package fleetdiscover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const token = "ghs_never_print_this"

type reply struct {
	status int
	body   any
}

func rsc(contexts ...string) map[string]any {
	var checks []map[string]string
	for _, c := range contexts {
		checks = append(checks, map[string]string{"context": c})
	}
	return map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": checks}}
}

var (
	notProtected = reply{404, map[string]string{"message": "Branch not protected"}}
	hidden       = reply{404, map[string]string{"message": "Not Found"}}
	forbidden    = reply{403, map[string]string{"message": "Resource not accessible by integration"}}
)

// gateCase is what the stub answers for one repository: the effective-rules
// endpoint, the classic protection endpoint, and what GraphQL's
// refUpdateRule answers when the reader falls back to it (nil for no rule, a
// slice of contexts, or "403").
type gateCase struct {
	rules      reply
	protection reply
	classic    any
}

func protection(contexts ...string) reply {
	return reply{200, map[string]any{"required_status_checks": map[string]any{"contexts": contexts}}}
}

// The shapes the required-check rule must survive: either source alone, both,
// neither, a ruleset with rules but no status-check rule, a protection object
// with no required checks, a 404 that means "not protected", a 404 that means
// "not yours to read", a 403.
var gateCases = map[string]gateCase{
	"classic-only":         {reply{200, []any{}}, protection("check"), nil},
	"ruleset-only":         {reply{200, []any{rsc("check")}}, notProtected, nil},
	"both":                 {reply{200, []any{rsc("check")}}, protection("check", "integration"), []string{"check"}},
	"neither":              {reply{200, []any{}}, notProtected, nil},
	"ruleset-no-checks":    {reply{200, []any{map[string]any{"type": "pull_request", "parameters": map[string]any{}}}}, notProtected, nil},
	"protection-no-checks": {reply{200, []any{}}, reply{200, map[string]any{"enforce_admins": map[string]any{"enabled": true}}}, nil},
	"protection-hidden":    {reply{200, []any{}}, hidden, []string{"check"}},
	"protection-forbidden": {reply{200, []any{}}, forbidden, []string{"check"}},
	"rules-404":            {hidden, notProtected, nil},
	"rules-403":            {forbidden, notProtected, nil},
	"classic-unreadable":   {reply{200, []any{}}, forbidden, "403"},
	"rules-not-an-array":   {reply{200, map[string]any{"oops": true}}, notProtected, nil},
	"graphql-errors":       {reply{200, []any{}}, forbidden, "errors"},
	"graphql-null-repo":    {reply{200, []any{}}, forbidden, "nullrepo"},
}

type stub struct {
	repos []map[string]any
	gates map[string]gateCase
	files map[string]bool // owner/name/path present
	srv   *httptest.Server
	auth  string
}

func newStub(t *testing.T, repos []map[string]any, gates map[string]gateCase, files map[string]bool) *stub {
	t.Helper()
	if repos == nil {
		repos = []map[string]any{}
	}
	s := &stub{repos: repos, gates: gates, files: files}
	send := func(w http.ResponseWriter, r reply) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.status)
		_ = json.NewEncoder(w).Encode(r.body)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.auth = r.Header.Get("Authorization")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.URL.Path == "/installation/repositories":
			page := 1
			fmt.Sscan(r.URL.Query().Get("page"), &page)
			lo, hi := (page-1)*100, page*100
			if lo > len(s.repos) {
				lo = len(s.repos)
			}
			if hi > len(s.repos) {
				hi = len(s.repos)
			}
			send(w, reply{200, map[string]any{"repositories": s.repos[lo:hi]}})
		case parts[0] == "repos" && len(parts) == 6 && parts[3] == "rules":
			send(w, s.gates[parts[2]].rules)
		case parts[0] == "repos" && len(parts) == 6 && parts[3] == "branches" && parts[5] == "protection":
			send(w, s.gates[parts[2]].protection)
		case parts[0] == "repos" && len(parts) >= 5 && parts[3] == "contents":
			if s.files[strings.Join(parts[1:3], "/")+"/"+strings.Join(parts[4:], "/")] {
				send(w, reply{200, map[string]string{"name": parts[4]}})
				return
			}
			send(w, hidden)
		case r.URL.Path == "/graphql":
			var q struct {
				Variables struct{ R string } `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			c := s.gates[q.Variables.R].classic
			switch c {
			case "403":
				send(w, forbidden)
			case "errors":
				send(w, reply{200, map[string]any{"errors": []string{"boom"}, "data": map[string]any{"repository": nil}}})
			case "nullrepo":
				send(w, reply{200, map[string]any{"data": map[string]any{"repository": nil}}})
			default:
				var rule any
				if c != nil {
					rule = map[string]any{"requiredStatusCheckContexts": c}
				}
				send(w, reply{200, map[string]any{"data": map[string]any{"repository": map[string]any{"defaultBranchRef": map[string]any{"refUpdateRule": rule}}}}})
			}
		default:
			send(w, hidden)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func repoObj(full, vis string, archived bool) map[string]any {
	return map[string]any{"full_name": full, "visibility": vis, "archived": archived, "default_branch": "master"}
}

type result struct {
	out, summary, output string
	err                  error
}

func run(t *testing.T, s *stub, o Options) result {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	o.Out = &b
	o.API = s.srv.URL
	o.Token = token
	o.Summary = filepath.Join(dir, "summary")
	o.Output = filepath.Join(dir, "output")
	if o.Estate == "" {
		o.Estate = "all"
	}
	err := Run(context.Background(), o)
	sum, _ := os.ReadFile(o.Summary)
	out, _ := os.ReadFile(o.Output)
	r := result{b.String(), string(sum), string(out), err}
	if strings.Contains(r.out+r.summary+r.output, token) {
		t.Error("the token must never be printed")
	}
	if s.auth != "Bearer "+token && s.auth != "" {
		t.Errorf("Authorization = %q", s.auth)
	}
	return r
}

func (r result) get(key string) string {
	for _, l := range strings.Split(r.output, "\n") {
		if strings.HasPrefix(l, key+"=") {
			return strings.TrimPrefix(l, key+"=")
		}
	}
	return ""
}

func (r result) keptList() []string {
	var l []string
	_ = json.Unmarshal([]byte(r.get("repositories")), &l)
	return l
}

func has(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestRequiredCheckRule(t *testing.T) {
	var repos []map[string]any
	files := map[string]bool{}
	for name := range gateCases {
		repos = append(repos, repoObj("stub/"+name, "private", false))
		files["stub/"+name+"/devbox.json"] = true
	}
	s := newStub(t, repos, gateCases, files)
	r := run(t, s, Options{RequireCheck: "true", RequireFile: "devbox.json"})
	if r.err != nil {
		t.Fatal(r.err, r.out)
	}
	kept := r.keptList()
	none := "no required status check on master (rulesets: none, classic protection: none)"

	for _, name := range []string{"classic-only", "ruleset-only", "both", "protection-hidden", "protection-forbidden"} {
		if !has(kept, "stub/"+name) {
			t.Errorf("%s is processed", name)
		}
		if strings.Contains(r.summary, "- stub/"+name+" — ") {
			t.Errorf("%s needed no undecided note", name)
		}
	}
	for _, name := range []string{"neither", "ruleset-no-checks", "protection-no-checks"} {
		if has(kept, "stub/"+name) {
			t.Errorf("%s is skipped", name)
		}
		if !strings.Contains(r.summary, "| stub/"+name+" | "+none+" |\n") {
			t.Errorf("%s says why:\n%s", name, r.summary)
		}
	}
	for name, detail := range map[string]string{
		"rules-404":          "rulesets: could not read, classic protection: none",
		"rules-403":          "rulesets: could not read, classic protection: none",
		"rules-not-an-array": "rulesets: could not read, classic protection: none",
		"classic-unreadable": "rulesets: none, classic protection: could not read",
		"graphql-errors":     "rulesets: none, classic protection: could not read",
		"graphql-null-repo":  "rulesets: none, classic protection: could not read",
	} {
		if !has(kept, "stub/"+name) {
			t.Errorf("%s is processed although the rule is undecided", name)
		}
		if !strings.Contains(r.summary, "- stub/"+name+" — "+detail+"\n") {
			t.Errorf("%s is reported as undecided:\n%s", name, r.summary)
		}
		if !strings.Contains(r.out, "::warning::stub/"+name+": could not decide the required-check rule ("+detail+") — processing it anyway\n") {
			t.Errorf("%s warns on the run:\n%s", name, r.out)
		}
	}
	if got := r.get("count"); got != fmt.Sprint(len(gateCases)-3) {
		t.Errorf("count = %s", got)
	}
	if r.out[strings.Index(r.out, "## fleet-discover"):] != r.summary {
		t.Error("the summary goes to the log and to the summary file, identically")
	}
}

func TestRules(t *testing.T) {
	gates := map[string]gateCase{}
	repos := []map[string]any{
		repoObj("acme/b-pub", "public", false),
		repoObj("acme/a-priv", "private", false),
		repoObj("acme/old", "private", true),
		repoObj("acme/nofile", "private", false),
		repoObj("acme/ungated", "private", false),
	}
	for _, r := range repos {
		n := strings.TrimPrefix(r["full_name"].(string), "acme/")
		gates[n] = gateCase{reply{200, []any{rsc("check")}}, notProtected, nil}
	}
	gates["ungated"] = gateCase{reply{200, []any{}}, notProtected, nil}
	files := map[string]bool{"acme/b-pub/renovate.json": true, "acme/a-priv/renovate.json": true, "acme/old/renovate.json": true, "acme/ungated/renovate.json": true}
	s := newStub(t, repos, gates, files)

	for _, tc := range []struct {
		name string
		o    Options
		want string // the repositories output
		why  map[string]string
	}{
		{"estate private", Options{Estate: "private", RequireCheck: "true", RequireFile: "renovate.json"},
			`["acme/a-priv"]`, map[string]string{"acme/b-pub": "visibility public, estate private", "acme/old": "archived", "acme/nofile": "no renovate.json", "acme/ungated": "no required status check on master (rulesets: none, classic protection: none)"}},
		{"estate public", Options{Estate: "public", RequireFile: "renovate.json"},
			`["acme/b-pub"]`, map[string]string{"acme/a-priv": "visibility private, estate public"}},
		{"no check rule, no file rule", Options{RequireCheck: "false"},
			`["acme/a-priv","acme/b-pub","acme/nofile","acme/ungated"]`, map[string]string{"acme/old": "archived"}},
		{"filter", Options{Filter: "^acme/a-"}, `["acme/a-priv"]`, map[string]string{"acme/b-pub": "does not match filter"}},
		{"enrolled", Options{Enrolled: `["b-pub","gone","gone2"]`, RequireCheck: "false"}, `["acme/b-pub"]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, s, tc.o)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if got := r.get("repositories"); got != tc.want {
				t.Errorf("repositories = %s, want %s", got, tc.want)
			}
			for repo, why := range tc.why {
				if !strings.Contains(r.summary, "| "+repo+" | "+why+" |\n") {
					t.Errorf("%s: want reason %q in\n%s", repo, why, r.summary)
				}
			}
		})
	}

	r := run(t, s, Options{Enrolled: `["b-pub","gone2","gone"]`, RequireCheck: "false"})
	if r.get("missing") != `["gone","gone2"]` {
		t.Errorf("missing = %s", r.get("missing"))
	}
	for _, n := range []string{"gone", "gone2"} {
		if !strings.Contains(r.out, "::error::"+n+" is enrolled but the App cannot see it — install the App on it, or remove it from the list\n") {
			t.Errorf("no error annotation for %s:\n%s", n, r.out)
		}
	}
	if !strings.Contains(r.summary, "**Enrolled but not reachable by the App:** gone, gone2\n") {
		t.Errorf("summary:\n%s", r.summary)
	}
	if strings.Contains(r.summary, "not enrolled") {
		t.Errorf("'not enrolled' rows would bury the rest:\n%s", r.summary)
	}
	if !strings.HasPrefix(r.summary, "## fleet-discover — estate `all`\n\n**1** repositories to process out of 5 the App can see.\n\n- acme/b-pub\n\n") {
		t.Errorf("summary head:\n%s", r.summary)
	}
}

func TestInputErrors(t *testing.T) {
	s := newStub(t, nil, nil, nil)
	r := run(t, s, Options{Estate: "everything"})
	if r.err == nil || r.out != "::error::estate must be public, private or all (got 'everything')\n" {
		t.Errorf("%v %q", r.err, r.out)
	}
	for _, bad := range []string{`{"a":1}`, `[]`, `nope`} {
		r := run(t, s, Options{Enrolled: bad})
		if r.err == nil || r.out != "::error::repositories is not a non-empty JSON array of names: "+bad+"\n" {
			t.Errorf("%s: %v %q", bad, r.err, r.out)
		}
	}
	r = run(t, s, Options{Filter: "(", RequireCheck: "false"})
	if r.err != nil || !strings.Contains(r.out, "::warning::filter is not a valid RE2 expression") {
		t.Errorf("an invalid filter is said once: %v %q", r.err, r.out)
	}
	if r.get("repositories") != "[]" || r.get("count") != "0" || r.get("missing") != "[]" {
		t.Errorf("empty outputs: %q", r.output)
	}
}

func TestPaginationAndFailure(t *testing.T) {
	var repos []map[string]any
	gates := map[string]gateCase{}
	for i := 0; i < 230; i++ {
		n := fmt.Sprintf("r%03d", i)
		repos = append(repos, repoObj("acme/"+n, "private", false))
		gates[n] = gateCase{reply{200, []any{rsc("c")}}, notProtected, nil}
	}
	s := newStub(t, repos, gates, nil)
	r := run(t, s, Options{RequireCheck: "true"})
	if r.err != nil || r.get("count") != "230" {
		t.Errorf("every page is read: %v count=%s", r.err, r.get("count"))
	}
	// a token the API refuses fails the step, and never leaks
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credentials: "+r.Header.Get("Authorization"), 401)
	}))
	defer bad.Close()
	var b strings.Builder
	err := Run(context.Background(), Options{Token: token, Estate: "all", API: bad.URL, Out: &b})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("err = %v", err)
	}
}
