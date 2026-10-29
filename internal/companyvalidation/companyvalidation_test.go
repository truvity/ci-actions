package companyvalidation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	companyToken  = "company-token-never-print"
	dispatchToken = "dispatch-token-never-print"

	companyRepo  = "acme/company"
	platformRepo = "acme/platform"
	workflow     = "validation.yaml"
	checkName    = "platform-validation"
	appSlug      = "acme-validation-roster"
	prNumber     = "7"
	runIdentity  = "123"
	attempt      = "1"

	headSHA = "1111111111111111111111111111111111111111"
	baseSHA = "2222222222222222222222222222222222222222"
	runSHA  = "3333333333333333333333333333333333333333"
	runURL  = "https://example.test/acme/platform/actions/runs/99"

	requestID = companyRepo + ":" + prNumber + ":" + headSHA + ":" + baseSHA + ":" + runIdentity + ":" + attempt
)

func wantRequest() map[string]any {
	return map[string]any{
		"company":            "acme",
		"company_repository": companyRepo,
		"pull_request":       prNumber,
		"head_sha":           headSHA,
		"base_sha":           baseSHA,
		"request_id":         requestID,
	}
}

type checkSpec struct {
	id          int64
	name        string
	head        string
	external    string
	app         string
	status      string
	conclusion  string
	summary     string
	omitSummary bool
}

func goodCheck() checkSpec {
	return checkSpec{
		id: 10, name: checkName, head: headSHA, external: requestID, app: appSlug,
		status: "completed", conclusion: "success",
	}
}

type fake struct {
	prReads     int
	prStatus    int
	prErrorBody string
	prState     string
	prHeadSHA   string
	prBaseSHA   string
	prHeadRepo  string
	prBaseRepo  string
	prBaseRef   string
	prMoveHead  string
	prMoveBase  string

	dispatchStatus int
	dispatchBody   string
	dispatches     []map[string]any

	runStatus     string
	runStatuses   []string
	runConclusion string
	runHeadSHA    string
	runHTTPStatus int
	runReads      int

	checksStatus int
	checks       []checkSpec
	result       map[string]any
}

func (f *fake) reset() {
	*f = fake{
		prState: "open", prHeadSHA: headSHA, prBaseSHA: baseSHA,
		prHeadRepo: companyRepo, prBaseRepo: companyRepo, prBaseRef: "master",
		dispatchBody: fmt.Sprintf(`{"workflow_run_id":99}`),
		runStatus:    "completed", runConclusion: "success", runHeadSHA: runSHA,
		checks: []checkSpec{goodCheck()},
		result: map[string]any{
			"request":             wantRequest(),
			"candidate_sha":       headSHA,
			"platform_repository": platformRepo,
			"platform_revision":   runSHA,
		},
	}
}

