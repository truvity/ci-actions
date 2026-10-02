// Command ci-actions is the single binary behind this repository's thin
// composite-action wrappers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/truvity/ci-actions/internal/callerparity"
	"github.com/truvity/ci-actions/internal/cluster"
	"github.com/truvity/ci-actions/internal/devboxparity"
	"github.com/truvity/ci-actions/internal/fleet"
	"github.com/truvity/ci-actions/internal/fleetdiscover"
	"github.com/truvity/ci-actions/internal/openbaosecrets"
	"github.com/truvity/ci-actions/internal/policyconformance"
	"github.com/truvity/ci-actions/internal/publicrunners"
	"github.com/truvity/ci-actions/internal/recipe"
	"github.com/truvity/ci-actions/internal/remotebuilders"
	"github.com/truvity/ci-actions/internal/repocheck"
	"github.com/truvity/ci-actions/internal/runcmd"
	"github.com/truvity/ci-actions/internal/setupdevbox"
	"github.com/truvity/ci-actions/internal/taggedpins"
)

// version is stamped by goreleaser.
var version = "dev"

const usage = `ci-actions <command>

Commands:
  tagged-pins          refuse a pin into a shared CI library that names no release
                       (env: LIBRARIES, whitespace- or comma-separated)
  policy-conformance   hold the checkout against the component contract's rules C1-C13
                       (env: STRICT, SKIP, REASON, RENOVATE_PRESET, DEFAULT_BRANCH, GITHUB_REPOSITORY,
                       GITHUB_STEP_SUMMARY; run it from the repository root)
  repo-check cache-seam  this repository's own gate: setup-devbox's cache delegation holds
                       (reads ci-cache's action at the pinned sha; env: none, run from the root)
  setup-devbox <step>  the setup-devbox action's steps: preflight, aws-config, detect-baked, strip-tools,
                       install-devbox, materialize, proto, expose-token, go-private, codeartifact,
                       retired-cache-server, guard-goproxy, guard-aws (env: see setup-devbox/action.yaml)
  recipe               run one task-runner recipe in devbox and assert a clean tree (env: RECIPE, COMMAND)
  remote-builders      register BuildKit builders as one buildx builder (env: NODES, plus
                       HOME, RUNNER_TEMP, GITHUB_ENV)
  openbao-secrets      read one OpenBAO KV path as the job's own identity (env: ISSUER, ADDRESS,
                       KV_PATH, BAO_NAMESPACE, MOUNT, AUTH_MOUNT, ROLE, AUDIENCE, WANTED, CA_CERT,
                       ACCESSCTL_MODE, GITHUB_OUTPUT)
  public-runners       refuse a self-hosted runner in a public repository
                       (env: RUNNERS, VISIBILITY, GITHUB_REPOSITORY, GH_TOKEN, GITHUB_API_URL)
  cluster <step>       the cluster action's steps: fork-guard, kind, wait, shared-connect,
                       shared-finish (env: see cluster/action.yml)
  caller-parity        compare each repository's shared caller workflows with the canonical kits
                       (env: TOKEN, REPOSITORIES, KITS or ACTION_PATH/kits, FAIL_ON_DIFF, API,
                       GITHUB_OUTPUT, GITHUB_STEP_SUMMARY)
  devbox-parity        refresh devbox, align the go toolchain and playwright followers, open one PR
                       (env: TOKEN, WORKDIR, BASE, LABEL, MODE, FULL_DAY, MODULE_DIRS, GIT_USER, GIT_EMAIL)
  fleet discover       which repositories of a GitHub App installation a fleet job works on
                       (env: TOKEN, ESTATE, REQUIRE_CHECK, REQUIRE_FILE, FILTER, ENROLLED, API,
                       GITHUB_OUTPUT, GITHUB_STEP_SUMMARY)
  fleet pins           which ci-workflows, ci-actions and setup-devbox versions each
                       repository pins, transitively (token from GITHUB_TOKEN or GH_TOKEN)
  version              print the version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "tagged-pins":
		err := taggedpins.Run(taggedpins.Options{Libraries: getenv("LIBRARIES"), Out: stdout})
		switch {
		case err == nil:
			return 0
		case errors.Is(err, taggedpins.ErrUntagged):
			return 1
		default:
			fmt.Fprintln(stderr, "tagged-pins:", err)
			return 1
		}
	case "cluster":
		return clusterCmd(ctx, args[1:], stdout, stderr, getenv)
	case "caller-parity":
		kits := getenv("KITS")
		if kits == "" {
			// the action's own kits/, next to its action.yaml
			kits = filepath.Join(getenv("ACTION_PATH"), "kits")
		}
		err := callerparity.Run(ctx, callerparity.Options{
			Token: getenv("TOKEN"), Repositories: getenv("REPOSITORIES"), Kits: kits,
			FailOnDiff: getenv("FAIL_ON_DIFF"), API: getenv("API"), Out: stdout,
			Summary: getenv("GITHUB_STEP_SUMMARY"), Output: getenv("GITHUB_OUTPUT"),
		})
		switch {
		case err == nil:
			return 0
		case errors.Is(err, callerparity.ErrReported):
			return 1
		default:
			fmt.Fprintln(stderr, "caller-parity:", err)
			return 1
		}
	case "devbox-parity":
		err := devboxparity.Run(ctx, devboxparity.Options{
			Token: getenv("TOKEN"), WorkDir: getenv("WORKDIR"), Base: getenv("BASE"), Label: getenv("LABEL"),
			Mode: getenv("MODE"), FullDay: getenv("FULL_DAY"), ModuleDirs: getenv("MODULE_DIRS"),
			GitUser: getenv("GIT_USER"), GitEmail: getenv("GIT_EMAIL"),
			GoDLURL: getenv("CI_ACTIONS_GO_DL_URL"), APIURL: getenv("GITHUB_API_URL"),
			GHBaseURL:  getenv("CI_ACTIONS_GH_DL_URL"),
			RunnerTemp: getenv("RUNNER_TEMP"), GithubOutput: getenv("GITHUB_OUTPUT"), GithubPath: getenv("GITHUB_PATH"),
			Environ: os.Environ(), Out: stdout, Err: stderr,
		})
		switch {
		case err == nil:
			return 0
		case devboxparity.IsReported(err):
			return 1
		default:
			fmt.Fprintln(stderr, "devbox-parity:", err)
			return 1
		}
	case "policy-conformance":
		err := policyconformance.Run(ctx, policyconformance.Options{
			Strict: getenv("STRICT"), Skip: getenv("SKIP"), Reason: getenv("REASON"),
			RenovatePreset: getenv("RENOVATE_PRESET"), DefaultBranch: getenv("DEFAULT_BRANCH"),
			GithubRepository: getenv("GITHUB_REPOSITORY"), SummaryPath: getenv("GITHUB_STEP_SUMMARY"), Out: stdout,
		})
		var ee *policyconformance.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &ee):
			return ee.Code
		default:
			fmt.Fprintln(stderr, "policy-conformance:", err)
			return 1
		}
	case "repo-check":
		if len(args) >= 2 && args[1] == "cache-seam" {
			root := "."
			if len(args) >= 3 {
				root = args[2]
			}
			if err := repocheck.CacheSeam(ctx, repocheck.CacheSeamOptions{Root: root, Out: stdout}); err != nil {
				if !errors.Is(err, repocheck.ErrFailed) {
					fmt.Fprintln(stderr, "repo-check:", err)
				}
				return 1
			}
			return 0
		}
		fmt.Fprint(stderr, usage)
		return 2
	case "setup-devbox":
		return setupDevbox(ctx, args[1:], stdout, stderr, getenv)
	case "recipe":
		err := recipe.Run(ctx, recipe.Options{Recipe: getenv("RECIPE"), Command: getenv("COMMAND"), Out: stdout, Err: stderr})
		switch {
		case err == nil:
			return 0
		case errors.Is(err, recipe.ErrFailed):
			return 1
		default:
			fmt.Fprintln(stderr, "recipe:", err)
			return 1
		}
	case "remote-builders":
		err := remotebuilders.Run(ctx, remotebuilders.Options{
			Nodes: getenv("NODES"), Home: getenv("HOME"), RunnerTemp: getenv("RUNNER_TEMP"), GithubEnv: getenv("GITHUB_ENV"),
			Out: stdout, Err: stderr,
		})
		switch {
		case err == nil:
			return 0
		case errors.Is(err, remotebuilders.ErrFailed):
			return 1
		default:
			fmt.Fprintln(stderr, "remote-builders:", err)
			return runcmd.ExitCode(err)
		}
	case "openbao-secrets":
		err := openbaosecrets.Run(ctx, openbaosecrets.Options{
			Issuer: getenv("ISSUER"), Address: getenv("ADDRESS"), KVPath: getenv("KV_PATH"),
			Namespace: getenv("BAO_NAMESPACE"), Mount: getenv("MOUNT"), AuthMount: getenv("AUTH_MOUNT"),
			Role: getenv("ROLE"), Audience: getenv("AUDIENCE"), Wanted: getenv("WANTED"),
			CACert: getenv("CA_CERT"), AccessctlBy: getenv("ACCESSCTL_MODE"),
			IDTokenRequestURL: getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), RunnerTemp: getenv("RUNNER_TEMP"),
			GithubOutput: getenv("GITHUB_OUTPUT"), Out: stdout, Err: stderr,
		})
		switch {
		case err == nil:
			return 0
		case errors.Is(err, openbaosecrets.ErrReported):
			return 1
		default:
			fmt.Fprintln(stderr, "openbao-secrets:", err)
			return 1
		}
	case "public-runners":
		token := getenv("GH_TOKEN")
		if token == "" {
			token = getenv("GITHUB_TOKEN")
		}
		err := publicrunners.Run(ctx, publicrunners.Options{
			Runners: getenv("RUNNERS"), Visibility: getenv("VISIBILITY"), Repository: getenv("GITHUB_REPOSITORY"),
			Token: token, APIURL: getenv("GITHUB_API_URL"), Out: stdout,
		})
		switch {
		case err == nil:
			return 0
		case errors.Is(err, publicrunners.ErrRefused):
			return 1
		default:
			fmt.Fprintln(stderr, "public-runners:", err)
			return 1
		}
	case "fleet":
		if len(args) >= 2 && args[1] == "pins" {
			return fleetPins(ctx, args[2:], stdout, stderr, getenv)
		}
		if len(args) >= 2 && args[1] == "discover" {
			err := fleetdiscover.Run(ctx, fleetdiscover.Options{
				Token: getenv("TOKEN"), Estate: getenv("ESTATE"), RequireCheck: getenv("REQUIRE_CHECK"),
				RequireFile: getenv("REQUIRE_FILE"), Filter: getenv("FILTER"), Enrolled: getenv("ENROLLED"),
				API: getenv("API"), Out: stdout, Summary: getenv("GITHUB_STEP_SUMMARY"), Output: getenv("GITHUB_OUTPUT"),
			})
			if err != nil {
				if !errors.Is(err, fleetdiscover.ErrReported) {
					fmt.Fprintln(stderr, "fleet discover:", err)
				}
				return 1
			}
			return 0
		}
		fmt.Fprint(stderr, usage)
		return 2
	default:
		fmt.Fprintf(stderr, "ci-actions: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// Exit codes of `fleet pins`: 0 ok, 1 the gate failed, 2 usage, 3 a
// repository could not be read (a gate that cannot see a repository must
// not pass it).
func fleetPins(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("fleet pins", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var orgs listFlag
	fs.Var(&orgs, "org", "organisation (or user) to scan; repeatable or comma-separated")
	wfRepo := fs.String("workflows-repo", "truvity/ci-workflows", "the reusable-workflow library")
	acRepo := fs.String("actions-repo", "truvity/ci-actions", "the composite-action library")
	minSetup := fs.String("min-setup-devbox", "", "exit 1 when any repository resolves setup-devbox below this version, e.g. v1.6.1")
	jsonOut := fs.String("json", "", "write the machine-readable report to this file ('-' for stdout, which moves the table to stderr)")
	runnerFilter := fs.String("runner-filter", "any", "only print repositories that run on: any, hosted or self-hosted (reporting only: the JSON, the gate and the exit code ignore it)")
	archived := fs.Bool("include-archived", false, "also scan archived repositories")
	all := fs.Bool("all", false, "list repositories that pin nothing too")
	conc := fs.Int("concurrency", 8, "repositories scanned at once")
	apiURL := fs.String("api-url", getenv("GITHUB_API_URL"), "GitHub API base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch *runnerFilter {
	case "any", "hosted", "self-hosted":
	default:
		fmt.Fprintf(stderr, "fleet pins: --runner-filter %q is not any, hosted or self-hosted\n", *runnerFilter)
		return 2
	}
	if len(orgs) == 0 {
		fmt.Fprintln(stderr, "fleet pins: at least one --org is required")
		return 2
	}
	token := getenv("GITHUB_TOKEN")
	if token == "" {
		token = getenv("GH_TOKEN")
	}
	if token == "" {
		fmt.Fprintln(stderr, "fleet pins: set GITHUB_TOKEN (or GH_TOKEN)")
		return 2
	}

	client := fleet.NewClient(*apiURL, token)
	rep, err := fleet.Scan(ctx, client, fleet.Config{
		Orgs: orgs, WorkflowsRepo: *wfRepo, ActionsRepo: *acRepo, IncludeArchived: *archived, Concurrency: *conc,
	})
	if err != nil {
		fmt.Fprintln(stderr, "fleet pins:", redact(err.Error(), token))
		return 1
	}
	rep.RunnerFilter = *runnerFilter
	if *minSetup != "" {
		if _, err := fleet.ApplyGate(rep, *minSetup); err != nil {
			fmt.Fprintln(stderr, "fleet pins:", err)
			return 2
		}
	}

	table := stdout
	if *jsonOut == "-" {
		table = stderr
	}
	fleet.WriteTable(table, rep, *all)
	if *jsonOut != "" {
		var w io.Writer = stdout
		if *jsonOut != "-" {
			f, err := os.Create(*jsonOut)
			if err != nil {
				fmt.Fprintln(stderr, "fleet pins:", err)
				return 1
			}
			defer f.Close()
			w = f
		}
		if err := fleet.WriteJSON(w, rep); err != nil {
			fmt.Fprintln(stderr, "fleet pins:", err)
			return 1
		}
	}
	switch {
	case len(rep.Below) > 0:
		return 1
	case len(rep.Errors) > 0:
		return 3
	}
	return 0
}

// redact is belt and braces: the client already strips the token from its
// errors.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// clusterCmd runs one step of the cluster action. The step's own exit status
// is the command's: a failed kind box exits with the box's code.
func clusterCmd(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	o := cluster.Options{Getenv: getenv, Out: stdout, Err: stderr}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "fork-guard":
		err = cluster.ForkGuard(o)
	case "kind":
		err = cluster.KindLaunch(ctx, o)
	case "wait":
		err = cluster.KindWait(o)
	case "shared-connect":
		err = cluster.SharedConnect(ctx, o)
	case "shared-finish":
		err = cluster.SharedFinish(o)
	case "box-run":
		// the detached half of `kind` with background: true
		fs := flag.NewFlagSet("cluster box-run", flag.ContinueOnError)
		fs.SetOutput(stderr)
		state := fs.String("state-dir", "", "")
		box := fs.String("box", "", "")
		kubeconfig := fs.String("kubeconfig", "", "")
		version := fs.String("policy-version", "", "")
		if fs.Parse(args[1:]) != nil {
			return 2
		}
		return cluster.BoxRun(ctx, o, *state, *box, *kubeconfig, *version)
	default:
		fmt.Fprintf(stderr, "ci-actions: unknown cluster step %q\n\n%s", args[0], usage)
		return 2
	}
	var ee *cluster.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.Code
	default:
		fmt.Fprintln(stderr, "cluster:", err)
		return 1
	}
}

// setupDevbox runs one step of the setup-devbox action.
func setupDevbox(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	e := setupdevbox.Env{Getenv: getenv, Lookup: os.LookupEnv, Out: stdout, Err: stderr}
	var err error
	switch args[0] {
	case "preflight":
		err = setupdevbox.Preflight(ctx, e)
	case "aws-config":
		err = setupdevbox.AWSConfig(e)
	case "detect-baked":
		err = setupdevbox.DetectBaked(e)
	case "strip-tools":
		err = setupdevbox.StripLocalTools(ctx, e)
	case "install-devbox":
		err = setupdevbox.InstallDevbox(ctx, e)
	case "materialize":
		err = setupdevbox.Materialize(ctx, e)
	case "proto":
		err = setupdevbox.Proto(ctx, e)
	case "expose-token":
		err = setupdevbox.ExposeToken(e)
	case "go-private":
		err = setupdevbox.GoPrivate(ctx, e)
	case "codeartifact":
		err = setupdevbox.CodeArtifact(ctx, e)
	case "retired-cache-server":
		err = setupdevbox.RetiredCacheServer(e)
	case "guard-goproxy":
		err = setupdevbox.GuardGoproxy(e)
	case "guard-aws":
		err = setupdevbox.GuardAWS(e)
	default:
		fmt.Fprintf(stderr, "ci-actions: unknown setup-devbox step %q\n\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, setupdevbox.ErrFailed):
		return 1
	default:
		fmt.Fprintln(stderr, "setup-devbox:", err)
		return runcmd.ExitCode(err)
	}
}
