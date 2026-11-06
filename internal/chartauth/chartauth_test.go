package chartauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	password = "ecr-password-AAAA/bbbb+cccc=="
	profile  = "reader@account-1"
)

type call struct {
	name string
	args []string
	env  []string
}

type harness struct {
	t     *testing.T
	dir   string
	opts  Options
	calls []call
	out   bytes.Buffer
}

// newHarness is a runner that has run token-exchange: an AWS config with the
// profile, no static keys in the environment, and an `aws` that answers.
func newHarness(t *testing.T) *harness {
	t.Helper()
	d := t.TempDir()
	h := &harness{t: t, dir: d}
	cfg := filepath.Join(d, "aws-config")
	body := "[profile " + profile + "]\ncredential_process = /bin/true\nregion = eu-west-1\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"github-env", "github-output"} {
		if err := os.WriteFile(filepath.Join(d, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.opts = Options{
		Profile: profile, Registry: "registry.example", Region: "eu-west-1",
		Environ: []string{"PATH=/usr/bin", "HOME=/home/runner"}, ConfigFile: cfg,
		CredentialsFile: filepath.Join(d, "no-credentials"), // hermetic: never the machine's own
		RunnerTemp:      filepath.Join(d, "runner-temp"), GithubEnv: filepath.Join(d, "github-env"), GithubOutput: filepath.Join(d, "github-output"),
		InCI: true,
		Out:  &h.out,
		Exec: func(_ context.Context, name string, args, env []string) ([]byte, error) {
			h.calls = append(h.calls, call{name, args, env})
			return []byte(password + "\n"), nil
		},
	}
	if err := os.MkdirAll(h.opts.RunnerTemp, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) run() error { return Run(context.Background(), h.opts) }

func (h *harness) refused(err error, rule string) {
	h.t.Helper()
	if !errors.Is(err, ErrFailed) {
		h.t.Fatalf("want ErrFailed, got %v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), "::error::chart-auth: "+rule+":") {
		h.t.Fatalf("want rule %q in:\n%s", rule, h.out.String())
	}
	h.noSecretOutside(password)
}

// noSecretOutside: a value appears in the log only inside an ::add-mask::.
func (h *harness) noSecretOutside(secret string) {
	h.t.Helper()
	for _, line := range strings.Split(h.out.String(), "\n") {
		if strings.Contains(line, secret) && line != "::add-mask::"+secret {
			h.t.Fatalf("%q is printed outside an ::add-mask:: line: %q", secret, line)
		}
	}
}

func (h *harness) noFiles() {
	h.t.Helper()
	if _, err := os.Stat(filepath.Join(h.opts.RunnerTemp, "chart-auth")); err == nil {
		h.t.Fatalf("a refusal must leave no credential behind")
	}
	if len(h.calls) != 0 {
		h.t.Fatalf("aws must not be called, got %v", h.calls)
	}
}

func TestALongLivedKeyIsRefusedBeforeAnyLogin(t *testing.T) {
	for _, kv := range []string{"AWS_ACCESS_KEY_ID=AKIAEXAMPLEEXAMPLE", "AWS_SECRET_ACCESS_KEY=secretsecretsecret", "AWS_SESSION_TOKEN=sessiontokenvalue"} {
		t.Run(strings.SplitN(kv, "=", 2)[0], func(t *testing.T) {
			h := newHarness(t)
			h.opts.Environ = append(h.opts.Environ, kv)
			h.refused(h.run(), "static-credentials")
			h.noFiles()
			h.noSecretOutside(strings.SplitN(kv, "=", 2)[1])
		})
	}
}

func TestALongLivedKeyIsRefusedOutsideCIToo(t *testing.T) {
	h := newHarness(t)
	h.opts.InCI = false
	h.opts.Environ = append(h.opts.Environ, "AWS_ACCESS_KEY_ID=AKIAEXAMPLEEXAMPLE")
	h.refused(h.run(), "static-credentials")
}

func TestAnEmptyKeyVariableIsNotAKey(t *testing.T) {
	h := newHarness(t)
	h.opts.Environ = append(h.opts.Environ, "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=")
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
}

func TestAProfileThatCarriesAStaticKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	body := "[profile " + profile + "]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\naws_secret_access_key = secretsecretsecret\n"
	if err := os.WriteFile(h.opts.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.refused(h.run(), "static-credentials")
	h.noFiles()
	h.noSecretOutside("secretsecretsecret")
}

func TestAMissingProfileFails(t *testing.T) {
	cases := map[string]func(*harness){
		"no profile and no exchange output": func(h *harness) { h.opts.Profile, h.opts.Profiles = "", "" },
		"several profiles, none chosen":     func(h *harness) { h.opts.Profile, h.opts.Profiles = "", "a@1 b@2" },
		"profile absent from the config":    func(h *harness) { h.opts.Profile = "someone-else@account-2" },
		"no config file":                    func(h *harness) { h.opts.ConfigFile = filepath.Join(h.dir, "nope") },
		"profile that looks like a flag":    func(h *harness) { h.opts.Profile = "--endpoint-url=http://x" },
		"profile with whitespace":           func(h *harness) { h.opts.Profile = "reader@account-1 --debug" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			mod(h)
			h.refused(h.run(), "profile-missing")
			h.noFiles()
		})
	}
}

func TestTheOnlyProfileThatTheExchangeWroteIsUsed(t *testing.T) {
	h := newHarness(t)
	h.opts.Profile, h.opts.Profiles = "", profile
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if len(h.calls) != 1 || !contains(h.calls[0].args, "--profile") || !contains(h.calls[0].args, profile) {
		t.Fatalf("calls: %+v", h.calls)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestALoginThatFailsFailsInCI(t *testing.T) {
	h := newHarness(t)
	h.opts.Exec = func(context.Context, string, []string, []string) ([]byte, error) {
		return []byte("SECRETFRAGMENT-from-aws-stderr"), errors.New("exit status 255: SECRETFRAGMENT-from-aws-stderr")
	}
	h.refused(h.run(), "login-failed")
	h.noSecretOutside("SECRETFRAGMENT-from-aws-stderr")
	if _, err := os.Stat(filepath.Join(h.opts.RunnerTemp, "chart-auth")); err == nil {
		t.Fatalf("a failed login must leave no credential behind")
	}
	if got, _ := os.ReadFile(h.opts.GithubOutput); len(got) != 0 {
		t.Fatalf("a failed login must set no outputs: %q", got)
	}
}

func TestAnEmptyPasswordIsAFailedLogin(t *testing.T) {
	h := newHarness(t)
	h.opts.Exec = func(context.Context, string, []string, []string) ([]byte, error) { return []byte("\n"), nil }
	h.refused(h.run(), "login-failed")
}

func TestALoginThatFailsOutsideCISkipsWithAWarning(t *testing.T) {
	h := newHarness(t)
	h.opts.InCI = false
	h.opts.Exec = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("exit status 255")
	}
	if err := h.run(); err != nil {
		t.Fatalf("outside CI a failed login is skipped, got %v", err)
	}
	if !strings.Contains(h.out.String(), "::warning::chart-auth: login skipped") || strings.Contains(h.out.String(), "::error::") {
		t.Fatalf("want one warning and no error:\n%s", h.out.String())
	}
	if got, _ := os.ReadFile(h.opts.GithubOutput); len(got) != 0 {
		t.Fatalf("a skipped login must set no outputs: %q", got)
	}
}

func TestNoPlaceToKeepTheCredentialFails(t *testing.T) {
	h := newHarness(t)
	h.opts.RunnerTemp = ""
	h.refused(h.run(), "output-location")
	h.noFiles()
}

func TestABadRegistryIsRefused(t *testing.T) {
	for _, reg := range []string{"", "https://registry.example", "registry.example/path", "reg istry", "registry.example\n::error::x", "-x.example"} {
		h := newHarness(t)
		h.opts.Registry = reg
		h.refused(h.run(), "registry-invalid")
		h.noFiles()
	}
}

func TestTheCredentialStaysInRunnerTempAndIsMaskedFirst(t *testing.T) {
	h := newHarness(t)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	out := h.out.String()
	if !strings.HasPrefix(out, "::add-mask::"+password+"\n") {
		t.Fatalf("the password must be masked before anything else is printed:\n%s", out)
	}
	h.noSecretOutside(password)

	dir := filepath.Join(h.opts.RunnerTemp, "chart-auth")
	st, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config.json mode %v, want 0600", st.Mode().Perm())
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v, want 0700", d.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("AWS:" + password)); cfg.Auths["registry.example"].Auth != want {
		t.Errorf("auth entry: %+v", cfg.Auths)
	}
	gh, _ := os.ReadFile(h.opts.GithubOutput)
	if string(gh) != "config-dir="+dir+"\nregistry=registry.example\n" {
		t.Errorf("outputs: %q", gh)
	}
	if strings.Contains(string(gh), password) {
		t.Errorf("the password is an output")
	}
	if env, _ := os.ReadFile(h.opts.GithubEnv); len(env) != 0 {
		t.Errorf("nothing is exported unless asked: %q", env)
	}
}

func TestLoginIsOnePullCredentialForTheProfileAndRegion(t *testing.T) {
	h := newHarness(t)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 {
		t.Fatalf("want one aws call, got %+v", h.calls)
	}
	c := h.calls[0]
	if c.name != "aws" || strings.Join(c.args, " ") != "ecr get-login-password --profile "+profile+" --region eu-west-1" {
		t.Errorf("call: %s %v", c.name, c.args)
	}
	h.opts.Region = ""
	h.calls = nil
	_ = os.RemoveAll(filepath.Join(h.opts.RunnerTemp, "chart-auth"))
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(h.calls[0].args, " "), "--region") {
		t.Errorf("an empty region leaves the profile's own: %v", h.calls[0].args)
	}
}

func TestExportEnvPointsDockerAndHelmAtTheFileNotTheSecret(t *testing.T) {
	h := newHarness(t)
	h.opts.ExportEnv = true
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.opts.RunnerTemp, "chart-auth")
	env, _ := os.ReadFile(h.opts.GithubEnv)
	if string(env) != "DOCKER_CONFIG="+dir+"\nHELM_REGISTRY_CONFIG="+filepath.Join(dir, "config.json")+"\n" {
		t.Errorf("env: %q", env)
	}
}

func TestAMultiLinePasswordIsMaskedLineByLine(t *testing.T) {
	h := newHarness(t)
	h.opts.Exec = func(context.Context, string, []string, []string) ([]byte, error) {
		return []byte("line-one-secret\nline-two-secret\n"), nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	for _, l := range []string{"line-one-secret", "line-two-secret"} {
		if !strings.Contains(h.out.String(), "::add-mask::"+l+"\n") {
			t.Errorf("%s is not masked:\n%s", l, h.out.String())
		}
		h.noSecretOutside(l)
	}
}

func TestTheAWSCommandSeesNoStaticKeysAndNoShellFromAnInput(t *testing.T) {
	h := newHarness(t)
	h.opts.Environ = append(h.opts.Environ, "AWS_CONFIG_FILE="+h.opts.ConfigFile, "GITHUB_TOKEN=ghs_should_not_pass")
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, kv := range h.calls[0].env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "AWS_ACCESS_KEY_ID=") {
			t.Errorf("the aws command got %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	if !contains(h.calls[0].env, "AWS_CONFIG_FILE="+h.opts.ConfigFile) || !contains(h.calls[0].env, "PATH=/usr/bin") {
		t.Errorf("the aws command needs the config file and PATH: %v", h.calls[0].env)
	}
}

func TestABadRegionIsRefused(t *testing.T) {
	for _, region := range []string{"eu west 1", "--debug", "EU-WEST-1", "eu-west-1\n::error::x"} {
		h := newHarness(t)
		h.opts.Region = region
		h.refused(h.run(), "region-invalid")
		h.noFiles()
	}
}

func TestAProfileThatChainsToAStaticKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	body := "[profile " + profile + "]\nsource_profile = base\nrole_arn = x\n\n[profile base]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\n"
	if err := os.WriteFile(h.opts.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.refused(h.run(), "static-credentials")
	h.noFiles()
}

func TestTheRealCommandIsRunWithTheScrubbedEnvironmentAndItsStderrIsNotShown(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "aws")
	script := "#!/bin/sh\necho \"args=$*\" >&2\nif [ -n \"$GITHUB_TOKEN\" ]; then echo leaked >&2; exit 3; fi\nprintf 'pw-from-stub\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.opts.Exec = nil
	h.opts.Environ = append(h.opts.Environ, "PATH="+dir+":/usr/bin:/bin", "GITHUB_TOKEN=ghs_nope")
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), "::add-mask::pw-from-stub\n") || strings.Contains(h.out.String(), "args=") {
		t.Fatalf("output:\n%s", h.out.String())
	}
}