func (f *fake) handler(t *testing.T) http.Handler {
	t.Helper()
	dispatchPath := "/repos/" + platformRepo + "/actions/workflows/" + workflow + "/dispatches"
	runPath := "/repos/" + platformRepo + "/actions/runs/99"
	checksPath := "/repos/" + companyRepo + "/commits/" + headSHA + "/check-runs"
	prPath := "/repos/" + companyRepo + "/pulls/" + prNumber
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == prPath:
			f.prReads++
			if f.prStatus != 0 {
				body := f.prErrorBody
				if body == "" {
					body = "refused"
				}
				http.Error(w, body, f.prStatus)
				return
			}
			head, base := f.prHeadSHA, f.prBaseSHA
			if f.prReads > 1 && f.prMoveHead != "" {
				head = f.prMoveHead
			}
			if f.prReads > 1 && f.prMoveBase != "" {
				base = f.prMoveBase
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": f.prState,
				"head":  map[string]any{"sha": head, "repo": map[string]any{"full_name": f.prHeadRepo}},
				"base":  map[string]any{"sha": base, "ref": f.prBaseRef, "repo": map[string]any{"full_name": f.prBaseRepo}},
			})

		case r.Method == http.MethodPost && r.URL.Path == dispatchPath:
			if got := r.Header.Get("Authorization"); got != "Bearer "+dispatchToken {
				t.Errorf("dispatch Authorization = %q", got)
			}
			if got := r.Header.Get("X-GitHub-Api-Version"); got != dispatchAPIVersion {
				t.Errorf("dispatch API version = %q", got)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode dispatch: %v", err)
			}
			if body["ref"] != "master" || body["return_run_details"] != true {
				t.Errorf("dispatch body = %#v", body)
			}
			if inputs, ok := body["inputs"].(map[string]any); !ok || !reflect.DeepEqual(inputs, wantRequest()) {
				t.Errorf("dispatch inputs = %#v, want %#v", body["inputs"], wantRequest())
			}
			f.dispatches = append(f.dispatches, body)
			if f.dispatchStatus != 0 {
				w.WriteHeader(f.dispatchStatus)
				fmt.Fprint(w, "forbidden")
				return
			}
			fmt.Fprint(w, f.dispatchBody)

		case r.Method == http.MethodGet && r.URL.Path == runPath:
			f.runReads++
			if f.runHTTPStatus != 0 {
				http.Error(w, "unavailable", f.runHTTPStatus)
				return
			}
			status := f.runStatus
			if len(f.runStatuses) > 0 {
				i := f.runReads - 1
				if i >= len(f.runStatuses) {
					i = len(f.runStatuses) - 1
				}
				status = f.runStatuses[i]
			}
			conclusion := f.runConclusion
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 99, "status": status, "conclusion": conclusion,
				"head_sha": f.runHeadSHA, "html_url": runURL,
			})

		case r.Method == http.MethodGet && r.URL.Path == checksPath:
			if got := r.Header.Get("Authorization"); got != "Bearer "+companyToken {
				t.Errorf("checks Authorization = %q", got)
			}
			if got := r.URL.Query().Get("check_name"); got != checkName {
				t.Errorf("check_name = %q", got)
			}
			if got := r.URL.Query().Get("filter"); got != "all" {
				t.Errorf("filter = %q", got)
			}
			if f.checksStatus != 0 {
				http.Error(w, "unavailable", f.checksStatus)
				return
			}
			runs := make([]map[string]any, 0, len(f.checks))
			for _, c := range f.checks {
				summary := c.summary
				if summary == "" {
					raw, _ := json.Marshal(f.result)
					summary = string(raw)
				}
				out := map[string]any{"summary": summary}
				entry := map[string]any{
					"id": c.id, "name": c.name, "head_sha": c.head, "external_id": c.external,
					"status": c.status, "conclusion": c.conclusion,
					"app": map[string]any{"slug": c.app},
				}
				if !c.omitSummary {
					entry["output"] = out
				}
				runs = append(runs, entry)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": runs})

		default:
			http.NotFound(w, r)
		}
	})
}

