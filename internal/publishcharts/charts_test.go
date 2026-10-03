package publishcharts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/truvity/ci-actions/internal/runcmd"
)

func render(cs []Chart) string {
	var l []string
	for _, c := range cs {
		l = append(l, c.Dir+" -> "+c.Name)
	}
	return strings.Join(l, "\n")
}

// The cases of hack/chart-paths-cases.sh in ci-workflows, which ran the Python
// resolver as written: what a plain entry and a repository-root path resolve
// to, and everything that is refused.
func TestResolve(t *testing.T) {
	for _, tc := range []struct{ charts, root, want string }{
		{`["github-roster"]`, "charts", "charts/github-roster -> github-roster"},
		{`["url-shortener","url-shortener-infra"]`, "examples/url-shortener/charts",
			"examples/url-shortener/charts/url-shortener -> url-shortener\nexamples/url-shortener/charts/url-shortener-infra -> url-shortener-infra"},
		{`["a"]`, "charts/", "charts/a -> a"},
		{`[]`, "charts", ""},
		{`["charts/service-lib"]`, "examples/url-shortener/charts", "charts/service-lib -> service-lib"},
		{`["url-shortener","charts/service-lib"]`, "examples/url-shortener/charts",
			"examples/url-shortener/charts/url-shortener -> url-shortener\ncharts/service-lib -> service-lib"},
		{`["libs/shared/common"]`, "charts", "libs/shared/common -> common"},
		{`["a.b_c-d"]`, "c", "c/a.b_c-d -> a.b_c-d"},
	} {
		got, err := Resolve(tc.charts, tc.root)
		if err != nil || render(got) != tc.want {
			t.Errorf("%s: got [%s] err %v, want [%s]", tc.charts, render(got), err, tc.want)
		}
	}
}

func TestResolveRefusals(t *testing.T) {
	for _, tc := range []struct{ charts, want string }{
		{`not json`, "charts: not a JSON array: "},
		{`"url-shortener"`, "charts: not a JSON array"},
		{`{"a":1}`, "charts: not a JSON array"},
		{`[1]`, "charts: 1 is not a non-empty string"},
		{`[1.5]`, "charts: 1.5 is not a non-empty string"},
		{`[null]`, "charts: None is not a non-empty string"},
		{`[true]`, "charts: True is not a non-empty string"},
		{`[""]`, "charts: '' is not a non-empty string"},
		{`["/charts/x"]`, "charts: '/charts/x' is absolute: a chart path is from the repository root"},
		{`["../x"]`, "charts: '../x' has an empty, `.` or `..` element: name the chart, not a way to it"},
		{`["charts/../x"]`, "charts: 'charts/../x' has an empty, `.` or `..` element: name the chart, not a way to it"},
		{`["charts//x"]`, "charts: 'charts//x' has an empty, `.` or `..` element: name the chart, not a way to it"},
		{`["charts/x/"]`, "charts: 'charts/x/' has an empty, `.` or `..` element: name the chart, not a way to it"},
		{`["./x"]`, "charts: './x' has an empty, `.` or `..` element: name the chart, not a way to it"},
		{`["Upper"]`, "charts: 'Upper': the chart name 'Upper' is not a lowercase chart name (letters, digits, `-`, `.`, `_`)"},
		{`["charts/Upper"]`, "charts: 'charts/Upper': the chart name 'Upper' is not a lowercase chart name (letters, digits, `-`, `.`, `_`)"},
		{`["a b"]`, "charts: 'a b': the chart name 'a b' is not a lowercase chart name (letters, digits, `-`, `.`, `_`)"},
		{`["it's"]`, `charts: "it's": the chart name "it's" is not a lowercase chart name (letters, digits, ` + "`-`, `.`, `_`)"},
		{`["charts/x","other/x"]`, "charts: 'charts/x' and 'other/x' are both published as 'x': a chart is packaged, pushed and named by its last path element"},
		{`["x","charts/x"]`, "charts: 'x' and 'charts/x' are both published as 'x': a chart is packaged, pushed and named by its last path element"},
	} {
		_, err := Resolve(tc.charts, "charts")
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want it to start with %q", tc.charts, err, tc.want)
		}
	}
}

type rig struct {
	calls []string
	fail  string // a substring of a command line that fails
	out   bytes.Buffer
	err   bytes.Buffer
	o     Options
}

func newRig(t *testing.T) *rig {
	r := &rig{}
	r.o = Options{Charts: `["a","libs/b"]`, ChartRoot: "charts", Registry: "oci://reg.example.com/acme/charts", AppVersionMode: "release",
		HelmctlVersion: "0.8.0", Tag: "v1.2.3", RunnerTemp: t.TempDir(), Out: &r.out, Err: &r.err}
	r.o.Exec = func(_ context.Context, c runcmd.Cmd) error {
		line := c.Name + " " + strings.Join(c.Args, " ")
		line = strings.ReplaceAll(line, r.o.RunnerTemp, "$T")
		if c.Name == "tar" { // the download feeds the extraction
			io.Copy(io.Discard, c.Stdin)
		}
		r.calls = append(r.calls, line)
		if r.fail != "" && strings.Contains(line, r.fail) {
			return errors.New("boom")
		}
		return nil
	}
	return r
}