// The CLI also reads the shared credentials file, where a key for the same
// profile name wins over the config file.
func TestAStaticKeyInTheSharedCredentialsFileIsRefused(t *testing.T) {
	cases := map[string]struct{ cfg, creds string }{
		"the profile itself":      {"", "[" + profile + "]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\naws_secret_access_key = secretsecretsecret\n"},
		"a session token alone":   {"", "[" + profile + "]\naws_session_token = sessiontokenvalue\n"},
		"the source profile":      {"[profile " + profile + "]\nsource_profile = base\nrole_arn = x\n\n[profile base]\nregion = eu-west-1\n", "[base]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\n"},
		"a profile only in creds": {"[profile " + profile + "]\nsource_profile = base\nrole_arn = x\n", "[base]\naws_secret_access_key = secretsecretsecret\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if c.cfg != "" {
				if err := os.WriteFile(h.opts.ConfigFile, []byte(c.cfg), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h.opts.CredentialsFile = filepath.Join(h.dir, "credentials")
			if err := os.WriteFile(h.opts.CredentialsFile, []byte(c.creds), 0o600); err != nil {
				t.Fatal(err)
			}
			h.refused(h.run(), "static-credentials")
			h.noFiles()
			h.noSecretOutside("secretsecretsecret")
		})
	}
}