func baseOptions(apiURL, dir string) Options {
	return Options{
		Token: companyToken, DispatchToken: dispatchToken,
		Company: "acme", CompanyRepository: companyRepo, PlatformRepository: platformRepo,
		PlatformWorkflow: workflow, PlatformRef: "master",
		PullRequest: prNumber, HeadSHA: headSHA, BaseBranch: "master",
		CheckName: checkName, CheckAppSlug: appSlug,
		RunID: runIdentity, RunAttempt: attempt,
		WaitSeconds: 0, PollSeconds: 0, APIURL: apiURL,
		SummaryPath: filepath.Join(dir, "summary"), OutputPath: filepath.Join(dir, "output"),
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

func TestRunRefusesUnlessExact(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*fake)
		wantErr    bool
		noDispatch bool
	}{
		{name: "success"},
		{
			name: "run completes after polling",
			mutate: func(f *fake) {
				f.runStatuses = []string{"queued", "in_progress", "completed"}
			},
		},
		{name: "dispatch failure", mutate: func(f *fake) { f.dispatchStatus = http.StatusForbidden }, wantErr: true},
		{name: "missing run id", mutate: func(f *fake) { f.dispatchBody = `{}` }, wantErr: true},
		{name: "run failure", mutate: func(f *fake) { f.runConclusion = "failure" }, wantErr: true},
		{name: "run cancelled", mutate: func(f *fake) { f.runConclusion = "cancelled" }, wantErr: true},
		{name: "run skipped", mutate: func(f *fake) { f.runConclusion = "skipped" }, wantErr: true},
		{name: "run neutral", mutate: func(f *fake) { f.runConclusion = "neutral" }, wantErr: true},
		{name: "run timed out", mutate: func(f *fake) { f.runConclusion = "timed_out" }, wantErr: true},
		{name: "run action required", mutate: func(f *fake) { f.runConclusion = "action_required" }, wantErr: true},
		{name: "run stale", mutate: func(f *fake) { f.runConclusion = "stale" }, wantErr: true},
		{name: "run never completes", mutate: func(f *fake) { f.runStatus = "in_progress" }, wantErr: true},
		{name: "run read error", mutate: func(f *fake) { f.runHTTPStatus = http.StatusForbidden }, wantErr: true},
		{name: "missing check run", mutate: func(f *fake) { f.checks = nil }, wantErr: true},
		{name: "wrong check name", mutate: func(f *fake) { f.checks[0].name = "other" }, wantErr: true},
		{name: "wrong check head", mutate: func(f *fake) { f.checks[0].head = baseSHA }, wantErr: true},
		{name: "wrong check request", mutate: func(f *fake) { f.checks[0].external = "older-run" }, wantErr: true},
		{name: "wrong check app", mutate: func(f *fake) { f.checks[0].app = "company-authored" }, wantErr: true},
		{name: "check queued", mutate: func(f *fake) { f.checks[0].status = "queued" }, wantErr: true},
		{name: "check in progress", mutate: func(f *fake) { f.checks[0].status = "in_progress" }, wantErr: true},
		{name: "check failure", mutate: func(f *fake) { f.checks[0].conclusion = "failure" }, wantErr: true},
		{name: "check neutral", mutate: func(f *fake) { f.checks[0].conclusion = "neutral" }, wantErr: true},
		{name: "check skipped", mutate: func(f *fake) { f.checks[0].conclusion = "skipped" }, wantErr: true},
		{name: "check cancelled", mutate: func(f *fake) { f.checks[0].conclusion = "cancelled" }, wantErr: true},
		{name: "check timed out", mutate: func(f *fake) { f.checks[0].conclusion = "timed_out" }, wantErr: true},
		{name: "newest matching check decides", mutate: func(f *fake) {
			f.checks = []checkSpec{
				{id: 1, name: checkName, head: headSHA, external: requestID, app: appSlug, status: "completed", conclusion: "failure"},
				goodCheck(),
			}
		}},
		{name: "newest matching check is not green", mutate: func(f *fake) {
			old := goodCheck()
			old.id = 1
			bad := goodCheck()
			bad.id = 2
			bad.conclusion = "failure"
			f.checks = []checkSpec{old, bad}
		}, wantErr: true},
		{name: "summary missing", mutate: func(f *fake) { f.checks[0].omitSummary = true }, wantErr: true},
		{name: "summary malformed", mutate: func(f *fake) { f.checks[0].summary = "not JSON" }, wantErr: true},
		{name: "summary request missing", mutate: func(f *fake) { delete(f.result, "request") }, wantErr: true},
		{name: "wrong company", mutate: func(f *fake) { f.requestField("company", "other") }, wantErr: true},
		{name: "wrong company repository", mutate: func(f *fake) {
			f.requestField("company_repository", "acme/other")
		}, wantErr: true},
		{name: "wrong pull request", mutate: func(f *fake) { f.requestField("pull_request", "8") }, wantErr: true},
		{name: "wrong request head", mutate: func(f *fake) { f.requestField("head_sha", baseSHA) }, wantErr: true},
		{name: "wrong request base", mutate: func(f *fake) { f.requestField("base_sha", headSHA) }, wantErr: true},
		{name: "wrong request id", mutate: func(f *fake) { f.requestField("request_id", "other") }, wantErr: true},
		{name: "extra request field", mutate: func(f *fake) {
			f.result["request"].(map[string]any)["extra"] = "value"
		}, wantErr: true},
		{name: "wrong candidate", mutate: func(f *fake) { f.result["candidate_sha"] = baseSHA }, wantErr: true},
		{name: "wrong platform repository", mutate: func(f *fake) { f.result["platform_repository"] = "acme/other" }, wantErr: true},
		{name: "wrong platform revision", mutate: func(f *fake) { f.result["platform_revision"] = baseSHA }, wantErr: true},
		{name: "short platform revision", mutate: func(f *fake) { f.result["platform_revision"] = "abc" }, wantErr: true},
		{name: "fork", mutate: func(f *fake) { f.prHeadRepo = "foreign/company" }, wantErr: true, noDispatch: true},
		{name: "foreign base repository", mutate: func(f *fake) { f.prBaseRepo = "foreign/company" }, wantErr: true, noDispatch: true},
		{name: "closed pull request", mutate: func(f *fake) { f.prState = "closed" }, wantErr: true, noDispatch: true},
		{name: "other base branch", mutate: func(f *fake) { f.prBaseRef = "develop" }, wantErr: true, noDispatch: true},
		{name: "head is not the requested commit", mutate: func(f *fake) { f.prHeadSHA = baseSHA }, wantErr: true, noDispatch: true},
		{name: "base sha is not immutable", mutate: func(f *fake) { f.prBaseSHA = "not-a-sha" }, wantErr: true, noDispatch: true},
		{name: "pull request read error", mutate: func(f *fake) { f.prStatus = http.StatusForbidden }, wantErr: true, noDispatch: true},
		{name: "head moved after the result", mutate: func(f *fake) { f.prMoveHead = baseSHA }, wantErr: true},
		{name: "base moved after the result", mutate: func(f *fake) { f.prMoveBase = headSHA }, wantErr: true},
		{name: "check read error", mutate: func(f *fake) { f.checksStatus = http.StatusForbidden }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f fake
			f.reset()
			if tc.mutate != nil {
				tc.mutate(&f)
			}
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()

			dir := t.TempDir()
			o := baseOptions(srv.URL, dir)
			if tc.name == "run completes after polling" {
				o.WaitSeconds = 60
			}
			var out strings.Builder
			o.Out = &out

			err := Run(context.Background(), o)
			if tc.wantErr != errors.Is(err, ErrFailed) || (!tc.wantErr && err != nil) {
				t.Fatalf("err = %v, wantErr %v; output:\n%s", err, tc.wantErr, out.String())
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.noDispatch {
				if len(f.dispatches) != 0 {
					t.Errorf("dispatched %d times before the pull request check passed", len(f.dispatches))
				}
			} else if len(f.dispatches) != 1 {
				t.Errorf("dispatch count = %d, want 1", len(f.dispatches))
			}
			if tc.wantErr {
				if _, statErr := os.Stat(filepath.Join(dir, "summary")); !os.IsNotExist(statErr) {
					t.Errorf("a refused run wrote the step summary")
				}
				if _, statErr := os.Stat(filepath.Join(dir, "output")); !os.IsNotExist(statErr) {
					t.Errorf("a refused run wrote outputs")
				}
			}
			if !tc.wantErr {
				summary, readErr := os.ReadFile(filepath.Join(dir, "summary"))
				if readErr != nil {
					t.Fatalf("summary: %v", readErr)
				}
				var got result
				if err := json.Unmarshal([]byte(strings.TrimSpace(string(summary))), &got); err != nil {
					t.Errorf("summary is not the platform's JSON: %v", err)
				}
				outputs, readErr := os.ReadFile(filepath.Join(dir, "output"))
				if readErr != nil {
					t.Fatalf("outputs: %v", readErr)
				}
				for _, want := range []string{
					"run-id=99\n", "run-url=" + runURL + "\n", "request-id=" + requestID + "\n",
				} {
					if !strings.Contains(string(outputs), want) {
						t.Errorf("outputs %q do not contain %q", outputs, want)
					}
				}
			}
			for _, token := range []string{companyToken, dispatchToken} {
				if strings.Contains(out.String(), token) {
					t.Errorf("output leaked a token: %q", out.String())
				}
			}
		})
	}
}

