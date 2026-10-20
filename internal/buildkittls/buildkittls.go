// Package buildkittls is the port of the setup-buildkit-tls action: decide
// where the runner's BuildKit client certificate comes from and, when it was
// handed over as PEM text in the environment, put it in files.
//
// A runner provides the client certificate for the CI plane's remote
// builders in one of two forms:
//
//	CI_BUILDKIT_TLS_DIR                      a directory holding ca.pem,
//	                                         cert.pem and key.pem (an ARC pod
//	                                         mounts a Secret there)
//	CI_BUILDKIT_TLS_CA_PEM, _CERT_PEM,       the three PEM documents as
//	CI_BUILDKIT_TLS_KEY_PEM                  values (an ECS task secret)
//
// The directory wins and nothing else happens. Without it, all three PEM
// values are written to a fresh directory under RUNNER_TEMP, named exactly as
// the directory form is (ca.pem, cert.pem, key.pem), and that directory is
// exported as CI_BUILDKIT_TLS_DIR to the later steps of the job, so every
// consumer of the directory form works unchanged. Some but not all of the
// three is an error: a half certificate is a misconfiguration, never plain TCP.
//
// The private key is a secret. It is registered with ::add-mask:: line by
// line before anything else is written, and no function here puts a value
// of the environment into a message: errors name the variable and the kind of
// PEM block only.
package buildkittls

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Names of the variables a runner may export.
const (
	DirVar  = "CI_BUILDKIT_TLS_DIR"
	CAVar   = "CI_BUILDKIT_TLS_CA_PEM"
	CertVar = "CI_BUILDKIT_TLS_CERT_PEM"
	KeyVar  = "CI_BUILDKIT_TLS_KEY_PEM"
)

// File names inside the directory: the ones every consumer of
// CI_BUILDKIT_TLS_DIR expects.
const (
	CAFile   = "ca.pem"
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
)

// Source says where the TLS material came from.
type Source string

const (
	// FromDir is CI_BUILDKIT_TLS_DIR, used as it is.
	FromDir Source = "dir"
	// FromEnv is the three PEM variables, written to a fresh directory.
	FromEnv Source = "env"
	// None means the runner provided no TLS material.
	None Source = "none"
)

// Options are the runner's environment and the step's outputs.
type Options struct {
	Getenv     func(string) string // the runner's environment
	RunnerTemp string              // the fresh directory goes under this
	GithubEnv  string              // $GITHUB_ENV; empty skips the export
	Out        io.Writer           // workflow commands (::add-mask::, ::error::)
	Err        io.Writer           // log lines
}

// ErrFailed marks a failure whose ::error:: line is already written.
var ErrFailed = errors.New("buildkit-tls: failed")

// plainPath is what the consumers of CI_BUILDKIT_TLS_DIR accept.
var plainPath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// Run resolves the TLS material and, for the PEM form, writes it out. It
// returns the source that won and, for FromEnv, the directory written.
func Run(o Options) (Source, string, error) {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	fail := func(format string, a ...any) (Source, string, error) {
		fmt.Fprintf(o.Out, "::error::"+format+"\n", a...)
		return None, "", ErrFailed
	}
	set := func(name string) bool { return strings.TrimSpace(o.Getenv(name)) != "" }

	if set(DirVar) {
		fmt.Fprintf(o.Err, "buildkit tls: TLS material from %s\n", DirVar)
		return FromDir, "", nil
	}

	pems := []struct {
		name string
		file string
	}{{CAVar, CAFile}, {CertVar, CertFile}, {KeyVar, KeyFile}}
	// The key is registered first: nothing below may be written, or fail,
	// before the runner knows not to print it.
	key := o.Getenv(KeyVar)
	if set(KeyVar) {
		maskKey(o.Out, key)
	}
	var present, missing []string
	for _, p := range pems {
		if set(p.name) {
			present = append(present, p.name)
		} else {
			missing = append(missing, p.name)
		}
	}
	switch {
	case len(present) == 0:
		fmt.Fprintf(o.Err, "buildkit tls: no TLS material (neither %s nor the three PEM variables are set)\n", DirVar)
		return None, "", nil
	case len(missing) > 0:
		return fail("the runner sets %s but not %s: provide all of %s, %s and %s, or %s, or none",
			strings.Join(present, ", "), strings.Join(missing, ", "), CAVar, CertVar, KeyVar, DirVar)
	}

	ca, err := normalise(CAVar, o.Getenv(CAVar), certificateOnly)
	if err != nil {
		return fail("%v", err)
	}
	cert, err := normalise(CertVar, o.Getenv(CertVar), certificateOnly)
	if err != nil {
		return fail("%v", err)
	}
	keyPEM, err := normalise(KeyVar, key, privateKey)
	if err != nil {
		return fail("%v", err)
	}

	if o.RunnerTemp == "" {
		return fail("RUNNER_TEMP is not set: nowhere to write the TLS material")
	}
	dir, err := os.MkdirTemp(o.RunnerTemp, "buildkit-tls-")
	if err != nil {
		return fail("cannot create a directory for the TLS material under RUNNER_TEMP: %v", unwrapPath(err))
	}
	// A directory that did not end up fully written is removed: it may hold
	// the key.
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(dir)
		}
	}()
	if !plainPath.MatchString(dir) {
		return fail("the directory %s for the TLS material is not a plain absolute path", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fail("cannot restrict %s: %v", dir, unwrapPath(err))
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{{CAFile, ca, 0o644}, {CertFile, cert, 0o644}, {KeyFile, keyPEM, 0o600}}
	for _, f := range files {
		if err := writeFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return fail("cannot write %s: %v", filepath.Join(dir, f.name), unwrapPath(err))
		}
	}

	if o.GithubEnv != "" {
		ef, err := os.OpenFile(o.GithubEnv, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return fail("cannot export %s: %v", DirVar, unwrapPath(err))
		}
		_, werr := fmt.Fprintf(ef, "%s=%s\n", DirVar, dir)
		if cerr := ef.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fail("cannot export %s: %v", DirVar, unwrapPath(werr))
		}
	}
	done = true
	fmt.Fprintf(o.Err, "buildkit tls: TLS material from runner env (%s, %s, %s), written to %s as %s, %s and %s\n",
		CAVar, CertVar, KeyVar, dir, CAFile, CertFile, KeyFile)
	return FromEnv, dir, nil
}

