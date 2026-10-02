package setupdevbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/truvity/ci-actions/internal/runcmd"
)

// AWSConfig is "CI face of the AWS config": caller-supplied, because aws.ini
// is laptop-shaped (SSO + [default]) and a credential-less step consulting it
// hangs or dies on SSO. Repos that keep a CI-shaped variant name it; others
// leave the input empty and the step does not run.
//
// Values reach the code through env, never through an expression spliced into
// script text, and the value is written to GITHUB_ENV with a RANDOM-DELIMITER
// heredoc: GITHUB_ENV is line-oriented, so a value carrying a newline in the
// NAME=VALUE form would write a second variable of the caller's choosing.
func AWSConfig(e Env) error {
	e.defaults()
	return e.exportEnv("AWS_CONFIG_FILE", e.Getenv("GITHUB_WORKSPACE")+"/"+e.Getenv("AWS_CONFIG_REL"))
}

// DetectBaked says which tools the runner image already has. The image bakes
// nix (S3 substituter pre-configured in the baked nix.conf) and devbox:
// installing them again per job was the bulk of the bootstrap minute.
// GitHub-hosted runners have neither and keep the install path. Same
// principle one layer up: ask the repository what it needs (a .prototools)
// rather than making every caller declare it.
func DetectBaked(e Env) error {
	e.defaults()
	b := func(v bool) string { return strconv.FormatBool(v) }
	out := fmt.Sprintf("nix=%s\ndevbox=%s\nprototools=%s\n",
		b(pathLookup("nix", e.Getenv("PATH")) != ""),
		b(pathLookup("devbox", e.Getenv("PATH")) != ""),
		b(fileExists(".prototools")))
	return appendTo(e.Getenv("GITHUB_OUTPUT"), out)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// StripLocalTools: the npm-backed cubic CLI is LOCAL review tooling. CI never
// invokes it, and its proto plugin cannot bootstrap on a pristine runner (the
// plugin execs npm mid-materialization). devbox's init hook materializes proto
// on the FIRST devbox run, so the strip must precede it. Workspace copy only;
// nothing is committed. Nothing to strip in a repo with no .prototools, which
// is most of them.
func StripLocalTools(ctx context.Context, e Env) error {
	e.defaults()
	if !fileExists(".prototools") {
		e.printf("no .prototools; nothing to strip\n")
		return nil
	}
	b, err := os.ReadFile(".prototools")
	if err != nil {
		return err
	}
	st, _ := os.Stat(".prototools")
	var keep []string
	ls := strings.SplitAfter(string(b), "\n")
	for _, l := range ls {
		if !strings.Contains(l, "@cubic-dev-ai") {
			keep = append(keep, l)
		}
	}
	if err := os.WriteFile(".prototools", []byte(strings.Join(keep, "")), st.Mode().Perm()); err != nil {
		return err
	}
	// The strip modifies a TRACKED file. goreleaser reads `git status` and
	// stamped every snapshot "dirty" over it (a release build REFUSES outright
	// on dirty). The mutation is CI plumbing, not source: hide it from the
	// index.
	if err := e.Exec(ctx, runcmd.Cmd{Name: "git", Args: []string{"update-index", "--assume-unchanged", ".prototools"}, Stdout: e.Out, Stderr: e.Err}); err != nil {
		return fmt.Errorf("git update-index: %w", err)
	}
	return nil
}

// Materialize splits the devbox closure cost out so timings tell the truth:
// the FIRST devbox invocation transparently fetches, unpacks and registers the
// whole nix closure, hidden inside whatever step happened to run devbox first.
// This step is the closure cost; every devbox step after it is only itself.
func Materialize(ctx context.Context, e Env) error {
	e.defaults()
	start := e.Now()
	if err := e.retry(ctx, e.Out, func(w io.Writer) error { return e.devbox(ctx, w, e.Err, "true") }); err != nil {
		return fmt.Errorf("devbox run -- true: %w", err)
	}
	return appendTo(e.Getenv("GITHUB_STEP_SUMMARY"), fmt.Sprintf("- devbox closure: %ds\n", int(e.Now().Sub(start).Seconds())))
}

// Proto installs the proto toolchain: pure proto now, the closure cost lives
// in Materialize. Retries: proto's plugin fetches are flaky from the NAT'd
// runner (intermittent non-JSON responses with the token present: consecutive
// runs disagree); two attempts, the last one outside the loop so its status
// propagates. GITHUB_TOKEN and RUNNER_ARCH come through the environment.
func Proto(ctx context.Context, e Env) error {
	e.defaults()
	logPath := e.Getenv("RUNNER_TEMP") + "/bootstrap-proto.log"
	f, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	start := e.Now()
	w := io.MultiWriter(e.Out, f)
	rerr := e.retry(ctx, w, func(w io.Writer) error { return e.devbox(ctx, w, w, "proto", "use") })
	f.Sync()
	if rerr != nil {
		return fmt.Errorf("devbox run -- proto use: %w", rerr)
	}
	b, _ := os.ReadFile(logPath)
	installs := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "installed") {
			installs++
		}
	}
	return appendTo(e.Getenv("GITHUB_STEP_SUMMARY"), fmt.Sprintf("### Bootstrap (%s)\n- proto use: %ds (%d install lines; warm cache = near-zero)\n",
		e.Getenv("RUNNER_ARCH"), int(e.Now().Sub(start).Seconds()), installs))
}

