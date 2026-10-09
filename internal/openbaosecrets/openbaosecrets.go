// Package openbaosecrets is the port of the openbao-secrets action: read one
// path of secrets from an OpenBAO KV v2 mount, as the JOB, with no stored
// credential anywhere.
//
// The chain is the one a laptop already uses, with the job's own identity in
// place of a sign-in:
//
//  1. `sluisctl token --audience <audience>` exchanges the job's GitHub OIDC
//     identity token at the sluis issuer. The issuer's grants
//     decide: a job reaches this audience only if its matchers (repository,
//     ref, event, workflow files) admit it.
//  2. That token logs in at `auth/<auth-mount>/login`, whose role maps the
//     token's groups onto OpenBAO's identity groups, and the policy those
//     carry decides which paths the login may read.
//  3. One read of `<mount>/data/<path>`, then the login is revoked.
//
// Nothing here is rotated, copied into a repository, or held between runs.
//
// sluisctl stays an executed command (through devbox, where its version is
// pinned beside every other tool the job uses, or from PATH). Where the job
// has no sluisctl but still has accessctl, its deprecated name, that one runs
// instead, with a warning (see exchangeCommand). The HTTP calls are Go's own.
// NO SECRET VALUE IS EVER PRINTED except as the argument of an `::add-mask::`
// command, which is how the runner is told to hide it: every value is masked,
// line by line, BEFORE it is written anywhere, so a multi-line secret is
// masked whole. Log, output and error lines are the shell version's.
package openbaosecrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Options are the action's inputs and the runner's environment.
type Options struct {
	Issuer     string
	Address    string
	KVPath     string
	Namespace  string
	Mount      string
	AuthMount  string
	Role       string
	Audience   string
	Wanted     string // keys, whitespace- or comma-separated; empty takes all
	CACert     string // PEM
	SluisctlBy string // "auto" or "direct"

	IDTokenRequestURL string // ACTIONS_ID_TOKEN_REQUEST_URL
	RunnerTemp        string
	GithubOutput      string
	Dir               string // where devbox.json is looked for; "" is the working directory

	Out, Err io.Writer
	// Token runs sluisctl and returns its stdout. Nil runs the real one.
	Token func(ctx context.Context, args []string) (string, error)
	HTTP  *http.Client // nil builds one (30s, the CA when given)
}

// ErrReported marks a failure whose ::error:: line is already written.
var ErrReported = errors.New("openbao-secrets: reported")

