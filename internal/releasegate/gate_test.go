package releasegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func cr(id int64, name, url string, conclusion *string) checkRun {
	return checkRun{ID: id, Name: name, DetailsURL: url, Conclusion: conclusion}
}

// crApp is cr with the app that posted the check run.
func crApp(id int64, name, app, url string, conclusion *string) checkRun {
	c := cr(id, name, url, conclusion)
	c.App.Slug = app
	return c
}

func s(v string) *string { return &v }

func run(runID string) string { return fmt.Sprintf("https://x/actions/runs/%s/job/1", runID) }

// The sample of the step's own self-test: both exclusions on one commit.
// id 102 "release / release" is THIS run (999), the reusable-workflow shape
// that broke the name-based filter; id 103 "release" is a SIBLING project's
// release (888), pending, the shared-commit cross-block. Both must vanish.
func TestSample(t *testing.T) {
	pages := [][]checkRun{
		{
			cr(100, "check", run("111"), s("success")),
			cr(101, "suite", run("111"), s("cancelled")),
			cr(102, "release / release", run("999"), nil),
			cr(103, "release", run("888"), nil),
			cr(104, "renovate / renovate", run("777"), s("failure")),
			cr(105, "update / update", run("666"), nil),
		},
		{
			cr(200, "check", run("222"), nil),
			cr(201, "suite", run("222"), s("success")),
		},
	}
	got := strings.Join(Lines(pages, "999"), " ")
	if want := "check\tpending suite\tsuccess"; got != want {
		t.Errorf("got [%q] want [%q]: the newest run of a name wins across pages, own run and bot runs are excluded", got, want)
	}
}

func TestLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages [][]checkRun
		want  string
	}{
		{"a newest queued run beats an older success (the stale-green hazard)", [][]checkRun{{cr(1, "check", run("1"), s("success")), cr(2, "check", run("2"), nil)}}, "check\tpending"},
		{"a newest success beats an older failure (a re-run)", [][]checkRun{{cr(1, "check", run("1"), s("failure"))}, {cr(5, "check", run("2"), s("success"))}}, "check\tsuccess"},
		{"ids decide, not page order", [][]checkRun{{cr(9, "check", run("1"), s("success"))}, {cr(3, "check", run("2"), s("failure"))}}, "check\tsuccess"},
		{"own run is excluded by run id wherever it sits in the URL", [][]checkRun{{cr(1, "ci / build", run("999"), nil)}}, ""},
		{"a run id that is a prefix of another is not excluded", [][]checkRun{{cr(1, "build", run("9999"), s("success"))}}, "build\tsuccess"},
		{"release-shaped names are dropped, lookalikes are kept", [][]checkRun{{cr(1, "release", run("1"), nil), cr(2, "release / x", run("1"), nil), cr(3, "releases", run("1"), s("success")), cr(4, "pre-release / x", run("1"), s("success"))}}, "pre-release / x\tsuccess releases\tsuccess"},
		{"bot runs are dropped", [][]checkRun{{cr(1, "renovate / renovate", run("1"), s("failure")), cr(2, "update / update", run("1"), nil), cr(3, "renovate", run("1"), s("success"))}}, "renovate\tsuccess"},
		{"dependency-graph jobs of github-actions and dependabot are dropped", [][]checkRun{{
			crApp(1, "update-go_modules-graph", "github-actions", run("1"), s("failure")),
			crApp(2, "update-npm_and_yarn-graph", "github-actions", run("1"), nil),
			crApp(3, "update-github_actions-graph", "dependabot", run("1"), s("failure")),
			crApp(4, "check", "github-actions", run("1"), s("success"))}}, "check\tsuccess"},
		{"a similar name from another app or without an app is kept", [][]checkRun{{
			crApp(1, "update-go_modules-graph", "some-ci", run("1"), s("failure")),
			cr(2, "update-npm-graph", run("1"), s("failure"))}}, "update-go_modules-graph\tfailure update-npm-graph\tfailure"},
		{"dependabot update jobs of github-actions and dependabot are dropped", [][]checkRun{{
			crApp(1, "Dependabot", "github-actions", run("1"), s("failure")),
			crApp(2, "Dependabot Updates", "github-actions", run("1"), nil),
			crApp(3, "Dependabot", "dependabot", run("1"), s("failure")),
			crApp(4, "check", "github-actions", run("1"), s("success"))}}, "check\tsuccess"},
		{"a Dependabot-named check from another app or without an app is kept", [][]checkRun{{
			crApp(1, "Dependabot", "some-ci", run("1"), s("failure")),
			crApp(2, "Dependabot Updates", "some-ci", run("1"), nil),
			cr(3, "Dependabot", run("1"), s("failure"))}}, "Dependabot\tfailure Dependabot Updates\tpending"},
		{"a github-actions check that only resembles the Dependabot name is kept", [][]checkRun{{
			crApp(1, "dependabot", "github-actions", run("1"), s("failure")),
			crApp(2, "Dependabot / x", "github-actions", run("1"), s("failure")),
			crApp(3, "Dependabot Updates / x", "github-actions", run("1"), s("failure"))}}, "Dependabot / x\tfailure Dependabot Updates / x\tfailure dependabot\tfailure"},
		{"a github-actions check that is not the graph shape is kept", [][]checkRun{{
			crApp(1, "update-docs", "github-actions", run("1"), s("failure")),
			crApp(2, "update--graph", "github-actions", run("1"), s("failure")),
			crApp(3, "graph", "github-actions", run("1"), s("failure")),
			crApp(4, "update-go_modules-graph / x", "github-actions", run("1"), s("failure"))}}, "graph\tfailure update--graph\tfailure update-docs\tfailure update-go_modules-graph / x\tfailure"},
		{"an empty conclusion string is a conclusion, null is pending", [][]checkRun{{cr(1, "a", run("1"), s("")), cr(2, "b", run("1"), nil)}}, "a\t b\tpending"},
		{"sorted by name", [][]checkRun{{cr(1, "b", run("1"), s("success")), cr(2, "a", run("1"), s("success")), cr(3, "B", run("1"), s("success"))}}, "B\tsuccess a\tsuccess b\tsuccess"},
	} {
		if got := strings.Join(Lines(tc.pages, "999"), " "); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// serve answers the check-runs endpoint from a flat list, 100 per page, and
// records what it was asked.
func serve(t *testing.T, runs []checkRun) (*httptest.Server, *[]string) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			http.Error(w, "headers", 401)
			return
		}
		if r.URL.Path != "/repos/acme/app/commits/abc123/check-runs" {
			http.NotFound(w, r)
			return
		}
		var n int
		fmt.Sscan(r.URL.Query().Get("page"), &n)
		lo, hi := (n-1)*100, n*100
		if lo > len(runs) {
			lo = len(runs)
		}
		if hi > len(runs) {
			hi = len(runs)
		}
		json.NewEncoder(w).Encode(page{TotalCount: len(runs), CheckRuns: runs[lo:hi]})
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func gate(t *testing.T, runs []checkRun) (string, error, []string) {
	srv, asked := serve(t, runs)
	var out bytes.Buffer
	err := Run(context.Background(), Options{Token: "tok", Repo: "acme/app", SHA: "abc123", RunID: "999", API: srv.URL, Out: &out})
	return out.String(), err, *asked
}

