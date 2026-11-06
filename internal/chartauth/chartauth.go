// Package chartauth turns a profile that token-exchange wrote into a
// chart-read registry credential, and keeps it where only this job can read
// it.
//
// The credential is short-lived and comes from the job's own identity: the
// profile assumes a role by web identity, and `aws ecr get-login-password`
// on that profile is the pull password. So this refuses the other way to
// have one: a long-lived AWS access key in the environment (or in the
// profile it would use, or in one that profile chains to) is not a chart-read
// credential, it is a secret that outlives the job, and nothing here will
// run next to it.
//
// A login that fails FAILS when the job runs under GitHub Actions. Outside
// it (a developer's machine without the profile) the login is skipped with a
// warning, as chartgate's -require-ecr does: a gate that skips its
// authentication in CI would read as "no private charts" instead of "could
// not read them".
//
// The password is masked first, then written to a mode-0600 docker-style
// config under RUNNER_TEMP and nowhere else. It is never an output and never
// printed; the command that produces it sees a scrubbed environment and its
// stderr is never shown.
package chartauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrFailed marks a refusal whose ::error:: line has already been written.
var ErrFailed = errors.New("chart-auth: failed")

// The rules. A rule is a fixed token, safe to print.
const (
	RuleStaticCredentials = "static-credentials"
	RuleProfileMissing    = "profile-missing"
	RuleRegistryInvalid   = "registry-invalid"
	RuleRegionInvalid     = "region-invalid"
	RuleOutputLocation    = "output-location"
	RuleLoginFailed       = "login-failed"
)

var sentences = map[string]string{
	RuleStaticCredentials: "a long-lived AWS access key is present; use the short-lived credentials of a token-exchange profile",
	RuleProfileMissing:    "no usable AWS profile: run token-exchange for an aws: audience first, and name its profile when it wrote several",
	RuleRegistryInvalid:   "the registry must be a host name, with an optional port",
	RuleRegionInvalid:     "the region must look like eu-west-1",
	RuleOutputLocation:    "RUNNER_TEMP is not set, so there is nowhere safe to keep the credential",
	RuleLoginFailed:       "the registry login did not produce a password",
}

// Options are the action's inputs and the runner's files.
type Options struct {
	Profile  string // the AWS profile; empty means the one profile in Profiles
	Profiles string // token-exchange's `profiles` output, whitespace separated
	Registry string // the registry host the credential is for
	Region   string // optional; empty leaves the profile's own
	Environ  []string
	// ConfigFile is AWS_CONFIG_FILE; empty means $HOME/.aws/config.
	ConfigFile string
	// CredentialsFile is AWS_SHARED_CREDENTIALS_FILE; empty means
	// $HOME/.aws/credentials. It is read only to refuse a static key in it.
	CredentialsFile string
	RunnerTemp      string
	GithubEnv       string // GITHUB_ENV
	GithubOutput    string // GITHUB_OUTPUT
	InCI            bool   // GITHUB_ACTIONS == "true": a failed login fails
	ExportEnv       bool   // append DOCKER_CONFIG and HELM_REGISTRY_CONFIG to GITHUB_ENV
	// Exec runs a command with exactly the given environment and returns its
	// standard output; nil runs it for real.
	Exec func(ctx context.Context, name string, args, env []string) ([]byte, error)
	Out  io.Writer
}

