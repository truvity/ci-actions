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
	"strings"

	"github.com/truvity/ci-actions/internal/fleet"
	"github.com/truvity/ci-actions/internal/taggedpins"
)

// version is stamped by goreleaser.
var version = "dev"

const usage = `ci-actions <command>

Commands:
  tagged-pins          refuse a pin into a shared CI library that names no release
                       (env: LIBRARIES, whitespace- or comma-separated)
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
	case "fleet":
		if len(args) >= 2 && args[1] == "pins" {
			return fleetPins(ctx, args[2:], stdout, stderr, getenv)
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
