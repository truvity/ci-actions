// Package publishcharts is the port of the release workflow's "Package and
// push charts" step: which charts a release publishes and under which names
// (refused before anything is built or pushed), then helmctl's package and
// push for each. gh, devbox and helmctl stay the executed programs.
package publishcharts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// ErrFailed marks a failure whose message is already written.
var ErrFailed = errors.New("publish-charts: failed")

// Options are the step's environment.
type Options struct {
	Charts         string // CHARTS: JSON array of plain names or repository-root paths
	ChartRoot      string // CHART_ROOT
	Registry       string // REGISTRY: one oci:// URL
	AppVersionMode string // APP_VERSION_MODE: release or chart
	ChartImages    string // CHART_IMAGES: goreleaser or empty
	RequireDigests string // REQUIRE_IMAGE_DIGESTS: "true" or not
	HelmctlVersion string // HELMCTL_VERSION
	Tag            string // GITHUB_REF_NAME
	RunnerTemp     string // RUNNER_TEMP
	Dir            string // the checkout GoReleaser built in; "" is the working directory
	Out            io.Writer
	Err            io.Writer
	Exec           runcmd.Exec
}

// Chart is one chart to publish.
type Chart struct{ Dir, Name string }

var chartName = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)

// pyRepr is Python's repr of a JSON value, which the refusals quote.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		f, _ := x.Float64()
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEn") {
			s += ".0"
		}
		return s
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		var b strings.Builder
		b.WriteString(q)
		for _, r := range x {
			switch {
			case string(r) == q || r == '\\':
				b.WriteString(`\` + string(r))
			case r == '\n':
				b.WriteString(`\n`)
			case r == '\r':
				b.WriteString(`\r`)
			case r == '\t':
				b.WriteString(`\t`)
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, `\x%02x`, r)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteString(q)
		return b.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Resolve is what `charts` means: one entry per chart, a plain name under
// chartRoot or, with a `/`, a path from the repository root. The name is the
// last element either way, and it is what is packaged, pushed and tagged.
// Everything wrong is refused before anything is built or pushed, because a
// chart published under the wrong name cannot be taken back.
func Resolve(charts, chartRoot string) ([]Chart, error) {
	refuse := func(format string, a ...any) ([]Chart, error) {
		return nil, fmt.Errorf("charts: "+format, a...)
	}
	dec := json.NewDecoder(strings.NewReader(charts))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return refuse("not a JSON array: %v", err)
	}
	if dec.More() {
		return refuse("not a JSON array: extra data")
	}
	entries, ok := v.([]any)
	if !ok {
		return refuse("not a JSON array")
	}
	root := strings.TrimRight(chartRoot, "/")
	seen := map[string]string{}
	var out []Chart
	for _, e := range entries {
		entry, ok := e.(string)
		if !ok || entry == "" {
			return refuse("%s is not a non-empty string", pyRepr(e))
		}
		if strings.HasPrefix(entry, "/") {
			return refuse("%s is absolute: a chart path is from the repository root", pyRepr(entry))
		}
		parts := strings.Split(entry, "/")
		for _, p := range parts {
			if p == "" || p == "." || p == ".." {
				return refuse("%s has an empty, `.` or `..` element: name the chart, not a way to it", pyRepr(entry))
			}
		}
		name := parts[len(parts)-1]
		if !chartName.MatchString(name) {
			return refuse("%s: the chart name %s is not a lowercase chart name (letters, digits, `-`, `.`, `_`)", pyRepr(entry), pyRepr(name))
		}
		if prev, dup := seen[name]; dup {
			return refuse("%s and %s are both published as %s: a chart is packaged, pushed and named by its last path element", pyRepr(prev), pyRepr(entry), pyRepr(name))
		}
		seen[name] = entry
		dir := entry
		if !strings.Contains(entry, "/") {
			dir = root + "/" + entry
		}
		out = append(out, Chart{Dir: dir, Name: name})
	}
	return out, nil
}

// Run downloads helmctl, then packages and pushes each chart.
func Run(ctx context.Context, o Options) error {
	if o.Exec == nil {
		o.Exec = runcmd.OS
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	version := strings.TrimPrefix(o.Tag, "v")
	arch := runtime.GOARCH
	helmctlDir := filepath.Join(o.RunnerTemp, "helmctl")
	if err := os.MkdirAll(helmctlDir, 0o755); err != nil {
		return err
	}
	// gh release download ... --output - | tar -xz -C DIR helmctl
	pr, pw := io.Pipe()
	ghErr := make(chan error, 1)
	go func() {
		err := o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: "gh", Args: []string{"release", "download", "v" + o.HelmctlVersion, "--repo", "truvity/ocictl",
			"--pattern", fmt.Sprintf("helmctl_%s_linux_%s.tar.gz", o.HelmctlVersion, arch), "--output", "-"}, Stdout: pw, Stderr: o.Err})
		pw.CloseWithError(err)
		ghErr <- err
	}()
	tarErr := o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: "tar", Args: []string{"-xz", "-C", helmctlDir, "helmctl"}, Stdin: pr, Stdout: o.Out, Stderr: o.Err})
	pr.Close()
	if err := <-ghErr; err != nil {
		return fmt.Errorf("gh release download: %w", err)
	}
	if tarErr != nil {
		return fmt.Errorf("tar: %w", tarErr)
	}

	// chart-registry arrives as one oci:// URL; helmctl push takes the
	// registry host and the full chart path separately.
	ref := strings.TrimPrefix(o.Registry, "oci://")
	registry, _, _ := strings.Cut(ref, "/")
	repoPrefix := ref
	if i := strings.Index(ref, "/"); i >= 0 {
		repoPrefix = ref[i+1:]
	}

	charts, err := Resolve(o.Charts, o.ChartRoot)
	if err != nil {
		fmt.Fprintf(o.Err, "::error::%v\n", err)
		return ErrFailed
	}
	helmctl := filepath.Join(helmctlDir, "helmctl")
	run := func(name string, args ...string) error {
		return o.Exec(ctx, runcmd.Cmd{Dir: o.Dir, Name: name, Args: args, Stdout: o.Out, Stderr: o.Err})
	}
	for _, c := range charts {
		fmt.Fprintf(o.Out, "::group::%s %s\n", c.Name, version)
		// helmctl's --app-version is optional: omitting it leaves the chart's
		// declared appVersion untouched. Anything unrecognised is rejected
		// rather than falling back to "release": a typo would reproduce the
		// silent mis-stamp this option exists to prevent.
		var appVersion []string
		switch o.AppVersionMode {
		case "release":
			appVersion = []string{"--app-version", version}
		case "chart":
		default:
			fmt.Fprintf(o.Out, "::error::chart-app-version must be 'release' or 'chart', got '%s'\n", o.AppVersionMode)
			return ErrFailed
		}
		// A manifest carries the version, the appVersion and the values to
		// bake in, so it REPLACES the two version flags.
		var pkg []string
		if o.ChartImages == "goreleaser" {
			manifest := filepath.Join(o.RunnerTemp, c.Name+"-manifest.yaml")
			if err := run(helmctl, "goreleaser-manifest", "--goreleaser-dist", "dist", "--output", manifest); err != nil {
				return fmt.Errorf("helmctl goreleaser-manifest: %w", err)
			}
			pkg = []string{"--manifest", manifest}
		} else {
			pkg = append([]string{"--version", version}, appVersion...)
		}
		if o.RequireDigests == "true" {
			pkg = append(pkg, "--require-image-digests")
		}
		args := append([]string{"run", "--", helmctl, "package", "--chart", c.Dir}, pkg...)
		if err := run("devbox", append(args, "--output", "dist/")...); err != nil {
			return fmt.Errorf("helmctl package: %w", err)
		}
		if err := run(helmctl, "push", "--tgz", "dist/"+c.Name+"-"+version+".tgz", "--registry", registry,
			"--repository", repoPrefix+"/"+c.Name, "--name", c.Name, "--version", version); err != nil {
			return fmt.Errorf("helmctl push: %w", err)
		}
		fmt.Fprintln(o.Out, "::endgroup::")
	}
	return nil
}