func TestRun(t *testing.T) {
	t.Run("a red dependency-graph job does not refuse (the app survives the JSON round trip)", func(t *testing.T) {
		out, err, _ := gate(t, []checkRun{cr(1, "check", run("1"), s("success")), crApp(2, "update-go_modules-graph", "github-actions", run("2"), s("failure"))})
		if err != nil || !strings.Contains(out, "all checks green on abc123") || strings.Contains(out, "graph") {
			t.Errorf("err=%v out=%q", err, out)
		}
	})
	t.Run("green", func(t *testing.T) {
		out, err, asked := gate(t, []checkRun{cr(1, "check", run("1"), s("success")), cr(2, "suite", run("1"), s("skipped")), cr(3, "lint", run("1"), s("neutral")), cr(4, "release / release", run("999"), nil)})
		want := "checks on abc123:\n  check\tsuccess\n  lint\tneutral\n  suite\tskipped\nall checks green on abc123\n"
		if err != nil || out != want {
			t.Errorf("err %v out %q want %q", err, out, want)
		}
		if len(asked) != 1 || asked[0] != "/repos/acme/app/commits/abc123/check-runs?per_page=100&page=1" {
			t.Errorf("asked %v", asked)
		}
	})
	t.Run("a pending check refuses, and is listed", func(t *testing.T) {
		out, err, _ := gate(t, []checkRun{cr(1, "check", run("1"), s("success")), cr(2, "suite", run("1"), nil), cr(3, "e2e", run("1"), s("failure"))})
		want := "checks on abc123:\n  check\tsuccess\n  e2e\tfailure\n  suite\tpending\n::error::not-green checks on the tagged commit:\ne2e\tfailure\nsuite\tpending\n"
		if !errors.Is(err, ErrNotGreen) || out != want {
			t.Errorf("err %v out %q want %q", err, out, want)
		}
	})
	t.Run("no successful check refuses (the dms v0.41.0 case: master CI had not finished)", func(t *testing.T) {
		out, err, _ := gate(t, []checkRun{cr(1, "check", run("1"), nil), cr(2, "suite", run("1"), s("success"))})
		want := "checks on abc123:\n  check\tpending\n  suite\tsuccess\n::error::the tagged commit has no successful check run — releases build only from check-green commits\n"
		if !errors.Is(err, ErrNotGreen) || out != want {
			t.Errorf("err %v out %q want %q", err, out, want)
		}
	})
	t.Run("nothing but this run's own check refuses", func(t *testing.T) {
		out, err, _ := gate(t, []checkRun{cr(1, "release / release", run("999"), nil)})
		want := "checks on abc123:\n  \n::error::the tagged commit has no successful check run — releases build only from check-green commits\n"
		if !errors.Is(err, ErrNotGreen) || out != want {
			t.Errorf("err %v out %q want %q", err, out, want)
		}
	})
	t.Run("a check that must be named exactly check", func(t *testing.T) {
		_, err, _ := gate(t, []checkRun{cr(1, "Check", run("1"), s("success"))})
		if !errors.Is(err, ErrNotGreen) {
			t.Errorf("err %v", err)
		}
	})
	t.Run("every page is read", func(t *testing.T) {
		var runs []checkRun
		for i := 1; i <= 250; i++ {
			runs = append(runs, cr(int64(i), fmt.Sprintf("job-%03d", i), run("1"), s("success")))
		}
		runs = append(runs, cr(251, "check", run("1"), s("success")))
		// The only red run is on the last page.
		runs[200] = cr(201, "last-page", run("1"), s("failure"))
		out, err, asked := gate(t, runs)
		if !errors.Is(err, ErrNotGreen) || !strings.Contains(out, "last-page\tfailure\n") || len(asked) != 3 {
			t.Errorf("err %v asked %v", err, asked)
		}
		if !strings.HasSuffix(asked[2], "page=3") {
			t.Errorf("asked %v", asked)
		}
	})
	t.Run("exactly a page of checks is one request", func(t *testing.T) {
		var runs []checkRun
		for i := 1; i < 100; i++ {
			runs = append(runs, cr(int64(i), fmt.Sprintf("job-%03d", i), run("1"), s("success")))
		}
		runs = append(runs, cr(100, "check", run("1"), s("success")))
		_, err, asked := gate(t, runs)
		if err != nil || len(asked) != 1 {
			t.Errorf("err %v asked %v", err, asked)
		}
	})
	t.Run("an API error fails the step without deciding", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 502) }))
		defer srv.Close()
		var out bytes.Buffer
		err := Run(context.Background(), Options{Token: "tok", Repo: "acme/app", SHA: "abc123", RunID: "999", API: srv.URL, Out: &out})
		if err == nil || errors.Is(err, ErrNotGreen) || strings.Contains(out.String(), "all checks green") {
			t.Errorf("err %v out %q", err, out.String())
		}
	})
}

