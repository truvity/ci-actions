package openbaosecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	jwt         = "eyJ-job-identity-token-value"
	clientToken = "hvs.CAESIclient-token-value"
)

type req struct {
	method, path string
	header       http.Header
	body         string
}

type vault struct {
	srv        *httptest.Server
	reqs       []req
	data       string // the KV data object, JSON
	loginCode  int
	loginBody  string
	readCode   int
	readBody   string
	revokeSeen bool
}

func newVault(t *testing.T, tls bool, data string) *vault {
	t.Helper()
	v := &vault{data: data}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v.reqs = append(v.reqs, req{r.Method, r.URL.Path, r.Header.Clone(), string(b)})
		switch {
		case r.URL.Path == "/v1/auth/jwt-roster/login" && r.Method == http.MethodPut:
			if v.loginCode != 0 {
				w.WriteHeader(v.loginCode)
				fmt.Fprint(w, v.loginBody)
				return
			}
			fmt.Fprintf(w, `{"auth":{"client_token":%q,"policies":["default","ci-read"]}}`, clientToken)
		case r.URL.Path == "/v1/auth/token/revoke-self":
			v.revokeSeen = true
			w.WriteHeader(204)
		case strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			if r.Header.Get("X-Vault-Token") != clientToken {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"errors":["permission denied"]}`)
				return
			}
			if v.readCode != 0 {
				w.WriteHeader(v.readCode)
				fmt.Fprint(w, v.readBody)
				return
			}
			fmt.Fprintf(w, `{"data":{"data":%s,"metadata":{"version":3}}}`, v.data)
		default:
			w.WriteHeader(404)
		}
	})
	if tls {
		v.srv = httptest.NewTLSServer(h)
	} else {
		v.srv = httptest.NewServer(h)
	}
	t.Cleanup(v.srv.Close)
	return v
}

type env struct {
	t   *testing.T
	v   *vault
	o   Options
	out strings.Builder
	err strings.Builder
}

func newEnv(t *testing.T, v *vault) *env {
	t.Helper()
	e := &env{t: t, v: v}
	e.o = Options{
		Issuer: "https://issuer.example", Address: v.srv.URL, KVPath: "ci/goreleaser",
		IDTokenRequestURL: "https://actions.example/token", RunnerTemp: t.TempDir(),
		GithubOutput: filepath.Join(t.TempDir(), "output"),
		Token:        func(context.Context, []string) (string, error) { return "Info: chatter\n" + jwt + "\n", nil },
	}
	e.o.Out, e.o.Err = &e.out, &e.err
	return e
}

func (e *env) run() error { return Run(context.Background(), e.o) }

func (e *env) envFile() string {
	b, err := os.ReadFile(filepath.Join(e.o.RunnerTemp, "openbao-secrets-ci-goreleaser.env"))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) outputs() string {
	b, _ := os.ReadFile(e.o.GithubOutput)
	return string(b)
}

// noLeak is what every test asserts: a secret appears in the log ONLY as the
// argument of an ::add-mask:: command, never in stderr and never in an error
// line.
func (e *env) noLeak(secrets ...string) {
	e.t.Helper()
	for _, l := range strings.Split(e.out.String(), "\n") {
		for _, s := range secrets {
			for _, part := range strings.Split(s, "\n") {
				if len(part) >= 4 && strings.Contains(l, part) && !strings.HasPrefix(l, "::add-mask::") {
					e.t.Errorf("secret %q printed outside ::add-mask:: in line %q", part, l)
				}
			}
		}
	}
	for _, s := range secrets {
		if strings.Contains(e.err.String(), s) {
			e.t.Errorf("secret %q printed on stderr", s)
		}
	}
}

func TestSingleValue(t *testing.T) {
	v := newVault(t, false, `{"GORELEASER_KEY":"gr-key-12345"}`)
	e := newEnv(t, v)
	e.o.Namespace = "team/ci"
	if err := e.run(); err != nil {
		t.Fatal(err, e.out.String())
	}
	wantOut := "exchanging this job's identity for the openbao audience\n" +
		"::add-mask::" + jwt + "\n::add-mask::" + clientToken + "\n" +
		"logged in at jwt-roster as role roster; policies: default, ci-read\n" +
		"::add-mask::gr-key-12345\n" +
		"read 1 key(s) from kv/ci/goreleaser into $RUNNER_TEMP\n"
	if e.out.String() != wantOut {
		t.Errorf("log:\n%s\nwant:\n%s", e.out.String(), wantOut)
	}
	if got := e.envFile(); got != "GORELEASER_KEY='gr-key-12345'\n" {
		t.Errorf("env file %q", got)
	}
	st, _ := os.Stat(filepath.Join(e.o.RunnerTemp, "openbao-secrets-ci-goreleaser.env"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("env file mode %v", st.Mode().Perm())
	}
	m := regexp.MustCompile(`(?s)^env-file=(.*)\nvalue<<(openbao-[0-9a-f]+)\ngr-key-12345\n(openbao-[0-9a-f]+)\n$`).FindStringSubmatch(e.outputs())
	if m == nil || m[2] != m[3] || !strings.HasSuffix(m[1], "openbao-secrets-ci-goreleaser.env") {
		t.Errorf("outputs %q", e.outputs())
	}
	// the exchange, the login (token in the body, namespace on every call),
	// the read, and the revocation, in that order
	var seq []string
	for _, r := range v.reqs {
		seq = append(seq, r.method+" "+r.path)
		if r.header.Get("X-Vault-Namespace") != "team/ci" {
			t.Errorf("%s lacks the namespace", r.path)
		}
	}
	if strings.Join(seq, ",") != "PUT /v1/auth/jwt-roster/login,GET /v1/kv/data/ci/goreleaser,POST /v1/auth/token/revoke-self" {
		t.Errorf("requests: %v", seq)
	}
	var login map[string]string
	_ = json.Unmarshal([]byte(v.reqs[0].body), &login)
	if login["role"] != "roster" || login["jwt"] != jwt || strings.Contains(v.reqs[0].path, jwt) {
		t.Errorf("login body %v", v.reqs[0].body)
	}
	if v.reqs[1].header.Get("X-Vault-Token") != clientToken || v.reqs[2].header.Get("X-Vault-Token") != clientToken {
		t.Error("the read and the revocation carry the login")
	}
	e.noLeak("gr-key-12345")
}

func TestManyKeys(t *testing.T) {
	multi := "-----BEGIN KEY-----\nabcdefghij\nxy\n-----END KEY-----"
	v := newVault(t, false, `{"maven-username":"deploy-bot","maven-password":"it's a \"p@ss\" $(x)","tls":`+mustJSON(multi)+`,"n":42,"flag":true,"nothing":null,"obj":{"a":1}}`)
	e := newEnv(t, v)
	if err := e.run(); err != nil {
		t.Fatal(err, e.out.String())
	}
	want := "FLAG='true'\n" +
		"MAVEN_PASSWORD='it'\\''s a \"p@ss\" $(x)'\n" +
		"MAVEN_USERNAME='deploy-bot'\n" +
		"N='42'\n" +
		"NOTHING='null'\n" +
		"OBJ='{\n  \"a\": 1\n}'\n" +
		"TLS='" + multi + "'\n"
	if got := e.envFile(); got != want {
		t.Errorf("env file:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(e.out.String(), "read 7 key(s) from kv/ci/goreleaser into $RUNNER_TEMP\n") {
		t.Errorf("%s", e.out.String())
	}
	// no `value` output for several keys
	if strings.Contains(e.outputs(), "value<<") {
		t.Errorf("outputs %q", e.outputs())
	}
	// a multi-line secret is masked line by line, and a line too short to
	// mask ("xy") is not
	for _, l := range []string{"-----BEGIN KEY-----", "abcdefghij", "-----END KEY-----"} {
		if !strings.Contains(e.out.String(), "::add-mask::"+l+"\n") {
			t.Errorf("line %q is not masked", l)
		}
	}
	if strings.Contains(e.out.String(), "::add-mask::xy\n") {
		t.Error("a short line is not masked")
	}
	e.noLeak("deploy-bot", `it's a "p@ss" $(x)`, multi)
}

func TestKeysInput(t *testing.T) {
	v := newVault(t, false, `{"a-key":"value-a","b-key":"value-b","c-key":"value-c"}`)
	for _, tc := range []struct {
		wanted, want string
		count        int
	}{
		{"c-key a-key", "C_KEY='value-c'\nA_KEY='value-a'\n", 2},
		{"b-key,c-key", "B_KEY='value-b'\nC_KEY='value-c'\n", 2},
		{" , a-key ,, ", "A_KEY='value-a'\n", 1},
		{"a-key a-key", "A_KEY='value-a'\nA_KEY='value-a'\n", 2},           // as the shell version: a repeat is a repeat
		{",  ,", "A_KEY='value-a'\nB_KEY='value-b'\nC_KEY='value-c'\n", 3}, // nothing named takes everything
	} {
		e := newEnv(t, v)
		e.o.Wanted = tc.wanted
		if err := e.run(); err != nil {
			t.Fatal(err)
		}
		if got := e.envFile(); got != tc.want {
			t.Errorf("keys %q: %q, want %q", tc.wanted, got, tc.want)
		}
		if !strings.Contains(e.out.String(), fmt.Sprintf("read %d key(s)", tc.count)) {
			t.Errorf("keys %q: %s", tc.wanted, e.out.String())
		}
	}
	e := newEnv(t, v)
	e.o.Wanted = "a-key nope"
	if err := e.run(); !errors.Is(err, ErrReported) || !strings.HasSuffix(e.out.String(), "::error::kv/ci/goreleaser has no key nope\n") {
		t.Errorf("%v / %s", err, e.out.String())
	}
	e.noLeak("value-a")
}

func TestEnvNames(t *testing.T) {
	for key, ok := range map[string]bool{"a-b_c9": true, "_x": true, "9a": false, "a.b": false, "a b": false, "é": false} {
		v := newVault(t, false, fmt.Sprintf(`{%q:"secret-value-%s"}`, key, "x"))
		e := newEnv(t, v)
		err := e.run()
		if ok != (err == nil) {
			t.Errorf("key %q: err = %v", key, err)
		}
		if !ok && !strings.HasSuffix(e.out.String(), "::error::key "+key+" is no environment variable name\n") {
			t.Errorf("key %q: %s", key, e.out.String())
		}
		e.noLeak("secret-value-x")
	}
}

func TestRefusals(t *testing.T) {
	const refused = `{"errors":["permission denied"]}`
	t.Run("login refused", func(t *testing.T) {
		v := newVault(t, false, `{"k":"secret-value-1"}`)
		v.loginCode, v.loginBody = 403, refused
		e := newEnv(t, v)
		err := e.run()
		if !errors.Is(err, ErrReported) || !strings.HasSuffix(e.out.String(), "::error::login at jwt-roster failed: "+refused+"\n") {
			t.Errorf("%v / %s", err, e.out.String())
		}
		if v.revokeSeen {
			t.Error("there is no login to revoke")
		}
		e.noLeak(jwt)
	})
	t.Run("login without a token", func(t *testing.T) {
		v := newVault(t, false, `{"k":"secret-value-1"}`)
		v.loginCode, v.loginBody = 200, `{"auth":null}`
		e := newEnv(t, v)
		if err := e.run(); !errors.Is(err, ErrReported) || !strings.HasSuffix(e.out.String(), "::error::jwt-roster/login returned no token\n") {
			t.Errorf("%v / %s", err, e.out.String())
		}
	})
	t.Run("read refused, login revoked", func(t *testing.T) {
		v := newVault(t, false, `{"k":"secret-value-1"}`)
		v.readCode, v.readBody = 403, refused
		e := newEnv(t, v)
		if err := e.run(); !errors.Is(err, ErrReported) || !strings.HasSuffix(e.out.String(), "::error::reading kv/ci/goreleaser failed: "+refused+"\n") {
			t.Errorf("%v / %s", err, e.out.String())
		}
		if !v.revokeSeen {
			t.Error("a login is revoked even when the read fails")
		}
		e.noLeak(jwt, clientToken)
	})
	t.Run("an empty path", func(t *testing.T) {
		v := newVault(t, false, `{}`)
		e := newEnv(t, v)
		if err := e.run(); !errors.Is(err, ErrReported) || !strings.HasSuffix(e.out.String(), "::error::kv/ci/goreleaser holds no keys\n") || !v.revokeSeen {
			t.Errorf("%v / %s", err, e.out.String())
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		v := newVault(t, false, `{}`)
		e := newEnv(t, v)
		e.o.Address = "http://127.0.0.1:1"
		if err := e.run(); !errors.Is(err, ErrReported) || !strings.HasSuffix(e.out.String(), "::error::login at jwt-roster failed: \n") {
			t.Errorf("%v / %s", err, e.out.String())
		}
		if strings.Contains(e.err.String(), jwt) || !strings.Contains(e.err.String(), "127.0.0.1:1") {
			t.Errorf("stderr: %q", e.err.String())
		}
	})
}

func TestInputsAndSluisctl(t *testing.T) {
	v := newVault(t, false, `{"k":"secret-value-1"}`)
	for _, tc := range []struct {
		mut  func(o *Options)
		want string
	}{
		{func(o *Options) { o.Issuer = "" }, "::error::ISSUER is required\n"},
		{func(o *Options) { o.Address = "" }, "::error::ADDRESS is required\n"},
		{func(o *Options) { o.KVPath = "" }, "::error::KV_PATH is required\n"},
		{func(o *Options) { o.IDTokenRequestURL = "" }, "::error::this job has no id-token permission; the read is an exchange of the job's own identity token\n"},
		{func(o *Options) {
			o.Token = func(context.Context, []string) (string, error) { return "", errors.New("exit 1") }
		}, "exchanging this job's identity for the openbao audience\n::error::sluisctl could not exchange this job's token for openbao -- the issuer's grants decide, so check the job's matchers\n"},
		{func(o *Options) {
			o.Token = func(context.Context, []string) (string, error) { return "chatter\n\n", nil }
		},
			"exchanging this job's identity for the openbao audience\n::error::sluisctl returned no token for openbao\n"},
		{func(o *Options) { o.CACert = "not a certificate" }, "::error::ca-cert holds no PEM certificate\n"},
	} {
		e := newEnv(t, v)
		tc.mut(&e.o)
		if err := e.run(); !errors.Is(err, ErrReported) || e.out.String() != tc.want {
			t.Errorf("%v / %q, want %q", err, e.out.String(), tc.want)
		}
	}
	// the exchange is asked for the right audience, from the right issuer, and
	// a CR in the last line is dropped
	var got []string
	e := newEnv(t, v)
	e.o.Audience = "other"
	e.o.Token = func(_ context.Context, a []string) (string, error) { got = a; return "Info\n" + jwt + "\r\n", nil }
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "token --issuer https://issuer.example --audience other" {
		t.Errorf("sluisctl args %v", got)
	}
	var login map[string]string
	_ = json.Unmarshal([]byte(v.reqs[len(v.reqs)-3].body), &login)
	if login["jwt"] != jwt {
		t.Errorf("jwt %q", login["jwt"])
	}
}

func TestSluisctlCommand(t *testing.T) {
	// One directory per PATH a job might have: sluisctl only, both names,
	// the deprecated name only, neither. devbox's stub answers `printenv
	// PATH` with $DEVBOX_PATH, so the devbox environment is a PATH of its
	// own, as it is for real.
	mk := func(names ...string) string {
		dir := t.TempDir()
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\necho "+name+" \"$@\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	newOnly, both, oldOnly, neither := mk("sluisctl"), mk("sluisctl", "accessctl"), mk("accessctl"), mk()
	tools := t.TempDir()
	devbox := "#!/bin/sh\n" +
		"if [ \"$3\" = printenv ]; then echo 'Info: devbox chatter'; echo \"$DEVBOX_PATH\"; exit 0; fi\n" +
		"echo devbox \"$@\"\n"
	if err := os.WriteFile(filepath.Join(tools, "devbox"), []byte(devbox), 0o755); err != nil {
		t.Fatal(err)
	}
	withDevbox, without := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(withDevbox, "devbox.json"), []byte("{}"), 0o644)
	const warning = "::warning::no sluisctl on PATH, running accessctl, its deprecated name; add sluisctl to this repository's devbox\n"
	for _, tc := range []struct {
		name, dir, mode, jobPath, devboxPath, want, warn string
	}{
		{"devbox has sluisctl", withDevbox, "auto", neither, newOnly, "devbox run -- sluisctl token --audience x", ""},
		{"devbox has both", withDevbox, "auto", neither, both, "devbox run -- sluisctl token --audience x", ""},
		{"devbox has accessctl only", withDevbox, "auto", neither, oldOnly, "devbox run -- accessctl token --audience x", warning},
		{"devbox has neither", withDevbox, "auto", neither, neither, "devbox run -- sluisctl token --audience x", ""},
		{"image bakes sluisctl", withDevbox, "auto", newOnly, "", "devbox run -- sluisctl token --audience x", ""},
		{"direct, sluisctl", withDevbox, "direct", newOnly, oldOnly, "sluisctl token --audience x", ""},
		{"direct, accessctl only", withDevbox, "direct", oldOnly, newOnly, "accessctl token --audience x", warning},
		{"no devbox.json", without, "auto", both, oldOnly, "sluisctl token --audience x", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tools+string(os.PathListSeparator)+tc.jobPath+string(os.PathListSeparator)+"/usr/bin:/bin")
			t.Setenv("DEVBOX_PATH", tc.devboxPath)
			var out bytes.Buffer
			o := Options{Dir: tc.dir, SluisctlBy: tc.mode, Out: &out, Err: io.Discard}
			got, err := o.sluisctl(context.Background(), []string{"token", "--audience", "x"})
			if err != nil || strings.TrimSpace(got) != tc.want {
				t.Errorf("got %q %v, want %q", got, err, tc.want)
			}
			if out.String() != tc.warn {
				t.Errorf("warning %q, want %q", out.String(), tc.warn)
			}
		})
	}
}

