package setupdevbox

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The key material is a marker, not a key: buildkittls checks PEM block types,
// and these tests check that the marker never reaches a log line.
const keyMarker = "c2VjcmV0LWtleS1tYXJrZXI"

func pemBlock(typ string, body string) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: []byte(body)}))
}

// moonH is a harness for the step: a workspace that uses moon, a grpcs
// address, and a runner that provides nothing yet.
func moonH(t *testing.T) *h {
	t.Helper()
	x := newH(t)
	x.env["BAZEL_REMOTE"] = "grpcs://cache.example:443"
	if err := os.MkdirAll(filepath.Join(x.env["GITHUB_WORKSPACE"], ".moon"), 0o755); err != nil {
		t.Fatal(err)
	}
	return x
}

// trioDir writes the directory form, with the given files' contents.
func trioDir(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func fullTrio() map[string]string {
	return map[string]string{"ca.pem": pemBlock("CERTIFICATE", "ca"), "cert.pem": pemBlock("CERTIFICATE", "cert"), "key.pem": pemBlock("PRIVATE KEY", keyMarker)}
}

func pemEnv(x *h) {
	x.env["CI_BUILDKIT_TLS_CA_PEM"] = pemBlock("CERTIFICATE", "ca")
	x.env["CI_BUILDKIT_TLS_CERT_PEM"] = pemBlock("CERTIFICATE", "cert")
	x.env["CI_BUILDKIT_TLS_KEY_PEM"] = pemBlock("PRIVATE KEY", keyMarker)
}

// exported parses GITHUB_ENV into name -> value.
func exported(t *testing.T, x *h) map[string]string {
	t.Helper()
	got := map[string]string{}
	rest := x.file("github_env")
	for rest != "" {
		head, _, ok := strings.Cut(rest, "\n")
		if !ok {
			t.Fatalf("unterminated GITHUB_ENV: %q", rest)
		}
		_, delim, _ := strings.Cut(head, "<<")
		end := strings.Index(rest, "\n"+delim+"\n")
		if end < 0 {
			t.Fatalf("no closing delimiter in %q", rest)
		}
		block := rest[:end+len(delim)+2]
		name, value := envBlock(t, block)
		got[name] = value
		rest = rest[len(block):]
	}
	return got
}

// keyLine is the base64 line of the key PEM: what a leak would print.
var keyLine = strings.Split(pemBlock("PRIVATE KEY", keyMarker), "\n")[1]

func noLeak(t *testing.T, x *h) {
	t.Helper()
	for _, line := range strings.Split(x.out.String()+x.err.String(), "\n") {
		if strings.Contains(line, keyLine) && !strings.HasPrefix(line, "::add-mask::") {
			t.Fatalf("the key reached a log line: %q", line)
		}
		if strings.Contains(line, "BEGIN") {
			t.Fatalf("PEM text reached a log line: %q", line)
		}
	}
}

func TestMoonRemoteMTLSOn(t *testing.T) {
	t.Run("from CI_BUILDKIT_TLS_DIR", func(t *testing.T) {
		x := moonH(t)
		d := trioDir(t, fullTrio())
		x.env["CI_BUILDKIT_TLS_DIR"] = d
		// The directory wins over the PEM form, as in setup-buildkit-tls.
		pemEnv(x)
		if err := MoonRemoteMTLS(x.e()); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			MoonMTLSCACert:     filepath.Join(d, "ca.pem"),
			MoonMTLSClientCert: filepath.Join(d, "cert.pem"),
			MoonMTLSClientKey:  filepath.Join(d, "key.pem"),
		}
		if got := exported(t, x); len(got) != 3 || got[MoonMTLSCACert] != want[MoonMTLSCACert] || got[MoonMTLSClientCert] != want[MoonMTLSClientCert] || got[MoonMTLSClientKey] != want[MoonMTLSClientKey] {
			t.Fatalf("exported %v, want %v", got, want)
		}
		if !strings.Contains(x.out.String(), "moon remote mTLS: on for grpcs://cache.example:443, client certificate from CI_BUILDKIT_TLS_DIR") {
			t.Fatalf("log: %s", x.out.String())
		}
		noLeak(t, x)
	})

	t.Run("from the PEM variables", func(t *testing.T) {
		x := moonH(t)
		pemEnv(x)
		if err := MoonRemoteMTLS(x.e()); err != nil {
			t.Fatal(err)
		}
		got := exported(t, x)
		if len(got) != 3 {
			t.Fatalf("exported %v, want the three moon variables only", got)
		}
		if _, ok := got["CI_BUILDKIT_TLS_DIR"]; ok {
			t.Fatal("CI_BUILDKIT_TLS_DIR exported: the builders' transport is not this step's to change")
		}
		dir := filepath.Dir(got[MoonMTLSCACert])
		if !strings.HasPrefix(dir, x.env["RUNNER_TEMP"]+string(filepath.Separator)) {
			t.Fatalf("written to %s, not under RUNNER_TEMP", dir)
		}
		if got[MoonMTLSClientCert] != filepath.Join(dir, "cert.pem") || got[MoonMTLSClientKey] != filepath.Join(dir, "key.pem") {
			t.Fatalf("exported %v", got)
		}
		if b, _ := os.ReadFile(got[MoonMTLSClientKey]); !strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("key.pem holds %q", b)
		}
		if fi, _ := os.Stat(got[MoonMTLSClientKey]); fi.Mode().Perm() != 0o600 {
			t.Fatalf("key.pem mode %v", fi.Mode().Perm())
		}
		if !strings.Contains(x.out.String(), "::add-mask::") {
			t.Fatal("the key was not masked")
		}
		if !strings.Contains(x.out.String(), "moon remote mTLS: on for grpcs://cache.example:443, client certificate from the runner's CI_BUILDKIT_TLS_CA_PEM") {
			t.Fatalf("log: %s", x.out.String())
		}
		noLeak(t, x)
	})

	t.Run("the scheme is case-insensitive and userinfo is not logged", func(t *testing.T) {
		x := moonH(t)
		x.env["BAZEL_REMOTE"] = "GRPCS://user:pw@cache.example:443"
		x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, fullTrio())
		if err := MoonRemoteMTLS(x.e()); err != nil {
			t.Fatal(err)
		}
		if len(exported(t, x)) != 3 {
			t.Fatalf("not exported: %s", x.out.String())
		}
		if strings.Contains(x.out.String(), "pw@") {
			t.Fatalf("userinfo logged: %s", x.out.String())
		}
	})
}

