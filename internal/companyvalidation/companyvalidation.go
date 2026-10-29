// Package companyvalidation is the request/dispatch/wait/verify half of a
// company repository's delivery gate: dispatch a private platform validation
// workflow in another repository, wait for that exact run, and accept only the
// check run the platform explicitly correlated with the request.
//
// The company workflow is the requester and supplies only data. Nothing here
// checks out or executes company or platform code: the platform is trusted to
// do the rendering and validation, and this command's whole job is to prove
// that the answer it read belongs to this exact candidate and this exact
// dispatch. Every step fails closed. The token is only ever put in an
// Authorization header, and every message is redacted before it is printed.
package companyvalidation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/truvity/ci-actions/internal/ghapi"
)

// ErrFailed marks a refusal whose ::error:: line has already been written.
var ErrFailed = errors.New("company-validation: failed")

// dispatchAPIVersion is required for `return_run_details` on workflow
// dispatch: the response carries the workflow_run_id of the run it created.
const dispatchAPIVersion = "2026-03-10"

var (
	repositoryRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	positiveIntRe  = regexp.MustCompile(`^[1-9][0-9]*$`)
	shaRe          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	appSlugRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	refRe          = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	untrustedChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// Options are the action's inputs and the runner's files.
type Options struct {
	Token         string // company token: checks: read, pull-requests: read
	DispatchToken string // platform token: actions: write

	Company            string // fleet/company name, an input data value
	CompanyRepository  string // owner/name
	PlatformRepository string // owner/name
	PlatformWorkflow   string // workflow filename, name or ID
	PlatformRef        string // ref the workflow is dispatched on
	PullRequest        string // PR number, as a string
	HeadSHA            string // immutable PR head SHA
	BaseBranch         string // the PR's base branch
	CheckName          string // the check run the platform publishes
	CheckAppSlug       string // the App that publishes it

	RunID      string // GITHUB_RUN_ID
	RunAttempt string // GITHUB_RUN_ATTEMPT

	WaitSeconds int // how long to poll the dispatched run; 0 judges once
	PollSeconds int // pause between run reads; 0 pauses briefly

	APIURL      string // default https://api.github.com
	SummaryPath string // GITHUB_STEP_SUMMARY
	OutputPath  string // GITHUB_OUTPUT

	Out io.Writer
	// Sleep is the clock behind the polling loop; nil sleeps with the context.
	Sleep func(ctx context.Context, d time.Duration) error
}

// request is the object dispatched to the platform, and the object the
// platform must echo back in its check's output summary.
type request struct {
	Company           string `json:"company"`
	CompanyRepository string `json:"company_repository"`
	PullRequest       string `json:"pull_request"`
	HeadSHA           string `json:"head_sha"`
	BaseSHA           string `json:"base_sha"`
	RequestID         string `json:"request_id"`
}

type pull struct {
	State string `json:"state"`
	Head  struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

type dispatched struct {
	WorkflowRunID int64 `json:"workflow_run_id"`
}

type workflowRun struct {
	ID         int64   `json:"id"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	HeadSHA    string  `json:"head_sha"`
	HTMLURL    string  `json:"html_url"`
}

type checkRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Summary string `json:"summary"`
	} `json:"output"`
}

type checkRuns struct {
	CheckRuns []checkRun `json:"check_runs"`
}

// result is the structured summary the platform must publish.
type result struct {
	Request            json.RawMessage `json:"request"`
	CandidateSHA       string          `json:"candidate_sha"`
	PlatformRepository string          `json:"platform_repository"`
	PlatformRevision   string          `json:"platform_revision"`
}

// Run validates the inputs, dispatches the platform validation, waits for the
// exact run, accepts only its exactly correlated successful check, and only
// then writes the step summary and the outputs.
func Run(ctx context.Context, o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if err := o.validate(); err != nil {
		return o.fail("%v", err)
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	company := ghapi.New(o.APIURL, o.Token)
	platform := ghapi.New(o.APIURL, o.DispatchToken)
	platform.Version = dispatchAPIVersion

	pr, err := o.readPull(ctx, company, "")
	if err != nil {
		return o.fail("read pull request %s#%s: %v", o.CompanyRepository, o.PullRequest, err)
	}

	req := request{
		Company:           o.Company,
		CompanyRepository: o.CompanyRepository,
		PullRequest:       o.PullRequest,
		HeadSHA:           o.HeadSHA,
		BaseSHA:           pr.Base.SHA,
		RequestID:         fmt.Sprintf("%s:%s:%s:%s:%s:%s", o.CompanyRepository, o.PullRequest, o.HeadSHA, pr.Base.SHA, o.RunID, o.RunAttempt),
	}

	runID, err := o.dispatch(ctx, platform, req)
	if err != nil {
		return o.fail("%v", err)
	}

	run, err := o.wait(ctx, platform, runID)
	if err != nil {
		return o.fail("%v", err)
	}

	check, err := o.readCheck(ctx, company, req)
	if err != nil {
		return o.fail("%v", err)
	}
	if err := o.verifySummary(check, req, run); err != nil {
		return o.fail("%v", err)
	}

	// Re-read the pull request immediately before accepting: the platform
	// validated one candidate, and the merge would land whatever is there now.
	if _, err := o.readPull(ctx, company, pr.Base.SHA); err != nil {
		return o.fail("re-read pull request before accepting: %v", err)
	}

	if err := writeFile(o.SummaryPath, check.Output.Summary+"\n"); err != nil {
		return o.fail("write the step summary: %v", err)
	}
	outputs := fmt.Sprintf("run-id=%d\nrun-url=%s\nrequest-id=%s\n", run.ID, run.HTMLURL, req.RequestID)
	if err := writeFile(o.OutputPath, outputs); err != nil {
		return o.fail("write the step outputs: %v", err)
	}
	fmt.Fprintf(o.Out, "platform run %d completed successfully; check %d matches request %s\n", run.ID, check.ID, req.RequestID)
	return nil
}

// validate refuses malformed or incomplete inputs before any request is made.
// Nothing is interpolated into a URL path, a query or a body without having
// matched one of these shapes.
func (o *Options) validate() error {
	required := []struct {
		name, value string
	}{
		{"token", o.Token},
		{"dispatch-token", o.DispatchToken},
		{"company", o.Company},
		{"company-repository", o.CompanyRepository},
		{"platform-repository", o.PlatformRepository},
		{"platform-workflow", o.PlatformWorkflow},
		{"platform-ref", o.PlatformRef},
		{"pull-request", o.PullRequest},
		{"head-sha", o.HeadSHA},
		{"base-branch", o.BaseBranch},
		{"check-name", o.CheckName},
		{"check-app-slug", o.CheckAppSlug},
		{"run-id", o.RunID},
		{"run-attempt", o.RunAttempt},
	}
	for _, f := range required {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("%s is required", f.name)
		}
		if untrustedChars.MatchString(f.value) {
			return fmt.Errorf("%s contains a control character", f.name)
		}
	}
	for _, f := range []struct{ name, value string }{
		{"company-repository", o.CompanyRepository},
		{"platform-repository", o.PlatformRepository},
	} {
		if !repositoryRe.MatchString(f.value) || strings.Contains(f.value, "..") {
			return fmt.Errorf("%s %q is not an owner/name repository", f.name, f.value)
		}
	}
	for _, f := range []struct{ name, value string }{
		{"pull-request", o.PullRequest},
		{"run-id", o.RunID},
		{"run-attempt", o.RunAttempt},
	} {
		if !positiveIntRe.MatchString(f.value) {
			return fmt.Errorf("%s %q is not a positive integer", f.name, f.value)
		}
	}
	if !shaRe.MatchString(o.HeadSHA) {
		return fmt.Errorf("head-sha %q is not a 40-character lowercase hex commit", o.HeadSHA)
	}
	if !refRe.MatchString(o.PlatformRef) {
		return fmt.Errorf("platform-ref %q is not a plain git ref", o.PlatformRef)
	}
	if !refRe.MatchString(o.BaseBranch) {
		return fmt.Errorf("base-branch %q is not a plain branch name", o.BaseBranch)
	}
	if !appSlugRe.MatchString(o.CheckAppSlug) {
		return fmt.Errorf("check-app-slug %q is not an App slug", o.CheckAppSlug)
	}
	if o.WaitSeconds < 0 {
		return fmt.Errorf("wait-timeout-seconds must not be negative")
	}
	if o.PollSeconds < 0 {
		return fmt.Errorf("poll-interval-seconds must not be negative")
	}
	return nil
}

// readPull reads the current pull request and requires the exact candidate
// this run was started for. Called once before dispatch and again before the
// result is accepted, so a fork, a close, a moved head or a moved base fails
// the gate rather than validating something else.
func (o *Options) readPull(ctx context.Context, c *ghapi.Client, expectedBaseSHA string) (pull, error) {
	var pr pull
	body, _, err := c.Get(ctx, c.BaseURL+"/repos/"+o.CompanyRepository+"/pulls/"+o.PullRequest, "application/vnd.github+json")
	if err != nil {
		return pr, err
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return pr, fmt.Errorf("parse pull request: %v", err)
	}
	switch {
	case pr.State != "open":
		return pr, fmt.Errorf("pull request is %q, not open", pr.State)
	case pr.Head.Repo.FullName != o.CompanyRepository:
		return pr, fmt.Errorf("head repository is %q, not %q", pr.Head.Repo.FullName, o.CompanyRepository)
	case pr.Base.Repo.FullName != o.CompanyRepository:
		return pr, fmt.Errorf("base repository is %q, not %q", pr.Base.Repo.FullName, o.CompanyRepository)
	case pr.Base.Ref != o.BaseBranch:
		return pr, fmt.Errorf("base branch is %q, not %q", pr.Base.Ref, o.BaseBranch)
	case pr.Head.SHA != o.HeadSHA:
		return pr, fmt.Errorf("head is %q, not the requested %q", pr.Head.SHA, o.HeadSHA)
	case !shaRe.MatchString(pr.Base.SHA):
		return pr, fmt.Errorf("base SHA %q is not a 40-character lowercase hex commit", pr.Base.SHA)
	case expectedBaseSHA != "" && pr.Base.SHA != expectedBaseSHA:
		return pr, fmt.Errorf("base moved from %q to %q", expectedBaseSHA, pr.Base.SHA)
	}
	return pr, nil
}

// dispatch asks the platform to start the validation and requires the ID of
// the run it started.
func (o *Options) dispatch(ctx context.Context, c *ghapi.Client, req request) (int64, error) {
	payload, err := json.Marshal(map[string]any{
		"ref":                o.PlatformRef,
		"inputs":             req,
		"return_run_details": true,
	})
	if err != nil {
		return 0, fmt.Errorf("encode dispatch: %v", err)
	}
	endpoint := c.BaseURL + "/repos/" + o.PlatformRepository + "/actions/workflows/" +
		url.PathEscape(o.PlatformWorkflow) + "/dispatches"
	status, body, err := c.Raw(ctx, http.MethodPost, endpoint, payload)
	if err != nil {
		return 0, fmt.Errorf("dispatch %s: %v", o.PlatformWorkflow, err)
	}
	if status < 200 || status > 299 {
		return 0, fmt.Errorf("dispatch %s: HTTP %d: %s", o.PlatformWorkflow, status, c.Redact(truncate(string(body), 200)))
	}
	var d dispatched
	if err := json.Unmarshal(body, &d); err != nil {
		return 0, fmt.Errorf("dispatch %s returned no run details: %v", o.PlatformWorkflow, err)
	}
	if d.WorkflowRunID <= 0 {
		return 0, fmt.Errorf("dispatch %s returned no workflow_run_id", o.PlatformWorkflow)
	}
	fmt.Fprintf(o.Out, "dispatched %s on %s at %s as run %d\n", o.PlatformWorkflow, o.PlatformRepository, o.PlatformRef, d.WorkflowRunID)
	return d.WorkflowRunID, nil
}

// wait polls the exact dispatched run until it completes, and accepts only a
// successful conclusion. Every other terminal conclusion is a refusal.
func (o *Options) wait(ctx context.Context, c *ghapi.Client, runID int64) (workflowRun, error) {
	wait := time.Duration(o.WaitSeconds) * time.Second
	poll := time.Duration(o.PollSeconds) * time.Second
	if poll <= 0 {
		poll = time.Millisecond
	}
	deadline := time.Now().Add(wait)
	endpoint := fmt.Sprintf("%s/repos/%s/actions/runs/%d", c.BaseURL, o.PlatformRepository, runID)
	for {
		var run workflowRun
		body, _, err := c.Get(ctx, endpoint, "application/vnd.github+json")
		if err != nil {
			return run, fmt.Errorf("read platform run %d: %v", runID, err)
		}
		if err := json.Unmarshal(body, &run); err != nil {
			return run, fmt.Errorf("parse platform run %d: %v", runID, err)
		}
		if run.Status == "completed" {
			conclusion := ""
			if run.Conclusion != nil {
				conclusion = *run.Conclusion
			}
			if conclusion != "success" {
				fmt.Fprintf(o.Out, "platform run %d: status=%s conclusion=%s url=%s\n", runID, run.Status, conclusion, run.HTMLURL)
				return run, fmt.Errorf("platform run %d concluded %q, not success", runID, conclusion)
			}
			if !shaRe.MatchString(run.HeadSHA) {
				return run, fmt.Errorf("platform run %d has no immutable revision: %q", runID, run.HeadSHA)
			}
			fmt.Fprintf(o.Out, "platform run %d completed successfully (revision %s)\n", runID, run.HeadSHA)
			return run, nil
		}
		remaining := time.Until(deadline)
		if o.WaitSeconds == 0 || remaining <= 0 {
			conclusion := ""
			if run.Conclusion != nil {
				conclusion = *run.Conclusion
			}
			fmt.Fprintf(o.Out, "platform run %d: status=%s conclusion=%s url=%s\n", runID, run.Status, conclusion, run.HTMLURL)
			return run, fmt.Errorf("timed out after %ds waiting for platform run %d (status %q)", o.WaitSeconds, runID, run.Status)
		}
		pause := poll
		if pause > remaining {
			pause = remaining
		}
		if err := o.Sleep(ctx, pause); err != nil {
			return run, err
		}
	}
}

// readCheck reads the company commit's check runs and selects the newest one
// that names this check, this commit, this request ID and this App. A run id
// is monotonic, so the greatest matching id is the newest; when several
// historical attempts match, only the newest decides.
func (o *Options) readCheck(ctx context.Context, c *ghapi.Client, req request) (checkRun, error) {
	query := url.Values{}
	query.Set("check_name", o.CheckName)
	query.Set("filter", "all")
	query.Set("per_page", "100")
	endpoint := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?%s", c.BaseURL, o.CompanyRepository, o.HeadSHA, query.Encode())
	body, _, err := c.Get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return checkRun{}, fmt.Errorf("read check runs for %s: %v", o.HeadSHA, err)
	}
	var listed checkRuns
	if err := json.Unmarshal(body, &listed); err != nil {
		return checkRun{}, fmt.Errorf("parse check runs: %v", err)
	}
	var match *checkRun
	for i := range listed.CheckRuns {
		cr := &listed.CheckRuns[i]
		if cr.Name != o.CheckName || cr.HeadSHA != o.HeadSHA || cr.ExternalID != req.RequestID || cr.App.Slug != o.CheckAppSlug {
			continue
		}
		if match == nil || cr.ID >= match.ID {
			match = cr
		}
	}
	if match == nil {
		return checkRun{}, fmt.Errorf("no %q check by App %q matches request %s on %s", o.CheckName, o.CheckAppSlug, req.RequestID, o.HeadSHA)
	}
	if match.Status != "completed" || match.Conclusion != "success" {
		return checkRun{}, fmt.Errorf("the matching %q check is status=%q conclusion=%q, not a completed success", o.CheckName, match.Status, match.Conclusion)
	}
	return *match, nil
}

// verifySummary requires the platform's structured summary to name exactly
// this request, this candidate, this platform and the revision of the run it
// just completed. Deep equality rejects both a missing and an extra field.
func (o *Options) verifySummary(check checkRun, req request, run workflowRun) error {
	if strings.TrimSpace(check.Output.Summary) == "" {
		return fmt.Errorf("the matching %q check has no output summary", o.CheckName)
	}
	var got result
	if err := json.Unmarshal([]byte(check.Output.Summary), &got); err != nil {
		return fmt.Errorf("the matching %q check has a malformed output summary: %v", o.CheckName, err)
	}
	if len(got.Request) == 0 {
		return fmt.Errorf("the matching %q check summary has no request", o.CheckName)
	}
	var gotRequest map[string]any
	if err := json.Unmarshal(got.Request, &gotRequest); err != nil {
		return fmt.Errorf("the matching %q check summary has a malformed request: %v", o.CheckName, err)
	}
	wantRequest, err := toMap(req)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(gotRequest, wantRequest) {
		return fmt.Errorf("the platform summary request does not equal the dispatched request")
	}
	switch {
	case got.CandidateSHA != o.HeadSHA:
		return fmt.Errorf("the platform summary names candidate %q, not %q", got.CandidateSHA, o.HeadSHA)
	case got.PlatformRepository != o.PlatformRepository:
		return fmt.Errorf("the platform summary names repository %q, not %q", got.PlatformRepository, o.PlatformRepository)
	case !shaRe.MatchString(got.PlatformRevision):
		return fmt.Errorf("the platform summary revision %q is not a 40-character lowercase hex commit", got.PlatformRevision)
	case got.PlatformRevision != run.HeadSHA:
		return fmt.Errorf("the platform summary revision %q is not the run's revision %q", got.PlatformRevision, run.HeadSHA)
	}
	return nil
}

func toMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (o *Options) fail(format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	for _, token := range []string{o.Token, o.DispatchToken} {
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "***")
		}
	}
	fmt.Fprintf(o.Out, "::error::company-validation: %s\n", msg)
	return ErrFailed
}

func writeFile(path, content string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