func TestCACert(t *testing.T) {
	v := newVault(t, true, `{"k":"secret-value-1"}`)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: v.srv.Certificate().Raw}))

	e := newEnv(t, v)
	e.o.CACert = ca
	if err := e.run(); err != nil {
		t.Fatal(err, e.out.String(), e.err.String())
	}
	if got := e.envFile(); got != "K='secret-value-1'\n" {
		t.Errorf("%q", got)
	}

	// without it the runner's trust store does not cover the endpoint
	e = newEnv(t, v)
	if err := e.run(); !errors.Is(err, ErrReported) || !strings.Contains(e.err.String(), "certificate") {
		t.Errorf("%v / %s", err, e.err.String())
	}
}

func TestDefaultsAndPathNaming(t *testing.T) {
	var o Options
	o.defaults()
	if o.Mount != "kv" || o.AuthMount != "jwt-roster" || o.Role != "roster" || o.Audience != "openbao" || o.SluisctlBy != "auto" {
		t.Errorf("%+v", o)
	}
	if got := nonAlnumToDash("ci/gore-leaser é.x"); got != "ci-gore-leaser----x" {
		t.Errorf("%q", got)
	}
	if lastLine("a\nb\n") != "b" || lastLine("a\nb") != "b" || lastLine("") != "" || lastLine("a\n\n") != "" {
		t.Error("lastLine is tail -n1")
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