func TestRunDefaultMode(t *testing.T) {
	r := newRig(t)
	if err := Run(context.Background(), r.o); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"gh release download v0.8.0 --repo truvity/ocictl --pattern helmctl_0.8.0_linux_" + archOf() + ".tar.gz --output -",
		"tar -xz -C $T/helmctl helmctl",
		"devbox run -- $T/helmctl/helmctl package --chart charts/a --version 1.2.3 --app-version 1.2.3 --output dist/",
		"$T/helmctl/helmctl push --tgz dist/a-1.2.3.tgz --registry reg.example.com --repository acme/charts/a --name a --version 1.2.3",
		"devbox run -- $T/helmctl/helmctl package --chart libs/b --version 1.2.3 --app-version 1.2.3 --output dist/",
		"$T/helmctl/helmctl push --tgz dist/b-1.2.3.tgz --registry reg.example.com --repository acme/charts/b --name b --version 1.2.3",
	}
	if strings.Join(sorted(r.calls[:2]), "|")+"|"+strings.Join(r.calls[2:], "|") != strings.Join(sorted(want[:2]), "|")+"|"+strings.Join(want[2:], "|") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(r.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := r.out.String(); got != "::group::a 1.2.3\n::endgroup::\n::group::b 1.2.3\n::endgroup::\n" {
		t.Errorf("log %q", got)
	}
}

func sorted(s []string) []string {
	c := append([]string(nil), s...)
	for i := range c {
		for j := i + 1; j < len(c); j++ {
			if c[j] < c[i] {
				c[i], c[j] = c[j], c[i]
			}
		}
	}
	return c
}

func TestRunModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mod       func(*Options)
		wantArgs  []string // the package and manifest calls, after the download
		wantErr   string
		wantNoRun bool
	}{
		{"chart mode leaves appVersion", func(o *Options) { o.AppVersionMode = "chart"; o.Charts = `["a"]` },
			[]string{"devbox run -- $T/helmctl/helmctl package --chart charts/a --version 1.2.3 --output dist/"}, "", false},
		{"require digests", func(o *Options) { o.RequireDigests = "true"; o.Charts = `["a"]` },
			[]string{"devbox run -- $T/helmctl/helmctl package --chart charts/a --version 1.2.3 --app-version 1.2.3 --require-image-digests --output dist/"}, "", false},
		{"other values do not require digests", func(o *Options) { o.RequireDigests = "false"; o.Charts = `["a"]` },
			[]string{"devbox run -- $T/helmctl/helmctl package --chart charts/a --version 1.2.3 --app-version 1.2.3 --output dist/"}, "", false},
		{"goreleaser manifest replaces the version flags", func(o *Options) { o.ChartImages = "goreleaser"; o.Charts = `["a"]`; o.RequireDigests = "true" },
			[]string{"$T/helmctl/helmctl goreleaser-manifest --goreleaser-dist dist --output $T/a-manifest.yaml",
				"devbox run -- $T/helmctl/helmctl package --chart charts/a --manifest $T/a-manifest.yaml --require-image-digests --output dist/"}, "", false},
		{"an unknown mode is refused even with a manifest", func(o *Options) { o.AppVersionMode = "Release"; o.ChartImages = "goreleaser"; o.Charts = `["a"]` },
			nil, "::error::chart-app-version must be 'release' or 'chart', got 'Release'\n", true},
		{"registry without a path", func(o *Options) { o.Registry = "oci://reg.example.com"; o.Charts = `["a"]` },
			[]string{"devbox run -- $T/helmctl/helmctl package --chart charts/a --version 1.2.3 --app-version 1.2.3 --output dist/"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.mod(&r.o)
			err := Run(context.Background(), r.o)
			if tc.wantErr != "" {
				if !errors.Is(err, ErrFailed) || !strings.Contains(r.out.String(), tc.wantErr) {
					t.Errorf("err %v log %q", err, r.out.String())
				}
				for _, c := range r.calls[2:] {
					t.Errorf("nothing may run after the refusal: %s", c)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range r.calls[2:] {
				if !strings.Contains(c, " push ") {
					got = append(got, c)
				}
			}
			if strings.Join(got, "\n") != strings.Join(tc.wantArgs, "\n") {
				t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.wantArgs, "\n"))
			}
		})
	}
}

func TestRegistrySplit(t *testing.T) {
	r := newRig(t)
	r.o.Registry = "oci://reg.example.com"
	r.o.Charts = `["a"]`
	if err := Run(context.Background(), r.o); err != nil {
		t.Fatal(err)
	}
	if push := r.calls[len(r.calls)-1]; !strings.Contains(push, "--registry reg.example.com --repository reg.example.com/a ") {
		t.Errorf("a registry with no path keeps the whole reference as the prefix, as ${ref#*/} did: %s", push)
	}
}

func TestRefusalBeforeAnythingIsBuilt(t *testing.T) {
	r := newRig(t)
	r.o.Charts = `["Upper"]`
	err := Run(context.Background(), r.o)
	if !errors.Is(err, ErrFailed) || !strings.Contains(r.err.String(), "::error::charts: 'Upper': the chart name") {
		t.Errorf("err %v stderr %q", err, r.err.String())
	}
	for _, c := range r.calls {
		if strings.Contains(c, "package") || strings.Contains(c, "push") {
			t.Errorf("ran %s", c)
		}
	}
}

func TestFailuresStopTheLoop(t *testing.T) {
	for _, fail := range []string{"release download", "tar -xz", "package --chart charts/a", "push --tgz"} {
		r := newRig(t)
		r.fail = fail
		if err := Run(context.Background(), r.o); err == nil {
			t.Errorf("%s: a failed command must fail the step", fail)
		}
		for _, c := range r.calls {
			if strings.Contains(c, "libs/b") {
				t.Errorf("%s: went on to the next chart: %s", fail, c)
			}
		}
	}
}

func archOf() string { return runtime.GOARCH }
