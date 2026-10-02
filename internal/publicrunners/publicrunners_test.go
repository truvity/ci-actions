package publicrunners

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const repo = "acme/app"

func run(o Options) (string, error) {
	var b strings.Builder
	o.Out = &b
	if o.Repository == "" {
		o.Repository = repo
	}
	err := Run(context.Background(), o)
	return b.String(), err
}

func TestRun(t *testing.T) {
	refusal := "::error::acme/app is public, so its jobs run on GitHub-hosted runners only. A fork's code must never execute on the estate's own infrastructure. Drop the runner inputs and take the hosted default; if the work genuinely needs the estate, run it from a private repository that checks this one out at a pinned version.\n"
	for _, tc := range []struct {
		name string
		in   Options
		out  string
		fail bool
	}{
		{"private takes any runner", Options{Runners: "tier-big", Visibility: "private"}, "acme/app is private — any runner is allowed\n", false},
		{"internal takes any runner", Options{Runners: "x", Visibility: "internal"}, "acme/app is internal — any runner is allowed\n", false},
		{"public hosted", Options{Runners: "ubuntu-latest", Visibility: "public"}, "ok        ubuntu-latest\npublic repository, hosted runners only — checked\n", false},
		{"all three platforms", Options{Runners: "ubuntu-24.04 windows-2022\nmacos-14", Visibility: "public"},
			"ok        ubuntu-24.04\nok        windows-2022\nok        macos-14\npublic repository, hosted runners only — checked\n", false},
		{"no runners at all is fine", Options{Runners: "", Visibility: "public"}, "public repository, hosted runners only — checked\n", false},
		{"blank entries are ignored", Options{Runners: "  \t ubuntu-latest   ", Visibility: "public"}, "ok        ubuntu-latest\npublic repository, hosted runners only — checked\n", false},
		{"self-hosted label", Options{Runners: "self-hosted", Visibility: "public"}, "SELF-HOSTED  self-hosted\n" + refusal, true},
		{"estate tier among hosted", Options{Runners: "ubuntu-latest tier-small ubuntu-latest", Visibility: "public"},
			"ok        ubuntu-latest\nSELF-HOSTED  tier-small\nok        ubuntu-latest\n" + refusal, true},
		{"larger runner is refused too", Options{Runners: "linux-big", Visibility: "public"}, "SELF-HOSTED  linux-big\n" + refusal, true},
		{"prefix must be followed by a dash", Options{Runners: "ubuntu", Visibility: "public"}, "SELF-HOSTED  ubuntu\n" + refusal, true},
		{"unknown visibility refuses to guess", Options{Runners: "ubuntu-latest", APIURL: "http://127.0.0.1:1"},
			"::error::cannot tell whether acme/app is public; refusing to guess. Pass visibility: ${{ github.event.repository.visibility }}.\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := run(tc.in)
			if out != tc.out {
				t.Errorf("output:\n%q\nwant:\n%q", out, tc.out)
			}
			if tc.fail != errors.Is(err, ErrRefused) || (!tc.fail && err != nil) {
				t.Errorf("err = %v, fail want %v", err, tc.fail)
			}
		})
	}
}

func TestAPIFallback(t *testing.T) {
	const token = "ghs_never_print_me"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/repos/acme/app":
			fmt.Fprint(w, `{"visibility":"public"}`)
		case "/repos/acme/priv":
			fmt.Fprint(w, `{"visibility":"private"}`)
		case "/repos/acme/bad":
			fmt.Fprint(w, `not json`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out, err := run(Options{Runners: "ubuntu-latest", APIURL: srv.URL, Token: token})
	if err != nil || !strings.Contains(out, "hosted runners only") {
		t.Errorf("public via API: %v %q", err, out)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("the job's token authenticates the request: %q", gotAuth)
	}
	if out, err := run(Options{Runners: "tier", APIURL: srv.URL, Repository: "acme/priv"}); err != nil || !strings.Contains(out, "is private") {
		t.Errorf("private via API: %v %q", err, out)
	}
	for _, r := range []string{"acme/bad", "acme/missing"} {
		out, err := run(Options{Runners: "x", APIURL: srv.URL, Repository: r, Token: token})
		if !errors.Is(err, ErrRefused) || !strings.Contains(out, "cannot tell whether "+r) {
			t.Errorf("%s: %v %q", r, err, out)
		}
		if strings.Contains(out, token) {
			t.Error("the token must never be printed")
		}
	}
}