func (o *Options) defaults() {
	if o.Mount == "" {
		o.Mount = "kv"
	}
	if o.AuthMount == "" {
		o.AuthMount = "jwt-roster"
	}
	if o.Role == "" {
		o.Role = "roster"
	}
	if o.Audience == "" {
		o.Audience = "openbao"
	}
	if o.SluisctlBy == "" {
		o.SluisctlBy = "auto"
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
}

// Run reads the path and writes the env file and the outputs.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	out := o.Out
	fail := func(format string, a ...any) error {
		fmt.Fprintf(out, "::error::"+format+"\n", a...)
		return ErrReported
	}

	for _, f := range []struct{ name, val string }{{"ISSUER", o.Issuer}, {"ADDRESS", o.Address}, {"KV_PATH", o.KVPath}} {
		if f.val == "" {
			return fail("%s is required", f.name)
		}
	}
	if o.IDTokenRequestURL == "" {
		return fail("this job has no id-token permission; the read is an exchange of the job's own identity token")
	}

	client, err := o.httpClient()
	if err != nil {
		return fail("%v", err)
	}
	tokenFn := o.Token
	if tokenFn == nil {
		tokenFn = o.sluisctl
	}

	fmt.Fprintf(out, "exchanging this job's identity for the %s audience\n", o.Audience)
	raw, err := tokenFn(ctx, []string{"token", "--issuer", o.Issuer, "--audience", o.Audience})
	if err != nil {
		return fail("sluisctl could not exchange this job's token for %s -- the issuer's grants decide, so check the job's matchers", o.Audience)
	}
	token := strings.ReplaceAll(lastLine(raw), "\r", "")
	if token == "" {
		return fail("sluisctl returned no token for %s", o.Audience)
	}
	mask(out, token)

	// The token goes in the BODY, never in a URL or an argument: an argument
	// is in the process list and in the shell's history. A refusal reads as
	// OpenBAO's own sentence.
	body, _ := json.Marshal(map[string]string{"role": o.Role, "jwt": token})
	status, loginBody, err := o.do(ctx, client, http.MethodPut, o.Address+"/v1/auth/"+o.AuthMount+"/login", "", body)
	if err != nil {
		fmt.Fprintln(o.Err, err)
	}
	if err != nil || status >= 400 {
		return fail("login at %s failed: %s", o.AuthMount, strings.TrimRight(string(loginBody), "\n"))
	}
	var login struct {
		Auth struct {
			ClientToken string   `json:"client_token"`
			Policies    []string `json:"policies"`
		} `json:"auth"`
	}
	_ = json.Unmarshal(loginBody, &login)
	clientToken := login.Auth.ClientToken
	if clientToken == "" {
		return fail("%s/login returned no token", o.AuthMount)
	}
	mask(out, clientToken)
	fmt.Fprintf(out, "logged in at %s as role %s; policies: %s\n", o.AuthMount, o.Role, strings.Join(login.Auth.Policies, ", "))

	// Revoked whatever happens next: a read that fails must not leave a
	// usable login behind for the rest of the job.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _, _ = o.do(rctx, client, http.MethodPost, o.Address+"/v1/auth/token/revoke-self", clientToken, nil)
	}()

	status, readBody, err := o.do(ctx, client, http.MethodGet, o.Address+"/v1/"+o.Mount+"/data/"+o.KVPath, clientToken, nil)
	if err != nil {
		fmt.Fprintln(o.Err, err)
	}
	if err != nil || status >= 400 {
		return fail("reading %s/%s failed: %s", o.Mount, o.KVPath, strings.TrimRight(string(readBody), "\n"))
	}
	data := map[string]json.RawMessage{}
	var rb struct {
		Data struct {
			Data json.RawMessage `json:"data"`
		} `json:"data"`
	}
	if json.Unmarshal(readBody, &rb) == nil && len(rb.Data.Data) > 0 {
		_ = json.Unmarshal(rb.Data.Data, &data)
	}
	if len(data) == 0 {
		return fail("%s/%s holds no keys", o.Mount, o.KVPath)
	}

	var keys []string
	if strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "\v", "", "\f", "", ",", "").Replace(o.Wanted) != "" {
		keys = strings.Fields(strings.ReplaceAll(o.Wanted, ",", " "))
		for _, k := range keys {
			if _, ok := data[k]; !ok {
				return fail("%s/%s has no key %s", o.Mount, o.KVPath, k)
			}
		}
	} else {
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}

	tmp := o.RunnerTemp
	if tmp == "" {
		tmp = os.TempDir()
	}
	envFile := filepath.Join(tmp, "openbao-secrets-"+nonAlnumToDash(o.KVPath)+".env")
	f, err := os.OpenFile(envFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.Chmod(envFile, 0o600); err != nil {
		return err
	}

	count := 0
	single := ""
	for _, key := range keys {
		if key == "" {
			continue
		}
		value := strings.TrimRight(rawValue(data[key]), "\n") // command substitution strips trailing newlines
		mask(out, value)

		name := strings.ReplaceAll(asciiUpper(key), "-", "_")
		if !validEnvName(name) {
			return fail("key %s is no environment variable name", key)
		}
		// Single quotes, with any quote in the value escaped: a value is
		// data, and a file that is sourced would otherwise run it.
		if _, err := fmt.Fprintf(f, "%s='%s'\n", name, strings.ReplaceAll(value, "'", `'\''`)); err != nil {
			return err
		}
		single = value
		count++
	}
	fmt.Fprintf(out, "read %d key(s) from %s/%s into $RUNNER_TEMP\n", count, o.Mount, o.KVPath)

	if o.GithubOutput != "" {
		var b bytes.Buffer
		fmt.Fprintf(&b, "env-file=%s\n", envFile)
		if count == 1 {
			// A heredoc delimiter that cannot occur in the value.
			var r [16]byte
			if _, err := rand.Read(r[:]); err != nil {
				return err
			}
			d := "openbao-" + hex.EncodeToString(r[:])
			fmt.Fprintf(&b, "value<<%s\n%s\n%s\n", d, single, d)
		}
		of, err := os.OpenFile(o.GithubOutput, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		if _, err := of.Write(b.Bytes()); err != nil {
			of.Close()
			return err
		}
		return of.Close()
	}
	return nil
}

// mask tells the runner to hide a value, one command per line: ::add-mask::
// takes one line, so a multi-line secret masked as a whole would still print
// line by line. A line shorter than four characters is not masked (it would
// hide ordinary words).
func mask(out io.Writer, s string) {
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if utf8.RuneCountInString(line) >= 4 {
			fmt.Fprintf(out, "::add-mask::%s\n", line)
		}
	}
}