// requestField edits the request the platform echoes back.
func (f *fake) requestField(name, value string) {
	f.result["request"].(map[string]any)[name] = value
}

func TestRunRejectsMalformedInputsBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Options)
	}{
		{"missing token", func(o *Options) { o.Token = "" }},
		{"missing dispatch token", func(o *Options) { o.DispatchToken = "" }},
		{"missing company", func(o *Options) { o.Company = "" }},
		{"bad company repository", func(o *Options) { o.CompanyRepository = "acme" }},
		{"traversal repository", func(o *Options) { o.CompanyRepository = "../acme" }},
		{"bad platform repository", func(o *Options) { o.PlatformRepository = "acme/" }},
		{"missing workflow", func(o *Options) { o.PlatformWorkflow = "" }},
		{"bad platform ref", func(o *Options) { o.PlatformRef = "master ref" }},
		{"zero pull request", func(o *Options) { o.PullRequest = "0" }},
		{"non-numeric pull request", func(o *Options) { o.PullRequest = "7x" }},
		{"upper-case sha", func(o *Options) { o.HeadSHA = strings.Repeat("A", 40) }},
		{"short sha", func(o *Options) { o.HeadSHA = "abc" }},
		{"empty base branch", func(o *Options) { o.BaseBranch = "" }},
		{"empty check name", func(o *Options) { o.CheckName = "" }},
		{"bad app slug", func(o *Options) { o.CheckAppSlug = "not an app" }},
		{"zero run id", func(o *Options) { o.RunID = "0" }},
		{"non-numeric attempt", func(o *Options) { o.RunAttempt = "one" }},
		{"negative wait", func(o *Options) { o.WaitSeconds = -1 }},
		{"negative poll", func(o *Options) { o.PollSeconds = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f fake
			f.reset()
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()
			o := baseOptions(srv.URL, t.TempDir())
			tc.mutate(&o)
			var out strings.Builder
			o.Out = &out
			if err := Run(context.Background(), o); !errors.Is(err, ErrFailed) {
				t.Fatalf("err = %v, want ErrFailed; output:\n%s", err, out.String())
			}
			if f.prReads != 0 || len(f.dispatches) != 0 {
				t.Errorf("made a request for invalid input: prReads=%d dispatches=%d", f.prReads, len(f.dispatches))
			}
		})
	}
}

func TestRunRedactsBothTokens(t *testing.T) {
	var f fake
	f.reset()
	f.prStatus = http.StatusForbidden
	f.prErrorBody = companyToken + " " + dispatchToken
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	var out strings.Builder
	o := baseOptions(srv.URL, t.TempDir())
	o.Out = &out
	err := Run(context.Background(), o)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("err = %v", err)
	}
	text := out.String() + fmt.Sprint(err)
	for _, token := range []string{companyToken, dispatchToken} {
		if strings.Contains(text, token) {
			t.Errorf("a token reached the output: %q", text)
		}
	}
	if !strings.Contains(text, "***") {
		t.Errorf("the refusal does not show a redacted token: %q", text)
	}
}
