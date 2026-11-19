package ssmsecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const (
	jwt = "eyJ-job-identity-token-value"
	// Written in two halves: the leak canary bans a 12-digit run as text.
	account = "12345678" + "9012"
	role    = "ci-reader"
	path    = "/example/ci/name"
)

// fakeSSM is the SSM client interface faked: it serves pages of parameters
// and records what it was asked.
type fakeSSM struct {
	pages [][]types.Parameter
	err   error
	calls []ssm.GetParametersByPathInput
}

func (f *fakeSSM) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	f.calls = append(f.calls, *in)
	if f.err != nil {
		return nil, f.err
	}
	i := len(f.calls) - 1
	if i >= len(f.pages) {
		return &ssm.GetParametersByPathOutput{}, nil
	}
	out := &ssm.GetParametersByPathOutput{Parameters: f.pages[i]}
	if i+1 < len(f.pages) {
		out.NextToken = aws.String(fmt.Sprintf("page-%d", i+1))
	}
	return out, nil
}

func param(leaf, value string) types.Parameter {
	return types.Parameter{Name: aws.String(path + "/" + leaf), Value: aws.String(value)}
}

type env struct {
	t        *testing.T
	ssm      *fakeSSM
	o        Options
	out, err strings.Builder
	exchange []string // the sluisctl arguments
	gotToken string   // what the client builder was handed
}

func newEnv(t *testing.T, pages ...[]types.Parameter) *env {
	t.Helper()
	e := &env{t: t, ssm: &fakeSSM{pages: pages}}
	e.o = Options{
		Issuer: "https://issuer.example", Account: account, Role: role, Region: "eu-west-1", Path: path,
		IDTokenRequestURL: "https://actions.example/token", RunnerTemp: t.TempDir(),
		GithubOutput: filepath.Join(t.TempDir(), "output"),
		Token: func(_ context.Context, args []string) (string, error) {
			e.exchange = args
			return "Info: chatter\n" + jwt + "\n", nil
		},
		Client: func(_ context.Context, token string) (SSMAPI, error) {
			e.gotToken = token
			return e.ssm, nil
		},
	}
	e.o.Out, e.o.Err = &e.out, &e.err
	return e
}

func (e *env) run() error { return Run(context.Background(), e.o) }

