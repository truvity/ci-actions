package setupdevbox

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/truvity/ci-actions/internal/buildkittls"
	"github.com/truvity/ci-actions/internal/runnerenv"
)

// The variables moon reads for the mTLS client identity of its remote cache,
// in 2.5.x and 2.6.x alike: RemoteMtlsConfig in moon's
// crates/config/src/workspace/remote_config.rs carries
// #[config(env_prefix = "MOON_REMOTE_MTLS_")], and schematic derives each name
// from that prefix and the Rust field name (ca_cert, client_cert,
// client_key), upper-cased. Any one of them set makes remote.mtls present,
// and mTLS then takes precedence over remote.tls.
const (
	MoonMTLSCACert     = "MOON_REMOTE_MTLS_CA_CERT"
	MoonMTLSClientCert = "MOON_REMOTE_MTLS_CLIENT_CERT"
	MoonMTLSClientKey  = "MOON_REMOTE_MTLS_CLIENT_KEY"
)

// MoonRemoteMTLS hands moon the runner's BuildKit client certificate as the
// client identity of its remote cache, so a bazel-remote that requires a
// client certificate for writes accepts the job's.
//
// The bazel-remote address is the one "Resolve the cache endpoints" decided
// (BAZEL_REMOTE, its bazel-remote output; the job hands it to moon as
// MOON_REMOTE_HOST). The three variables are exported to the later steps only
// when every one of these holds; otherwise nothing is exported and the log
// line names the case:
//
//   - the address is grpcs:// (mTLS means nothing to a plain grpc:// or an
//     http(s) one);
//   - the repository uses moon (.moon in the workspace);
//   - the job has not set any MOON_REMOTE_MTLS_* itself (it is left as is);
//   - the runner provides the certificate, in either form setup-buildkit-tls
//     accepts: CI_BUILDKIT_TLS_DIR, or the three CI_BUILDKIT_TLS_*_PEM
//     variables, which are then written to a fresh directory under
//     RUNNER_TEMP by the same code (internal/buildkittls). That directory is
//     NOT exported as CI_BUILDKIT_TLS_DIR: how the remote builders are
//     registered stays setup-buildkit-tls's decision;
//   - ca.pem, cert.pem and key.pem in that directory are regular files that
//     are not empty.
//
// All three or none, because moon treats a partial set as a configuration
// error, and a set that names a missing file as a connection failure that
// disables the remote cache for the whole run: exporting without the files
// would turn a cache that reads into no cache at all.
//
// Fail-open, like the other cache wiring: a certificate that is incomplete or
// not PEM is a ::warning:: and the job runs with moon's remote cache as it was
// configured before this step. The key is masked before anything is written,
// and no value of a certificate or key is ever printed; the log names the
// source and the paths only.
func MoonRemoteMTLS(e Env) error {
	e.defaults()
	addr := strings.TrimSpace(e.Getenv("BAZEL_REMOTE"))
	off := func(why string) error {
		e.printf("moon remote mTLS: off (%s)\n", why)
		return nil
	}
	if addr == "" {
		return off("no bazel-remote address")
	}
	if !strings.HasPrefix(strings.ToLower(addr), "grpcs://") {
		return off(fmt.Sprintf("the bazel-remote address %s is not grpcs://", runnerenv.Redact(addr)))
	}
	if !isDir(filepath.Join(e.Getenv("GITHUB_WORKSPACE"), ".moon")) {
		return off("the repository does not use moon: no .moon in the workspace")
	}
	for _, name := range []string{MoonMTLSCACert, MoonMTLSClientCert, MoonMTLSClientKey} {
		if strings.TrimSpace(e.Getenv(name)) != "" {
			return off(name + " is already set; the job's own moon mTLS settings are left as they are")
		}
	}

	// The same rule setup-buildkit-tls applies, with its ::error:: lines turned
	// into warnings: here a bad certificate costs moon its client identity, not
	// the job. Its own log line is not wanted; this step writes one.
	src, dir, err := buildkittls.Run(buildkittls.Options{
		Getenv:     e.Getenv,
		RunnerTemp: e.Getenv("RUNNER_TEMP"),
		Out:        warnInsteadOfError{e.Out},
		Err:        io.Discard,
	})
	var from string
	switch {
	case err != nil:
		return off("the runner's BuildKit client certificate is not usable, see the warning above")
	case src == buildkittls.None:
		return off("the runner provides no BuildKit client certificate")
	case src == buildkittls.FromDir:
		dir = strings.TrimSpace(e.Getenv(buildkittls.DirVar))
		from = buildkittls.DirVar
	default:
		from = "the runner's " + buildkittls.CAVar + ", " + buildkittls.CertVar + " and " + buildkittls.KeyVar + ", written to " + dir
	}

	if !filepath.IsAbs(dir) {
		e.printf("::warning::moon remote mTLS: %s is not an absolute path; moon's remote cache keeps no client certificate\n", buildkittls.DirVar)
		return off(buildkittls.DirVar + " is not an absolute path")
	}
	files := []struct{ name, env string }{
		{buildkittls.CAFile, MoonMTLSCACert},
		{buildkittls.CertFile, MoonMTLSClientCert},
		{buildkittls.KeyFile, MoonMTLSClientKey},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if why := notUsable(p); why != "" {
			e.printf("::warning::moon remote mTLS: %s %s; moon's remote cache keeps no client certificate\n", p, why)
			return off(p + " " + why)
		}
	}
	for _, f := range files {
		if err := e.exportEnv(f.env, filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	e.printf("moon remote mTLS: on for %s, client certificate from %s (%s, %s, %s)\n",
		runnerenv.Redact(addr), from, MoonMTLSCACert, MoonMTLSClientCert, MoonMTLSClientKey)
	return nil
}

// notUsable says why p cannot serve moon as a PEM file, or "" when it can: it
// must be a regular file (after symlinks, as a mounted Secret's files are),
// not empty, and readable by this user.
func notUsable(p string) string {
	fi, err := os.Stat(p)
	switch {
	case os.IsNotExist(err):
		return "does not exist"
	case err != nil:
		return "cannot be read: " + unwrapPathErr(err).Error()
	case !fi.Mode().IsRegular():
		return "is not a regular file"
	case fi.Size() == 0:
		return "is empty"
	}
	f, err := os.Open(p)
	if err != nil {
		return "cannot be read: " + unwrapPathErr(err).Error()
	}
	f.Close()
	return ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func unwrapPathErr(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err
	}
	return err
}

// warnInsteadOfError passes workflow commands through (::add-mask:: above
// all), except that an ::error:: line becomes a ::warning:: naming this step.
// buildkittls writes each command with one Fprintf, so each Write is one line.
type warnInsteadOfError struct{ w io.Writer }

func (d warnInsteadOfError) Write(p []byte) (int, error) {
	const errCmd = "::error::"
	if !bytes.HasPrefix(p, []byte(errCmd)) {
		return d.w.Write(p)
	}
	line := append([]byte("::warning::moon remote mTLS: "), p[len(errCmd):]...)
	if _, err := d.w.Write(line); err != nil {
		return 0, err
	}
	return len(p), nil
}