// ExposeToken hands the minted token to the recipes as well as to git. A recipe
// that curls the GitHub API needs the token itself; the credential rewrite in
// GoPrivate only helps things that speak git.
//
// A DISTINCT name, never GH_TOKEN: `gh` reads GH_TOKEN implicitly, so
// overloading it would silently repoint every unrelated gh call in every
// recipe at a token scoped to other repositories.
func ExposeToken(e Env) error {
	e.defaults()
	token := e.Getenv("MODULE_TOKEN")
	e.printf("::add-mask::%s\n", token)
	// VALIDATE before writing. GITHUB_ENV is line-oriented and append-only, so
	// a value carrying a newline writes a SECOND variable of someone else's
	// choosing. Checks for a NEWLINE, not a character set: App installation
	// tokens are 383 characters and contain dots, and guessing at the shape of
	// someone else's credential is how a guard becomes an outage. Empty is
	// rejected too: a masked value stripped somewhere upstream arrives empty,
	// and writing CI_CONTENTS_TOKEN= would fail later and further away.
	if token == "" || strings.ContainsAny(token, "\n\r") {
		return e.fail("the minted token is empty or contains a newline; refusing to write it to GITHUB_ENV")
	}
	return e.exportEnv("CI_CONTENTS_TOKEN", token)
}

var reGoPrivate = regexp.MustCompile(`^[A-Za-z0-9._~/*,-]+$`)

// GoPrivate reaches private Go modules. GOPRIVATE alone is not enough and
// fails in a confusing way: it tells go to skip the proxy and go straight to
// git, and git then refuses a private repository with an authentication prompt
// that CI reports as "terminal prompts disabled".
//
// The rewrite is global rather than repo-local because `go` clones into its
// own module cache, outside any checkout.
func GoPrivate(ctx context.Context, e Env) error {
	e.defaults()
	token := e.Getenv("MODULE_TOKEN")
	// Mask FIRST, so anything below that echoes the token is redacted.
	e.printf("::add-mask::%s\n", token)
	if token == "" {
		return e.fail("go-private is set but no module token was minted — check module-app-client-id and module-app-private-key")
	}
	// GITHUB_ENV is line-oriented: a value carrying a newline writes a SECOND
	// variable of the attacker's choosing. This input comes from a caller's
	// workflow, so it is validated rather than trusted. The pattern is
	// anchored to the whole string, and a newline is outside the class, so a
	// multi-line value is rejected.
	goPrivate := e.Getenv("GO_PRIVATE")
	if !reGoPrivate.MatchString(goPrivate) {
		return e.fail("go-private must be a comma-separated list of module path patterns; got something outside [A-Za-z0-9._~/*,-]")
	}
	if err := e.exportEnv("GOPRIVATE", goPrivate); err != nil {
		return err
	}
	if err := e.Exec(ctx, runcmd.Cmd{Name: "git", Args: []string{"config", "--global", "--replace-all",
		"url.https://x-access-token:" + token + "@github.com/.insteadOf", "https://github.com/"}, Stdout: e.Out, Stderr: e.Err}); err != nil {
		return fmt.Errorf("git config: %w", err)
	}
	return nil
}