// maskKey registers the key with the runner, one line at a time: the runner
// masks a multi-line value only line by line. The armor lines carry no
// secret and are left out, so the log stays readable.
func maskKey(out io.Writer, key string) {
	for _, line := range strings.FieldsFunc(key, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}
		fmt.Fprintf(out, "::add-mask::%s\n", line)
	}
}

// kind says which PEM block types a variable may hold.
type kind struct {
	what  string
	types map[string]bool
	max   int // most blocks; 0 is unlimited
}

var (
	certificateOnly = kind{what: "certificate", types: map[string]bool{"CERTIFICATE": true}}
	privateKey      = kind{what: "private key", types: map[string]bool{"PRIVATE KEY": true, "RSA PRIVATE KEY": true, "EC PRIVATE KEY": true}, max: 1}
)

// normalise checks that v is nothing but PEM blocks of the wanted type and
// returns them re-encoded: LF line ends, 64-column lines, no stray text. An
// error names the variable and what was wrong, never the value.
func normalise(name, v string, k kind) ([]byte, error) {
	rest := []byte(strings.TrimSpace(v))
	if !bytes.HasPrefix(rest, []byte("-----BEGIN ")) {
		return nil, fmt.Errorf("%s is not PEM: expected a %s block (-----BEGIN ...-----)", name, k.what)
	}
	var out bytes.Buffer
	n := 0
	for len(bytes.TrimSpace(rest)) > 0 {
		b, r := pem.Decode(rest)
		if b == nil {
			return nil, fmt.Errorf("%s is not valid PEM after %d block(s): text that is not a complete PEM block", name, n)
		}
		if !k.types[b.Type] {
			if b.Type == "ENCRYPTED PRIVATE KEY" {
				return nil, fmt.Errorf("%s holds an encrypted private key: BuildKit needs an unencrypted one", name)
			}
			return nil, fmt.Errorf("%s holds a %q block, not a %s", name, b.Type, k.what)
		}
		if len(b.Headers) > 0 {
			if k.what == "private key" {
				return nil, fmt.Errorf("%s holds an encrypted private key: BuildKit needs an unencrypted one", name)
			}
			return nil, fmt.Errorf("%s holds a %s block with headers", name, k.what)
		}
		if len(b.Bytes) == 0 {
			return nil, fmt.Errorf("%s holds an empty %s block", name, k.what)
		}
		if err := pem.Encode(&out, b); err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		n++
		rest = r
	}
	if k.max > 0 && n > k.max {
		return nil, fmt.Errorf("%s holds %d %s blocks, expected %d", name, n, k.what, k.max)
	}
	return out.Bytes(), nil
}

// writeFile creates the file new, with exactly mode whatever the umask.
func writeFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// unwrapPath keeps the operating system's reason and drops nothing a path
// error carries that is not already in the message (it holds no value).
func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