func TestTheSharedCredentialsFileIsFoundByEnvironmentAndByHome(t *testing.T) {
	body := "[" + profile + "]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\n"
	t.Run("AWS_SHARED_CREDENTIALS_FILE", func(t *testing.T) {
		h := newHarness(t)
		p := filepath.Join(h.dir, "elsewhere")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		h.opts.CredentialsFile = ""
		h.opts.Environ = append(h.opts.Environ, "AWS_SHARED_CREDENTIALS_FILE="+p)
		h.refused(h.run(), "static-credentials")
	})
	t.Run("HOME", func(t *testing.T) {
		h := newHarness(t)
		home := filepath.Join(h.dir, "home")
		if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		h.opts.CredentialsFile = ""
		h.opts.Environ = []string{"PATH=/usr/bin", "HOME=" + home}
		h.refused(h.run(), "static-credentials")
	})
}

func TestAKeyForAnotherProfileInTheCredentialsFileIsNotOurs(t *testing.T) {
	h := newHarness(t)
	h.opts.CredentialsFile = filepath.Join(h.dir, "credentials")
	if err := os.WriteFile(h.opts.CredentialsFile, []byte("[unrelated]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
}

// Whatever the refusal saw, the command itself is not allowed to read the
// shared credentials file.
func TestTheAWSCommandDoesNotReadTheSharedCredentialsFile(t *testing.T) {
	h := newHarness(t)
	h.opts.Environ = append(h.opts.Environ, "AWS_SHARED_CREDENTIALS_FILE=/home/runner/.aws/credentials")
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if !contains(h.calls[0].env, "AWS_SHARED_CREDENTIALS_FILE="+os.DevNull) {
		t.Errorf("the aws command must get no credentials file: %v", h.calls[0].env)
	}
	for _, kv := range h.calls[0].env {
		if kv == "AWS_SHARED_CREDENTIALS_FILE=/home/runner/.aws/credentials" {
			t.Errorf("the original credentials file reached the command")
		}
	}
}

// credential_process is how token-exchange profiles produce their
// short-lived credentials, so it is not a reason to refuse.
func TestACredentialProcessProfileIsTheNormalCase(t *testing.T) {
	h := newHarness(t)
	if !strings.Contains(read(t, h.opts.ConfigFile), "credential_process") {
		t.Fatal("the harness profile should use credential_process")
	}
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