// lastLine is `tail -n1`: devbox writes "Info:" chatter to stdout, so only
// the last line is the token.
func lastLine(s string) string {
	s = strings.TrimSuffix(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func nonAlnumToDash(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			b[i] = '-'
		}
	}
	return string(b)
}

func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func validEnvName(n string) bool {
	if n == "" || !(n[0] >= 'A' && n[0] <= 'Z' || n[0] == '_') {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// rawValue is `jq -r '.[$k]'`: a string as it is, anything else as JSON
// (an object or array pretty-printed).
func rawValue(raw json.RawMessage) string {
	var s string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") == nil {
		return b.String()
	}
	return string(raw)
}

func (o Options) httpClient() (*http.Client, error) {
	if o.HTTP != nil {
		return o.HTTP, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.CACert != "" {
		// like curl --cacert: only this authority is trusted. A certificate
		// authority is not a secret; it still comes from the caller.
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(o.CACert)) {
			return nil, errors.New("ca-cert holds no PEM certificate")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}, nil
}

// do is one request: the namespace (when given) and a short timeout, and the
// body of an HTTP error handed back rather than swallowed.
func (o Options) do(ctx context.Context, c *http.Client, method, url, vaultToken string, body []byte) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", o.Namespace)
	}
	if vaultToken != "" {
		req.Header.Set("X-Vault-Token", vaultToken)
	}
	resp, err := c.Do(req)
	if err != nil {
		// never the request: it carries the token
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		return 0, nil, fmt.Errorf("%s %s: %v", method, stripQuery(url), err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, err
}

func stripQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// cliName is the token exchange's command, and legacyCLIName its deprecated
// name: the same binary (sluis is the renamed access-roster), which still
// works and prints a deprecation notice on stderr.
//
// TODO(remove): the accessctl fallback, once every consumer's devbox and
// every runner image carry sluisctl.
const (
	cliName       = "sluisctl"
	legacyCLIName = "accessctl"
)

// sluisctl runs the real one. It comes from the repository's devbox, where
// its version is pinned beside every other tool the job uses.
func (o Options) sluisctl(ctx context.Context, args []string) (string, error) {
	name, argv := o.exchangeCommand(ctx, args)
	return o.capture(ctx, name, argv)
}

// exchangeCommand is the command line the exchange runs: through devbox when
// the repository has a devbox.json (unless direct), from PATH otherwise.
//
// sluisctl is preferred. A job whose PATH (devbox's, when through devbox) has
// only accessctl runs that, with a warning, so that moving this action to the
// new name breaks no repository that has not moved yet. A PATH with neither
// runs sluisctl, which fails as plainly as it always did.
func (o Options) exchangeCommand(ctx context.Context, args []string) (string, []string) {
	viaDevbox := false
	if o.SluisctlBy != "direct" {
		if _, err := os.Stat(filepath.Join(o.Dir, "devbox.json")); err == nil {
			viaDevbox = true
		}
	}
	path := os.Getenv("PATH")
	if viaDevbox {
		// devbox's PATH, as the command will see it. Only the last line:
		// devbox may print its own chatter first. A failure keeps the job's
		// PATH, which still finds whatever the runner image bakes.
		if out, err := o.capture(ctx, "devbox", []string{"run", "--", "printenv", "PATH"}); err == nil {
			if p := strings.TrimSpace(lastLine(out)); p != "" {
				path = p
			}
		}
	}
	name := cliName
	if !onPath(path, cliName) && onPath(path, legacyCLIName) {
		name = legacyCLIName
		fmt.Fprintf(o.Out, "::warning::no %s on PATH, running %s, its deprecated name; add %s to this repository's devbox\n",
			cliName, legacyCLIName, cliName)
	}
	if viaDevbox {
		return "devbox", append([]string{"run", "--", name}, args...)
	}
	return name, args
}

// onPath reports whether name is an executable file in one of path's
// directories, the way exec.LookPath would find it.
func onPath(path, name string) bool {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}

// capture runs name in o.Dir and returns its stdout; stderr goes to o.Err.
func (o Options) capture(ctx context.Context, name string, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = o.Dir
	cmd.Stderr = o.Err
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}