// CodeArtifact logs in. The token comes from the DEFAULT credential chain
// deliberately: on a runner that is the pool's pod identity, which carries
// <cluster>-codeartifact-read. Nothing to configure per repository.
//
// Fails LOUDLY on an empty token, which is the whole reason this step exists.
// Every repo that needed CodeArtifact had its own copy of
// `export CODEARTIFACT_AUTH_TOKEN="$(aws codeartifact ... || echo ”)"`, and
// that `|| echo ”` turned an IAM failure into an anonymous fetch, so it
// surfaced as "Invalid authentication (as an anonymous user)": a message naming
// npm while the cause was a missing IAM grant.
func CodeArtifact(ctx context.Context, e Env) error {
	e.defaults()
	domain := e.Getenv("CA_DOMAIN")
	args := []string{"codeartifact", "get-authorization-token", "--domain", domain,
		"--domain-owner", e.Getenv("CA_OWNER"), "--region", e.Getenv("CA_REGION"),
		"--query", "authorizationToken", "--output", "text"}
	// The CLI comes from the PATH on a hosted runner and from devbox on an ARC
	// runner, whose image ships no aws at all (everything but nix arrives
	// through the project's devbox). devbox prefixes Info lines to stdout, so
	// only the last line is the token and CRs are stripped.
	var out bytes.Buffer
	var err error
	if pathLookup("aws", e.Getenv("PATH")) != "" {
		err = e.Exec(ctx, runcmd.Cmd{Name: "aws", Args: args, Stdout: &out, Stderr: e.Err})
	} else {
		var raw bytes.Buffer
		err = e.devbox(ctx, &raw, e.Err, append([]string{"aws"}, args...)...)
		s := strings.TrimSuffix(raw.String(), "\n")
		if i := strings.LastIndex(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		out.WriteString(strings.ReplaceAll(s, "\r", ""))
	}
	if err != nil {
		return e.fail("CodeArtifact login failed for domain %s. The identity this build runs as needs codeartifact:GetAuthorizationToken plus sts:GetServiceBearerToken — see <cluster>-codeartifact-read.", domain)
	}
	token := trimNL(out.String())
	if token == "" || token == "None" {
		return e.fail("CodeArtifact returned an empty token for domain %s; refusing to continue, because an empty token fetches anonymously and fails later as an npm authentication error.", domain)
	}
	e.printf("::add-mask::%s\n", token)
	return e.exportEnv("CODEARTIFACT_AUTH_TOKEN", token)
}

// RetiredCacheServer: go-cache-server is retired and does nothing. It pointed
// at a cache server that was measured out of the Go path. Ignoring it silently
// would be the same mistake the module proxy made, so a caller still passing
// it gets told. The input stays until a major so that a caller's pin and its
// org variable can move separately.
func RetiredCacheServer(e Env) error {
	e.defaults()
	e.printf("::warning::go-cache-server is retired and ignored; the Go build cache goes straight to go-cache-bucket. Remove the input and the CI_GOCACHE_SERVER org variable.\n")
	return nil
}

// devboxEnv reads one key of devbox.json's env block as jq's
// `.env[$k] // empty` printed it. ok is false when the file is not plain JSON
// (devbox tolerates comments).
func devboxEnv(file string, keys ...string) (map[string]string, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, true
	}
	var doc struct {
		Env json.RawMessage `json:"env"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil, false
	}
	out := map[string]string{}
	t := strings.TrimSpace(string(doc.Env))
	if t == "" || t == "null" {
		return out, true
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(doc.Env, &m) != nil {
		return nil, false
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch s := strings.TrimSpace(string(v)); s {
		case "null", "false":
		default:
			var str string
			if json.Unmarshal(v, &str) == nil {
				out[k] = str
			} else {
				out[k] = s
			}
		}
	}
	return out, true
}

// GuardGoproxy warns when devbox.json pins env.GOPROXY: devbox run re-applies
// that env block, so it overrides the CI module proxy passed as the goproxy
// input for every recipe. A file that is not plain JSON skips the check rather
// than failing the job.
func GuardGoproxy(e Env) error {
	e.defaults()
	const file = "devbox.json"
	if !fileExists(file) {
		return nil
	}
	vals, ok := devboxEnv(file, "GOPROXY")
	if !ok {
		e.printf("%s is not plain JSON; skipping the GOPROXY check\n", file)
		return nil
	}
	if pinned := vals["GOPROXY"]; pinned != "" {
		e.printf("::warning file=%s::%s sets env.GOPROXY (%s). devbox run re-applies that env block, so it overrides the CI module proxy passed as the goproxy input for every recipe. Remove GOPROXY from %s; laptops fall back to Go's default proxy.\n", file, file, pinned, file)
	}
	return nil
}

// GuardAWS is the same class of problem as GOPROXY, for the AWS identity: the
// action points AWS_CONFIG_FILE at the CI config (profiles that take their
// credentials from the job's OIDC token), and devbox run re-applies the env
// block over it. Build tooling such as the Go cache plugin then authenticates
// with the wrong profile and the build runs cold.
func GuardAWS(e Env) error {
	e.defaults()
	const file = "devbox.json"
	if !fileExists(file) {
		return nil
	}
	for _, key := range []string{"AWS_CONFIG_FILE", "AWS_PROFILE"} {
		vals, ok := devboxEnv(file, key)
		if !ok {
			e.printf("%s is not plain JSON; skipping the AWS config check\n", file)
			return nil
		}
		if pinned := vals[key]; pinned != "" {
			e.printf("::warning file=%s::%s sets env.%s (%s). devbox run re-applies that env block, so it overrides the CI AWS config and identity (OIDC) and build tooling such as the Go cache plugin loses its credentials. Set it in shell.init_hook with a fallback instead, e.g. export %s=\"${%s:-<default>}\".\n", file, file, key, pinned, key, key)
		}
	}
	return nil
}