var (
	profileRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@+=,:-]*$`)
	registryRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	regionRe   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
)

type fail struct{ rule string }

func (f fail) Error() string { return f.rule }

func refuse(rule string) error { return fail{rule} }

// Run obtains and keeps the credential. It returns ErrFailed when it refused
// (the ::error:: line is written) and another error when it could not read
// or write a file at all.
func Run(ctx context.Context, opts Options) error {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	err := login(ctx, opts, out)
	var f fail
	if errors.As(err, &f) {
		fmt.Fprintf(out, "::error::chart-auth: %s: %s\n", f.rule, sentences[f.rule])
		return ErrFailed
	}
	return err
}

func login(ctx context.Context, opts Options, out io.Writer) error {
	for _, kv := range opts.Environ {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN":
			if v != "" {
				return refuse(RuleStaticCredentials)
			}
		}
	}
	if !registryRe.MatchString(opts.Registry) {
		return refuse(RuleRegistryInvalid)
	}
	if opts.Region != "" && !regionRe.MatchString(opts.Region) {
		return refuse(RuleRegionInvalid)
	}

	profile := opts.Profile
	if profile == "" {
		if f := strings.Fields(opts.Profiles); len(f) == 1 {
			profile = f[0]
		}
	}
	if !profileRe.MatchString(profile) {
		return refuse(RuleProfileMissing)
	}
	cfgPath := opts.ConfigFile
	if cfgPath == "" {
		for _, kv := range opts.Environ {
			if v, ok := strings.CutPrefix(kv, "AWS_CONFIG_FILE="); ok && v != "" {
				cfgPath = v
			}
		}
	}
	if cfgPath == "" {
		for _, kv := range opts.Environ {
			if v, ok := strings.CutPrefix(kv, "HOME="); ok && v != "" {
				cfgPath = filepath.Join(v, ".aws", "config")
			}
		}
	}
	profiles, err := readConfig(cfgPath, false)
	if err != nil {
		return refuse(RuleProfileMissing)
	}
	// The shared credentials file is read by the CLI too, and a key there
	// for the same profile name wins over the config file. Its keys are
	// merged in, so a static key anywhere in the chain is refused.
	if credPath := credentialsPath(opts); credPath != "" {
		if creds, err := readConfig(credPath, true); err == nil {
			for name, sec := range creds {
				if profiles[name] == nil {
					profiles[name] = section{}
				}
				for k, v := range sec {
					if _, ok := profiles[name][k]; !ok || staticKey[k] {
						profiles[name][k] = v
					}
				}
			}
		}
	}
	if err := checkProfile(profiles, profile, 0); err != nil {
		return err
	}
	if opts.RunnerTemp == "" {
		return refuse(RuleOutputLocation)
	}

	args := []string{"ecr", "get-login-password", "--profile", profile}
	if opts.Region != "" {
		args = append(args, "--region", opts.Region)
	}
	run := opts.Exec
	if run == nil {
		run = execAWS
	}
	raw, err := run(ctx, "aws", args, scrub(opts.Environ, cfgPath))
	password := strings.TrimRight(string(raw), "\r\n")
	if err != nil || password == "" {
		if opts.InCI {
			return refuse(RuleLoginFailed)
		}
		fmt.Fprintf(out, "::warning::chart-auth: login skipped outside GitHub Actions: %s\n", sentences[RuleLoginFailed])
		return nil
	}
	// Masked before it is written anywhere, line by line.
	for _, line := range strings.Split(password, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			fmt.Fprintf(out, "::add-mask::%s\n", line)
		}
	}

	dir := filepath.Join(opts.RunnerTemp, "chart-auth")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	cfg, err := json.Marshal(map[string]any{"auths": map[string]any{
		opts.Registry: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("AWS:" + password))},
	}})
	if err != nil {
		return err
	}
	file := filepath.Join(dir, "config.json")
	_ = os.Remove(file)
	if err := os.WriteFile(file, cfg, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "chart-auth: %s: a pull credential for %s is in %s\n", profile, opts.Registry, file)

	if opts.GithubOutput != "" {
		if err := appendFile(opts.GithubOutput, "config-dir="+dir+"\nregistry="+opts.Registry+"\n"); err != nil {
			return err
		}
	}
	if opts.ExportEnv && opts.GithubEnv != "" {
		if err := appendFile(opts.GithubEnv, "DOCKER_CONFIG="+dir+"\nHELM_REGISTRY_CONFIG="+file+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func appendFile(p, s string) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// allowed is what the aws command may inherit: where to find things, how to
// reach the network, which region. No token of the job, no key.
var allowed = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true,
	"AWS_REGION": true, "AWS_DEFAULT_REGION": true, "AWS_CA_BUNDLE": true,
	"AWS_STS_REGIONAL_ENDPOINTS": true, "AWS_USE_FIPS_ENDPOINT": true, "AWS_MAX_ATTEMPTS": true, "AWS_RETRY_MODE": true,
	"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "https_proxy": true, "http_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
}

// credentialsPath is where the CLI would look for the shared credentials file.
func credentialsPath(opts Options) string {
	if opts.CredentialsFile != "" {
		return opts.CredentialsFile
	}
	home := ""
	for _, kv := range opts.Environ {
		if v, ok := strings.CutPrefix(kv, "AWS_SHARED_CREDENTIALS_FILE="); ok && v != "" {
			return v
		}
		if v, ok := strings.CutPrefix(kv, "HOME="); ok && v != "" {
			home = v
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".aws", "credentials")
}

func scrub(environ []string, cfgPath string) []string {
	var env []string
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); allowed[k] {
			env = append(env, kv)
		}
	}
	if cfgPath != "" {
		env = append(env, "AWS_CONFIG_FILE="+cfgPath)
	}
	// The shared credentials file is where a long-lived key would live; the
	// command is not allowed to read it, whatever the refusal above saw.
	env = append(env, "AWS_SHARED_CREDENTIALS_FILE="+os.DevNull)
	return env
}

func execAWS(ctx context.Context, name string, args, env []string) ([]byte, error) {
	// The command is found on the PATH it will run with, not on ours.
	bin, pathVal := name, ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVal = v // the last one wins, as in the process environment
		}
	}
	for _, d := range filepath.SplitList(pathVal) {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			bin = p
			break
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &limitWriter{w: &stdout, left: 64 << 10}
	// stderr is dropped: whatever the CLI says about a failure is not for the log.
	err := cmd.Run()
	return stdout.Bytes(), err
}

type limitWriter struct {
	w    io.Writer
	left int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.left {
		return 0, errors.New("output too large")
	}
	l.left -= len(p)
	return l.w.Write(p)
}

// section is one profile of the AWS config: its settings, lower-cased keys.
type section map[string]string

// readConfig reads the profiles of an AWS config file. The file is read as
// INI: `[profile name]` (or `[default]`), `key = value`, `#`/`;` comments.
// In the shared credentials file (credentials true) a profile is `[name]`.
func readConfig(p string, credentials bool) (map[string]section, error) {
	if p == "" {
		return nil, errors.New("no config")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	out := map[string]section{}
	var cur section
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			head := strings.Fields(strings.Trim(line, "[]"))
			cur = nil
			switch {
			case len(head) == 1 && (head[0] == "default" || credentials):
				cur = section{}
				out[head[0]] = cur
			case len(head) == 2 && head[0] == "profile" && !credentials:
				cur = section{}
				out[head[1]] = cur
			}
			continue
		}
		if cur == nil {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out, nil
}

var staticKey = map[string]bool{"aws_access_key_id": true, "aws_secret_access_key": true, "aws_session_token": true}

// checkProfile requires the profile to exist and refuses it, and any profile
// it chains to through source_profile, if it holds a static key.
func checkProfile(profiles map[string]section, name string, depth int) error {
	s, ok := profiles[name]
	if !ok {
		return refuse(RuleProfileMissing)
	}
	for k := range staticKey {
		if s[k] != "" {
			return refuse(RuleStaticCredentials)
		}
	}
	if next := s["source_profile"]; next != "" && depth < 8 {
		if _, ok := profiles[next]; ok {
			return checkProfile(profiles, next, depth+1)
		}
	}
	return nil
}
