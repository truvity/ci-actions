// Package ssmsecrets is the port of the ssm-secrets action: read the
// parameters directly under one AWS SSM Parameter Store path, as the JOB, with
// no stored credential anywhere.
//
// The chain:
//
//  1. `sluisctl token --audience aws:<account>:<role>` exchanges the job's
//     GitHub OIDC identity token at the sluis issuer. The issuer's grants
//     decide: a job reaches this audience only if its matchers (repository,
//     ref, event, workflow files) admit it.
//  2. That token is the web identity of an STS AssumeRoleWithWebIdentity call
//     for the role of the audience. The credentials it returns live in this
//     process only: nothing is written to an AWS config or credentials file
//     and nothing is exported to later steps.
//  3. One GetParametersByPath (decrypted, not recursive) under the path.
//
// Nothing here is rotated, copied into a repository, or held between runs.
//
// NO SECRET VALUE IS EVER PRINTED except as the argument of an `::add-mask::`
// command, which is how the runner is told to hide it: every value is masked,
// line by line, BEFORE it is written anywhere, so a multi-line secret is
// masked whole. The exchanged token and the temporary AWS credentials are
// masked too.
package ssmsecrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// SSMAPI is the one call of the SSM client this action makes. The real client
// is *ssm.Client; tests fake it.
type SSMAPI interface {
	GetParametersByPath(ctx context.Context, in *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

// Options are the action's inputs and the runner's environment.
type Options struct {
	Issuer     string
	Account    string
	Role       string
	Region     string
	Path       string
	Wanted     string // keys, whitespace- or comma-separated; empty takes all
	SluisctlBy string // "auto" or "direct"

	IDTokenRequestURL string // ACTIONS_ID_TOKEN_REQUEST_URL
	RunnerTemp        string
	GithubOutput      string
	Dir               string // where devbox.json is looked for; "" is the working directory

	Out, Err io.Writer
	// Token runs sluisctl and returns its stdout. Nil runs the real one.
	Token func(ctx context.Context, args []string) (string, error)
	// Client builds the SSM client from the exchanged token. Nil assumes the
	// role with it and builds the real one.
	Client func(ctx context.Context, token string) (SSMAPI, error)

	endpoint string // tests only: serves STS and SSM
}

// ErrReported marks a failure whose ::error:: line is already written.
var ErrReported = errors.New("ssm-secrets: reported")

var (
	accountRE = regexp.MustCompile(`^[0-9]{12}$`)
	// IAM role names: letters, digits and + = , . @ _ -, up to 64.
	roleRE   = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`)
	regionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
)

func (o *Options) defaults() {
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

// Audience is the exchange audience for a role: aws:<account>:<role>.
func Audience(account, role string) string { return "aws:" + account + ":" + role }

// partition is the AWS partition a region belongs to.
func partition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	}
	return "aws"
}

// RoleARN is the role the token is exchanged for. The partition is an
// argument, not a literal: the leak canary bans the ARN prefix as text, and
// this is the mechanism that writes it.
func RoleARN(region, account, role string) string {
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", partition(region), account, role)
}

// Parameter is one SSM parameter under the path, by its leaf name.
type Parameter struct {
	Leaf  string
	Value string
}

// EnvName is the environment variable a leaf is exported as: upper-cased
// ASCII with dashes turned into underscores (`maven-username` ->
// `MAVEN_USERNAME`).
func EnvName(leaf string) string {
	return strings.ReplaceAll(asciiUpper(leaf), "-", "_")
}

// Leaf is the last segment of a parameter name.
func Leaf(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// SelectKeys applies the keys filter: every parameter when wanted is blank,
// else exactly the wanted leaves, in the order asked, and an error naming a
// leaf that is missing.
func SelectKeys(params []Parameter, wanted string) ([]Parameter, error) {
	if strings.TrimSpace(strings.NewReplacer(",", " ").Replace(wanted)) == "" {
		out := append([]Parameter(nil), params...)
		sort.Slice(out, func(i, j int) bool { return out[i].Leaf < out[j].Leaf })
		return out, nil
	}
	byLeaf := make(map[string]Parameter, len(params))
	for _, p := range params {
		byLeaf[p.Leaf] = p
	}
	var out []Parameter
	seen := map[string]bool{}
	for _, k := range strings.Fields(strings.ReplaceAll(wanted, ",", " ")) {
		p, ok := byLeaf[k]
		if !ok {
			return nil, fmt.Errorf("no parameter %s", k)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out, nil
}

// FormatEnvFile renders the env file: one NAME='value' line per parameter,
// single-quoted with any quote in the value escaped, because a value is data
// and a file that is sourced would otherwise run it. It refuses a leaf that is
// no environment variable name, and two leaves that become the same name.
func FormatEnvFile(params []Parameter) (string, error) {
	var b strings.Builder
	names := map[string]string{}
	for _, p := range params {
		name := EnvName(p.Leaf)
		if !validEnvName(name) {
			return "", fmt.Errorf("key %s is no environment variable name", p.Leaf)
		}
		if other, dup := names[name]; dup {
			return "", fmt.Errorf("keys %s and %s are both %s", other, p.Leaf, name)
		}
		names[name] = p.Leaf
		fmt.Fprintf(&b, "%s='%s'\n", name, strings.ReplaceAll(p.Value, "'", `'\''`))
	}
	return b.String(), nil
}

// Run reads the path and writes the env file and the outputs.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	out := o.Out
	fail := func(format string, a ...any) error {
		fmt.Fprintf(out, "::error::"+format+"\n", a...)
		return ErrReported
	}

	for _, f := range []struct{ name, val string }{{"ISSUER", o.Issuer}, {"ACCOUNT", o.Account}, {"ROLE", o.Role}, {"REGION", o.Region}, {"SSM_PATH", o.Path}} {
		if f.val == "" {
			return fail("%s is required", f.name)
		}
	}
	if !accountRE.MatchString(o.Account) {
		return fail("account must be the 12-digit AWS account id")
	}
	if !roleRE.MatchString(o.Role) {
		return fail("role must be a role name, not an ARN or a path")
	}
	if !regionRE.MatchString(o.Region) {
		return fail("region %q is not an AWS region name", o.Region)
	}
	if !strings.HasPrefix(o.Path, "/") {
		return fail("path must start with /")
	}
	path := strings.TrimRight(o.Path, "/")
	if path == "" {
		return fail("path must name a prefix, not the root")
	}
	if o.IDTokenRequestURL == "" {
		return fail("this job has no id-token permission; the read is an exchange of the job's own identity token")
	}

	audience := Audience(o.Account, o.Role)
	tokenFn := o.Token
	if tokenFn == nil {
		tokenFn = o.sluisctl
	}
	fmt.Fprintf(out, "exchanging this job's identity for the %s audience\n", audience)
	raw, err := tokenFn(ctx, []string{"token", "--issuer", o.Issuer, "--audience", audience})
	if err != nil {
		return fail("sluisctl could not exchange this job's token for %s -- the issuer's grants decide, so check the job's matchers", audience)
	}
	token := strings.ReplaceAll(lastLine(raw), "\r", "")
	if token == "" {
		return fail("sluisctl returned no token for %s", audience)
	}
	mask(out, token)

	newClient := o.Client
	if newClient == nil {
		newClient = o.realClient
	}
	client, err := newClient(ctx, token)
	if err != nil {
		return fail("could not assume role %s with the exchanged token: %v", o.Role, err)
	}

	var params []Parameter
	var next *string
	for {
		res, err := client.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:           aws.String(path),
			Recursive:      aws.Bool(false),
			WithDecryption: aws.Bool(true),
			NextToken:      next,
		})
		if err != nil {
			return fail("reading %s failed: %v", path, err)
		}
		for _, p := range res.Parameters {
			leaf := Leaf(aws.ToString(p.Name))
			if leaf == "" {
				continue
			}
			value := strings.TrimRight(aws.ToString(p.Value), "\n")
			// Masked as soon as it is read, whatever happens to it next.
			mask(out, value)
			params = append(params, Parameter{Leaf: leaf, Value: value})
		}
		if res.NextToken == nil || *res.NextToken == "" {
			break
		}
		next = res.NextToken
	}
	if len(params) == 0 {
		return fail("%s holds no parameters", path)
	}

	selected, err := SelectKeys(params, o.Wanted)
	if err != nil {
		return fail("%s: %v", path, err)
	}
	body, err := FormatEnvFile(selected)
	if err != nil {
		return fail("%s: %v", path, err)
	}

	tmp := o.RunnerTemp
	if tmp == "" {
		tmp = os.TempDir()
	}
	envFile := filepath.Join(tmp, "ssm-secrets-"+nonAlnumToDash(path)+".env")
	f, err := os.OpenFile(envFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.Chmod(envFile, 0o600); err != nil {
		return err
	}
	if _, err := io.WriteString(f, body); err != nil {
		return err
	}
	fmt.Fprintf(out, "read %d key(s) from %s into $RUNNER_TEMP\n", len(selected), path)

	if o.GithubOutput != "" {
		var b bytes.Buffer
		fmt.Fprintf(&b, "env-file=%s\n", envFile)
		if len(selected) == 1 {
			// A heredoc delimiter that cannot occur in the value.
			var r [16]byte
			if _, err := rand.Read(r[:]); err != nil {
				return err
			}
			d := "ssm-" + hex.EncodeToString(r[:])
			fmt.Fprintf(&b, "value<<%s\n%s\n%s\n", d, selected[0].Value, d)
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

// tokenFile serves the exchanged token as the web identity of the call.
type tokenFile string

func (t tokenFile) GetIdentityToken() ([]byte, error) { return []byte(t), nil }

// realClient assumes the role with the exchanged token and builds the SSM
// client on the credentials that returns. The STS call is not signed, so the
// config it runs with carries no credentials; nothing is read from the
// environment or from an AWS profile, so a credential an earlier step left
// behind cannot take its place. The temporary credentials are retrieved at
// once, so a refusal reads as the role's, and masked.
func (o Options) realClient(ctx context.Context, token string) (SSMAPI, error) {
	base := aws.Config{Region: o.Region}
	provider := stscreds.NewWebIdentityRoleProvider(
		sts.NewFromConfig(base, func(so *sts.Options) { so.BaseEndpoint = endpointOrNil(o.endpoint) }),
		RoleARN(o.Region, o.Account, o.Role),
		tokenFile(token),
		func(w *stscreds.WebIdentityRoleOptions) { w.RoleSessionName = "ssm-secrets" },
	)
	cache := aws.NewCredentialsCache(provider)
	creds, err := cache.Retrieve(ctx)
	if err != nil {
		return nil, err
	}
	mask(o.Out, creds.SecretAccessKey)
	mask(o.Out, creds.SessionToken)
	cfg := aws.Config{Region: o.Region, Credentials: cache}
	return ssm.NewFromConfig(cfg, func(so *ssm.Options) { so.BaseEndpoint = endpointOrNil(o.endpoint) }), nil
}

func endpointOrNil(e string) *string {
	if e == "" {
		return nil
	}
	return &e
}

// mask tells the runner to hide a value, one command per line: ::add-mask::
// takes one line, so a multi-line secret masked as a whole would still print
// line by line. A line shorter than four characters is not masked (it would
// hide ordinary words).
func mask(out io.Writer, s string) {
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
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
	return strings.Trim(string(b), "-")
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

// cliName is the token exchange's command.
const cliName = "sluisctl"

// sluisctl runs the real one. It comes from the repository's devbox, where
// its version is pinned beside every other tool the job uses.
func (o Options) sluisctl(ctx context.Context, args []string) (string, error) {
	name, argv := o.exchangeCommand(args)
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = o.Dir
	cmd.Stderr = o.Err
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// exchangeCommand is the command line the exchange runs: through devbox when
// the repository has a devbox.json (unless direct), from PATH otherwise.
func (o Options) exchangeCommand(args []string) (string, []string) {
	if o.SluisctlBy != "direct" {
		if _, err := os.Stat(filepath.Join(o.Dir, "devbox.json")); err == nil {
			return "devbox", append([]string{"run", "--", cliName}, args...)
		}
	}
	return cliName, args
}