func TestMoonRemoteMTLSOff(t *testing.T) {
	for name, tc := range map[string]struct {
		setup   func(t *testing.T, x *h)
		log     string // the "off (...)" reason
		warning string // "" is no warning at all
	}{
		"no address": {
			setup: func(t *testing.T, x *h) {
				x.env["BAZEL_REMOTE"] = ""
				x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, fullTrio())
			},
			log: "off (no bazel-remote address)",
		},
		"plain grpc": {
			setup: func(t *testing.T, x *h) {
				x.env["BAZEL_REMOTE"] = "grpc://cache.example:9092"
				x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, fullTrio())
			},
			log: "off (the bazel-remote address grpc://cache.example:9092 is not grpcs://)",
		},
		"https is not grpcs": {
			setup: func(t *testing.T, x *h) {
				x.env["BAZEL_REMOTE"] = "https://cache.example"
				x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, fullTrio())
			},
			log: "is not grpcs://",
		},
		"no moon in the repository": {
			setup: func(t *testing.T, x *h) {
				_ = os.RemoveAll(filepath.Join(x.env["GITHUB_WORKSPACE"], ".moon"))
				x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, fullTrio())
			},
			log: "off (the repository does not use moon",
		},
		"the job set its own": {
			setup: func(t *testing.T, x *h) {
				x.env[MoonMTLSClientKey] = "/elsewhere/key.pem"
				x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, fullTrio())
			},
			log: "off (MOON_REMOTE_MTLS_CLIENT_KEY is already set",
		},
		"no certificate on the runner": {
			setup: func(t *testing.T, x *h) {},
			log:   "off (the runner provides no BuildKit client certificate)",
		},
		"directory without key.pem": {
			setup: func(t *testing.T, x *h) {
				f := fullTrio()
				delete(f, "key.pem")
				x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, f)
			},
			log:     "key.pem does not exist)",
			warning: "::warning::moon remote mTLS: ",
		},
		"directory with an empty cert.pem": {
			setup: func(t *testing.T, x *h) {
				f := fullTrio()
				f["cert.pem"] = ""
				x.env["CI_BUILDKIT_TLS_DIR"] = trioDir(t, f)
			},
			log:     "cert.pem is empty)",
			warning: "::warning::moon remote mTLS: ",
		},
		"directory whose ca.pem is a directory": {
			setup: func(t *testing.T, x *h) {
				f := fullTrio()
				delete(f, "ca.pem")
				d := trioDir(t, f)
				_ = os.Mkdir(filepath.Join(d, "ca.pem"), 0o755)
				x.env["CI_BUILDKIT_TLS_DIR"] = d
			},
			log:     "ca.pem is not a regular file)",
			warning: "::warning::moon remote mTLS: ",
		},
		"relative directory": {
			setup:   func(t *testing.T, x *h) { x.env["CI_BUILDKIT_TLS_DIR"] = "certs" },
			log:     "off (CI_BUILDKIT_TLS_DIR is not an absolute path)",
			warning: "::warning::moon remote mTLS: CI_BUILDKIT_TLS_DIR is not an absolute path",
		},
		"only some PEM variables": {
			setup: func(t *testing.T, x *h) {
				pemEnv(x)
				delete(x.env, "CI_BUILDKIT_TLS_CA_PEM")
			},
			log:     "off (the runner's BuildKit client certificate is not usable",
			warning: "::warning::moon remote mTLS: the runner sets CI_BUILDKIT_TLS_CERT_PEM, CI_BUILDKIT_TLS_KEY_PEM but not CI_BUILDKIT_TLS_CA_PEM",
		},
		"a PEM variable of the wrong type": {
			setup: func(t *testing.T, x *h) {
				pemEnv(x)
				x.env["CI_BUILDKIT_TLS_KEY_PEM"] = pemBlock("CERTIFICATE", keyMarker)
			},
			log:     "off (the runner's BuildKit client certificate is not usable",
			warning: "::warning::moon remote mTLS: CI_BUILDKIT_TLS_KEY_PEM holds a \"CERTIFICATE\" block",
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := moonH(t)
			tc.setup(t, x)
			if err := MoonRemoteMTLS(x.e()); err != nil {
				t.Fatalf("the step failed (it is fail-open): %v\n%s", err, x.out.String())
			}
			if got := x.file("github_env"); got != "" {
				t.Fatalf("exported %q; nothing may be exported in this case", got)
			}
			out := x.out.String()
			if !strings.Contains(out, "moon remote mTLS: off (") || !strings.Contains(out, tc.log) {
				t.Fatalf("log does not name the case %q:\n%s", tc.log, out)
			}
			if strings.Contains(out, "::error::") {
				t.Fatalf("an ::error:: line in a fail-open step:\n%s", out)
			}
			if tc.warning == "" && strings.Contains(out, "::warning::") {
				t.Fatalf("unexpected warning:\n%s", out)
			}
			if tc.warning != "" && !strings.Contains(out, tc.warning) {
				t.Fatalf("no warning %q:\n%s", tc.warning, out)
			}
			// A rejected PEM form leaves no directory holding the key behind.
			if ents, _ := os.ReadDir(x.env["RUNNER_TEMP"]); len(ents) != 0 {
				t.Fatalf("RUNNER_TEMP holds %v after an off case", ents)
			}
			noLeak(t, x)
		})
	}
}