// seq serves a different list of check runs on each successive look.
func seq(t *testing.T, looks ...[]checkRun) *httptest.Server {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := n
		if i >= len(looks) {
			i = len(looks) - 1
		}
		n++
		json.NewEncoder(w).Encode(page{TotalCount: len(looks[i]), CheckRuns: looks[i]})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunWait(t *testing.T) {
	var slept []time.Duration
	opts := func(srv *httptest.Server, out *bytes.Buffer) Options {
		slept = nil
		return Options{Token: "tok", Repo: "acme/app", SHA: "abc123", RunID: "999", API: srv.URL, Out: out,
			Wait: 100 * time.Second, Poll: 30 * time.Second,
			Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	}
	t.Run("a check that is still running is waited for", func(t *testing.T) {
		srv := seq(t,
			[]checkRun{},
			[]checkRun{cr(1, "check", run("1"), nil)},
			[]checkRun{cr(1, "check", run("1"), s("success")), cr(2, "suite", run("1"), nil)},
			[]checkRun{cr(1, "check", run("1"), s("success")), cr(2, "suite", run("1"), s("success"))},
		)
		var out bytes.Buffer
		if err := Run(context.Background(), opts(srv, &out)); err != nil || len(slept) != 3 || !strings.HasSuffix(out.String(), "all checks green on abc123\n") {
			t.Errorf("err %v slept %v out %q", err, slept, out.String())
		}
	})
	t.Run("a red check refuses at once", func(t *testing.T) {
		srv := seq(t, []checkRun{cr(1, "check", run("1"), nil), cr(2, "suite", run("1"), s("failure"))})
		var out bytes.Buffer
		err := Run(context.Background(), opts(srv, &out))
		if !errors.Is(err, ErrNotGreen) || len(slept) != 0 || !strings.Contains(out.String(), "suite\tfailure") {
			t.Errorf("err %v slept %v out %q", err, slept, out.String())
		}
	})
	t.Run("a cancelled check refuses at once", func(t *testing.T) {
		srv := seq(t, []checkRun{cr(1, "check", run("1"), s("cancelled"))})
		var out bytes.Buffer
		if err := Run(context.Background(), opts(srv, &out)); !errors.Is(err, ErrNotGreen) || len(slept) != 0 {
			t.Errorf("err %v slept %v", err, slept)
		}
	})
	t.Run("a check that never finishes times out, bounded", func(t *testing.T) {
		srv := seq(t, []checkRun{cr(1, "check", run("1"), nil)})
		var out bytes.Buffer
		err := Run(context.Background(), opts(srv, &out))
		var total time.Duration
		for _, d := range slept {
			total += d
		}
		if !errors.Is(err, ErrNotGreen) || total != 100*time.Second || !strings.Contains(out.String(), "no successful check run after 1m40s") {
			t.Errorf("err %v slept %v out %q", err, slept, out.String())
		}
	})
	t.Run("green with another check pending times out naming the pending check", func(t *testing.T) {
		srv := seq(t, []checkRun{cr(1, "check", run("1"), s("success")), cr(2, "suite", run("1"), nil)})
		var out bytes.Buffer
		err := Run(context.Background(), opts(srv, &out))
		if !errors.Is(err, ErrNotGreen) || !strings.HasSuffix(out.String(), "::error::not-green checks on the tagged commit:\nsuite\tpending\n") {
			t.Errorf("err %v out %q", err, out.String())
		}
	})
}