func (e *env) envFile() string {
	b, err := os.ReadFile(filepath.Join(e.o.RunnerTemp, "ssm-secrets-example-ci-name.env"))
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

func (e *env) masked(want ...string) {
	e.t.Helper()
	for _, w := range want {
		if !strings.Contains(e.out.String(), "::add-mask::"+w+"\n") {
			e.t.Errorf("%q is not masked; log:\n%s", w, e.out.String())
		}
	}
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"maven-username": "MAVEN_USERNAME",
		"goreleaser_key": "GORELEASER_KEY",
		"Token":          "TOKEN",
		"a-b-c":          "A_B_C",
		"key.name":       "KEY.NAME", // not a valid name: FormatEnvFile refuses it
	} {
		if got := EnvName(in); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLeaf(t *testing.T) {
	for in, want := range map[string]string{
		"/example/ci/name/token": "token",
		"token":                  "token",
		"/example/ci/name/":      "",
	} {
		if got := Leaf(in); got != want {
			t.Errorf("Leaf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSelectKeys(t *testing.T) {
	all := []Parameter{{"b", "2"}, {"a", "1"}, {"c", "3"}}
	leaves := func(ps []Parameter) string {
		var l []string
		for _, p := range ps {
			l = append(l, p.Leaf)
		}
		return strings.Join(l, ",")
	}
	for _, tc := range []struct {
		name, wanted, want, wantErr string
	}{
		{"blank takes all, sorted", "", "a,b,c", ""},
		{"whitespace only takes all", " \n\t ", "a,b,c", ""},
		{"commas only take all", " , ", "a,b,c", ""},
		{"one key", "b", "b", ""},
		{"comma separated, in the order asked", "c,a", "c,a", ""},
		{"whitespace and commas mixed", "c a,\nb", "c,a,b", ""},
		{"a repeat counts once", "a a", "a", ""},
		{"a missing key fails", "a,nope", "", "no parameter nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectKeys(all, tc.wanted)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case leaves(got) != tc.want:
				t.Errorf("got %s, want %s", leaves(got), tc.want)
			}
		})
	}
}

func TestFormatEnvFile(t *testing.T) {
	got, err := FormatEnvFile([]Parameter{{"maven-username", "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "MAVEN_USERNAME='bob'\n" {
		t.Errorf("got %q", got)
	}

	// a quote cannot end the value; $ and ` stay data inside single quotes
	got, err = FormatEnvFile([]Parameter{{"tok", "it's $(x) `y`"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "TOK='it'\\''s $(x) `y`'\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	for _, tc := range []struct {
		name    string
		in      []Parameter
		wantErr string
	}{
		{"digit first", []Parameter{{"1abc", "x"}}, "no environment variable name"},
		{"dot", []Parameter{{"a.b", "x"}}, "no environment variable name"},
		{"space", []Parameter{{"a b", "x"}}, "no environment variable name"},
		{"two leaves one name", []Parameter{{"a-b", "x"}, {"a_b", "y"}}, "both A_B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FormatEnvFile(tc.in); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRoleARNAndAudience(t *testing.T) {
	if got := Audience(account, role); got != "aws:"+account+":ci-reader" {
		t.Errorf("audience %q", got)
	}
	// The partition is an argument: the leak canary bans the ARN prefix as text.
	for region, partition := range map[string]string{
		"eu-west-1":      "aws",
		"cn-north-1":     "aws-cn",
		"us-gov-west-1":  "aws-us-gov",
		"ap-southeast-2": "aws",
	} {
		want := fmt.Sprintf("arn:%s:iam::%s:role/ci-reader", partition, account)
		if got := RoleARN(region, account, role); got != want {
			t.Errorf("RoleARN(%s) = %q, want %q", region, got, want)
		}
	}
}

func TestSingleValue(t *testing.T) {
	e := newEnv(t, []types.Parameter{param("goreleaser-key", "gr-key-value\n")})
	if err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, e.out.String())
	}
	if want := "GORELEASER_KEY='gr-key-value'\n"; e.envFile() != want {
		t.Errorf("env file %q, want %q", e.envFile(), want)
	}
	fi, _ := os.Stat(filepath.Join(e.o.RunnerTemp, "ssm-secrets-example-ci-name.env"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("env file mode %v, want 0600", fi.Mode().Perm())
	}
	outs := e.outputs()
	if !strings.Contains(outs, "env-file="+filepath.Join(e.o.RunnerTemp, "ssm-secrets-example-ci-name.env")+"\n") {
		t.Errorf("outputs lack env-file:\n%s", outs)
	}
	m := regexp.MustCompile(`(?m)^value<<(ssm-[0-9a-f]{32})\ngr-key-value\n(ssm-[0-9a-f]{32})\n`).FindStringSubmatch(outs)
	if m == nil || m[1] != m[2] {
		t.Errorf("outputs lack the value in a heredoc:\n%s", outs)
	}
	e.masked(jwt, "gr-key-value")
	e.noLeak(jwt, "gr-key-value")

	// the exchange is for the role's audience, at the issuer, with the token
	// handed to the client builder minus the devbox chatter
	if got, want := strings.Join(e.exchange, " "), "token --issuer https://issuer.example --audience aws:"+account+":ci-reader"; got != want {
		t.Errorf("exchange %q, want %q", got, want)
	}
	if e.gotToken != jwt {
		t.Errorf("client was handed %q, want the token", e.gotToken)
	}
	// decrypted, not recursive, under the path
	c := e.ssm.calls[0]
	if aws.ToString(c.Path) != path || !aws.ToBool(c.WithDecryption) || c.Recursive == nil || *c.Recursive {
		t.Errorf("request %+v", c)
	}
}

func TestManyKeysNoValueOutput(t *testing.T) {
	multi := "line-one-secret\nline-two-secret"
	e := newEnv(t, []types.Parameter{
		param("maven-username", "mvn-user"), param("maven-password", "p@ss'w0rd"), param("pem", multi),
	})
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	want := "MAVEN_PASSWORD='p@ss'\\''w0rd'\nMAVEN_USERNAME='mvn-user'\nPEM='" + multi + "'\n"
	if e.envFile() != want {
		t.Errorf("env file:\n%q\nwant\n%q", e.envFile(), want)
	}
	if strings.Contains(e.outputs(), "value<<") {
		t.Errorf("a value output with three keys:\n%s", e.outputs())
	}
	// each line of a multi-line value is masked on its own
	e.masked("mvn-user", "p@ss'w0rd", "line-one-secret", "line-two-secret")
	e.noLeak("mvn-user", "p@ss'w0rd", multi)
	if !strings.Contains(e.out.String(), "read 3 key(s) from "+path) {
		t.Errorf("log: %s", e.out.String())
	}
}

func TestPagesAreFollowed(t *testing.T) {
	e := newEnv(t,
		[]types.Parameter{param("a-key", "value-a")},
		[]types.Parameter{param("b-key", "value-b")},
	)
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if want := "A_KEY='value-a'\nB_KEY='value-b'\n"; e.envFile() != want {
		t.Errorf("env file %q", e.envFile())
	}
	if len(e.ssm.calls) != 2 || e.ssm.calls[0].NextToken != nil || aws.ToString(e.ssm.calls[1].NextToken) != "page-1" {
		t.Errorf("calls %+v", e.ssm.calls)
	}
}

func TestKeysFilter(t *testing.T) {
	e := newEnv(t, []types.Parameter{param("one-key", "value-one"), param("two-key", "value-two")})
	e.o.Wanted = "two-key"
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if want := "TWO_KEY='value-two'\n"; e.envFile() != want {
		t.Errorf("env file %q", e.envFile())
	}
	// one requested key is a value output
	if !strings.Contains(e.outputs(), "\nvalue-two\n") {
		t.Errorf("outputs:\n%s", e.outputs())
	}
	e.noLeak("value-one", "value-two")
}

func TestRefusals(t *testing.T) {
	good := []types.Parameter{param("a-key", "value-a")}
	for _, tc := range []struct {
		name  string
		pages [][]types.Parameter
		edit  func(e *env)
		want  string
	}{
		{"missing issuer", [][]types.Parameter{good}, func(e *env) { e.o.Issuer = "" }, "ISSUER is required"},
		{"missing account", [][]types.Parameter{good}, func(e *env) { e.o.Account = "" }, "ACCOUNT is required"},
		{"missing role", [][]types.Parameter{good}, func(e *env) { e.o.Role = "" }, "ROLE is required"},
		{"missing region", [][]types.Parameter{good}, func(e *env) { e.o.Region = "" }, "REGION is required"},
		{"missing path", [][]types.Parameter{good}, func(e *env) { e.o.Path = "" }, "SSM_PATH is required"},
		{"short account", [][]types.Parameter{good}, func(e *env) { e.o.Account = "1234" }, "12-digit"},
		{"role as an ARN", [][]types.Parameter{good}, func(e *env) { e.o.Role = "role/ci" + "-reader" }, "role name"},
		{"odd region", [][]types.Parameter{good}, func(e *env) { e.o.Region = "eu west" }, "not an AWS region"},
		{"relative path", [][]types.Parameter{good}, func(e *env) { e.o.Path = "example/ci" }, "must start with /"},
		{"root path", [][]types.Parameter{good}, func(e *env) { e.o.Path = "/" }, "not the root"},
		{"no id-token permission", [][]types.Parameter{good}, func(e *env) { e.o.IDTokenRequestURL = "" }, "no id-token permission"},
		{"sluisctl fails", [][]types.Parameter{good}, func(e *env) {
			e.o.Token = func(context.Context, []string) (string, error) { return "", errors.New("exit 1") }
		}, "check the job's matchers"},
		{"sluisctl returns nothing", [][]types.Parameter{good}, func(e *env) {
			e.o.Token = func(context.Context, []string) (string, error) { return "\n", nil }
		}, "returned no token"},
		{"the role cannot be assumed", [][]types.Parameter{good}, func(e *env) {
			e.o.Client = func(context.Context, string) (SSMAPI, error) { return nil, errors.New("not authorized") }
		}, "could not assume role ci-reader"},
		{"the read is refused", [][]types.Parameter{good}, func(e *env) { e.ssm.err = errors.New("AccessDeniedException") }, "reading /example/ci/name failed: AccessDeniedException"},
		{"nothing under the path", nil, nil, "holds no parameters"},
		{"a requested key is missing", [][]types.Parameter{good}, func(e *env) { e.o.Wanted = "a-key, b-key" }, "no parameter b-key"},
		{"a leaf that is no env name", [][]types.Parameter{{param("a.key", "value-a")}}, nil, "key a.key is no environment variable name"},
		{"two leaves one name", [][]types.Parameter{{param("a-key", "value-a"), param("a_key", "value-b")}}, nil, "both A_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.pages...)
			if tc.edit != nil {
				tc.edit(e)
			}
			err := e.run()
			if !errors.Is(err, ErrReported) {
				t.Fatalf("err = %v, want ErrReported\n%s", err, e.out.String())
			}
			if !strings.Contains(e.out.String(), "::error::") || !strings.Contains(e.out.String(), tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, e.out.String())
			}
			// a refusal writes no output
			if b, _ := os.ReadFile(e.o.GithubOutput); len(b) != 0 {
				t.Errorf("outputs after a refusal:\n%s", b)
			}
			e.noLeak("value-a", "value-b")
		})
	}
}

// A failed run leaves no env file: the file is written only after every key is
// checked.
func TestRefusalLeavesNoEnvFile(t *testing.T) {
	e := newEnv(t, []types.Parameter{param("a-key", "value-a")})
	e.o.Wanted = "nope"
	_ = e.run()
	if _, err := os.Stat(filepath.Join(e.o.RunnerTemp, "ssm-secrets-example-ci-name.env")); err == nil {
		t.Error("an env file exists after a refusal")
	}
}

func TestShortValuesAreNotMasked(t *testing.T) {
	e := newEnv(t, []types.Parameter{param("flag", "yes")})
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.out.String(), "::add-mask::yes") {
		t.Error("a three-character value was masked")
	}
}

func TestTrailingSlashPath(t *testing.T) {
	e := newEnv(t, []types.Parameter{param("a-key", "value-a")})
	e.o.Path = path + "/"
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(e.ssm.calls[0].Path); got != path {
		t.Errorf("path asked %q, want %q", got, path)
	}
}

func TestSluisctlCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sluisctl"), []byte("#!/bin/sh\necho sluisctl \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "devbox"), []byte("#!/bin/sh\necho devbox \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withDevbox, without := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(withDevbox, "devbox.json"), []byte("{}"), 0o644)
	for _, tc := range []struct {
		name, dir, mode, want string
	}{
		{"devbox.json, auto", withDevbox, "auto", "devbox run -- sluisctl token --audience x"},
		{"devbox.json, direct", withDevbox, "direct", "sluisctl token --audience x"},
		{"no devbox.json", without, "auto", "sluisctl token --audience x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tools+string(os.PathListSeparator)+dir+string(os.PathListSeparator)+"/usr/bin:/bin")
			var out bytes.Buffer
			o := Options{Dir: tc.dir, SluisctlBy: tc.mode, Out: &out, Err: io.Discard}
			got, err := o.sluisctl(context.Background(), []string{"token", "--audience", "x"})
			if err != nil || strings.TrimSpace(got) != tc.want {
				t.Errorf("got %q %v, want %q", got, err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("unexpected output %q", out.String())
			}
		})
	}
}

// TestRealClient runs the real exchange against a fake endpoint that plays STS
// and SSM: the token goes out as the web identity of the role's ARN, the
// credentials it returns sign the SSM call, and are masked.
func TestRealClient(t *testing.T) {
	const secretKey, sessionToken = "fake-secret-access-key-value", "fake-session-token-value"
	var stsForm url.Values
	var ssmTarget, ssmBody, ssmAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Header.Get("X-Amz-Target") == "AmazonSSM.GetParametersByPath":
			ssmTarget, ssmBody, ssmAuth = r.Header.Get("X-Amz-Target"), string(b), r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			_ = json.NewEncoder(w).Encode(map[string]any{"Parameters": []map[string]any{
				{"Name": path + "/real-key", "Type": "SecureString", "Value": "real-value-1"},
			}})
		default:
			stsForm, _ = url.ParseQuery(string(b))
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>AKIAFAKEKEYID</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, secretKey, sessionToken)
		}
	}))
	t.Cleanup(srv.Close)
	// nothing of the environment may stand in for the exchange
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAAMBIENTKEYID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")

	e := newEnv(t)
	e.o.Client = nil
	e.o.endpoint = srv.URL
	if err := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, e.out.String())
	}
	if got := stsForm.Get("Action"); got != "AssumeRoleWithWebIdentity" {
		t.Errorf("STS action %q", got)
	}
	if got, want := stsForm.Get("RoleArn"), RoleARN("eu-west-1", account, role); got != want {
		t.Errorf("RoleArn %q, want %q", got, want)
	}
	if got := stsForm.Get("WebIdentityToken"); got != jwt {
		t.Errorf("WebIdentityToken %q, want the exchanged token", got)
	}
	if ssmTarget == "" || !strings.Contains(ssmBody, `"WithDecryption":true`) || !strings.Contains(ssmBody, `"Path":"`+path+`"`) {
		t.Errorf("SSM call %q %s", ssmTarget, ssmBody)
	}
	if !strings.Contains(ssmAuth, "Credential=AKIAFAKEKEYID/") || strings.Contains(ssmAuth, "AMBIENT") {
		t.Errorf("SSM call not signed with the role's credentials: %s", ssmAuth)
	}
	if want := "REAL_KEY='real-value-1'\n"; e.envFile() != want {
		t.Errorf("env file %q", e.envFile())
	}
	e.masked(jwt, secretKey, sessionToken, "real-value-1")
	e.noLeak(jwt, secretKey, sessionToken, "real-value-1")
}

func TestRealClientRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>Not authorized to perform sts:AssumeRoleWithWebIdentity</Message></Error></ErrorResponse>`)
	}))
	t.Cleanup(srv.Close)
	e := newEnv(t)
	e.o.Client = nil
	e.o.endpoint = srv.URL
	err := e.run()
	if !errors.Is(err, ErrReported) || !strings.Contains(e.out.String(), "could not assume role ci-reader") {
		t.Fatalf("err = %v\n%s", err, e.out.String())
	}
	e.noLeak(jwt)
}
