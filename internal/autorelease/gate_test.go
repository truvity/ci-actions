package autorelease

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truvity/ci-actions/internal/runcmd"
)

func prJSON(title, login, typ string, labels ...string) string {
	type lb struct {
		Name string `json:"name"`
	}
	ls := []lb{}
	for _, l := range labels {
		ls = append(ls, lb{l})
	}
	b, _ := json.Marshal([]map[string]any{{"number": 7, "title": title, "user": map[string]string{"login": login, "type": typ}, "labels": ls}})
	return string(b)
}

func subjects(s ...string) string { return strings.Join(s, "\n") + "\n" }

func TestGate(t *testing.T) {
	const (
		H, U, R, B = "alice", "User", "renovate[bot]", "Bot"
	)
	dep := "dependencies"
	for _, tc := range []struct {
		name, want string // want: release or batch
		pulls      string
		subjects   string // what `gh api .../pulls/7/commits --jq` prints
		head       string
	}{
		{"human fix releases", "release", prJSON("fix: handle nil", H, U), "", ""},
		{"human fix with scope releases", "release", prJSON("fix(api): handle nil", H, U), "", ""},
		{"human fix! releases (still one patch)", "release", prJSON("fix!: drop old flag", H, U), "", ""},
		{"human fix(scope)! releases", "release", prJSON("fix(api)!: drop old flag", H, U), "", ""},
		{"renovate fix(deps) batches", "batch", prJSON("fix(deps): update module x", R, B, dep), "", ""},
		{"renovate fix by label only batches", "batch", prJSON("fix: bump y", "alice", U, dep), "", ""},
		{"feat batches", "batch", prJSON("feat: add flag", H, U), "", ""},
		{"chore batches", "batch", prJSON("chore: tidy", H, U), "", ""},
		{"docs batches", "batch", prJSON("docs: reword", H, U), "", ""},
		{"security label releases", "release", prJSON("chore(deps): bump z", R, B, dep, "security"), "", ""},
		{"security label on a feat releases", "release", prJSON("feat: x", H, U, "security"), "", ""},
		{"revert of a fix batches", "batch", prJSON(`Revert "fix: handle nil"`, H, U), subjects(`Revert "fix: handle nil"`), ""},
		{"revert: type batches", "batch", prJSON("revert: fix: handle nil", H, U), "", ""},
		{"rebase merge, untitled-type PR, one fix commit releases", "release", prJSON("Tidy up the parser", H, U), subjects("chore: rename", "fix(parser): off by one", "docs: note"), ""},
		{"rebase merge, untitled-type PR, no fix commit batches", "batch", prJSON("Tidy up the parser", H, U), subjects("chore: rename", "feat: add", "docs: note"), ""},
		{"fix title over feat commits releases (title wins)", "release", prJSON("fix: handle nil", H, U), subjects("feat: add"), ""},
		{"feat title over a fix commit batches (title wins)", "batch", prJSON("feat: add flag", H, U), subjects("fix: typo"), ""},
		{"renovate PR with unconventional title and fix commit batches", "batch", prJSON("Update dependency x", R, B, dep), subjects("fix(deps): update x"), ""},
		{"direct push of a human fix releases", "release", "[]", "", `{"commit":{"message":"fix: hot\n\nbody"},"author":{"login":"alice","type":"User"}}`},
		{"direct push of a renovate fix batches", "batch", "[]", "", `{"commit":{"message":"fix(deps): x"},"author":{"login":"renovate[bot]","type":"Bot"}}`},
		{"direct push of a chore batches", "batch", "[]", "", `{"commit":{"message":"chore: x"},"author":{"login":"alice","type":"User"}}`},
		{"direct push with no author batches", "batch", "[]", "", `{"commit":{"message":"chore: x"},"author":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			o := GateOptions{Repo: "o/r", SHA: "abc", Summary: filepath.Join(dir, "sum"), Output: filepath.Join(dir, "out")}
			o.Exec = func(_ context.Context, c runcmd.Cmd) error {
				if c.Name != "gh" || c.Args[0] != "api" {
					t.Fatalf("unexpected command %v", c)
				}
				switch p := c.Args[1]; {
				case strings.HasSuffix(p, "/commits/abc/pulls"):
					c.Stdout.Write([]byte(tc.pulls))
				case strings.HasSuffix(p, "/pulls/7/commits"):
					c.Stdout.Write([]byte(tc.subjects))
				case strings.HasSuffix(p, "/commits/abc"):
					c.Stdout.Write([]byte(tc.head))
				default:
					t.Fatalf("unexpected gh path %s", p)
				}
				return nil
			}
			if err := Gate(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			out, _ := os.ReadFile(o.Output)
			sum, _ := os.ReadFile(o.Summary)
			got := "release"
			if string(out) == "skip=true\n" {
				got = "batch"
			} else if len(out) != 0 {
				t.Errorf("GITHUB_OUTPUT %q", out)
			}
			if got != tc.want {
				t.Errorf("got %s, want %s (summary %q)", got, tc.want, sum)
			}
			if got == "batch" && !strings.HasSuffix(string(sum), " — Monday's batch carries it\n") {
				t.Errorf("batch summary %q", sum)
			}
			if got == "release" && !strings.HasSuffix(string(sum), " — releasing outside the weekly batch\n") {
				t.Errorf("release summary %q", sum)
			}
		})
	}
}

func TestGateSummaries(t *testing.T) {
	for _, tc := range []struct{ pulls, sub, want string }{
		{prJSON("feat: x", "a", "User", "security"), "", "security-labeled merge — releasing outside the weekly batch\n"},
		{prJSON("fix: x", "a", "User"), "", "fix merge (#7) — releasing outside the weekly batch\n"},
		{prJSON("Tidy", "a", "User"), "fix: y\n", "fix commit in unconventionally titled merge (#7) — releasing outside the weekly batch\n"},
	} {
		sum := filepath.Join(t.TempDir(), "sum")
		err := Gate(context.Background(), GateOptions{Repo: "o/r", SHA: "abc", Summary: sum, Exec: func(_ context.Context, c runcmd.Cmd) error {
			if strings.HasSuffix(c.Args[1], "/pulls") {
				c.Stdout.Write([]byte(tc.pulls))
			} else {
				c.Stdout.Write([]byte(tc.sub))
			}
			return nil
		}})
		got, _ := os.ReadFile(sum)
		if err != nil || string(got) != tc.want {
			t.Errorf("summary %q (err %v), want %q", got, err, tc.want)
		}
	}
}

func TestGateGhFailure(t *testing.T) {
	err := Gate(context.Background(), GateOptions{Repo: "o/r", SHA: "abc", Exec: func(context.Context, runcmd.Cmd) error { return os.ErrPermission }})
	if err == nil {
		t.Error("a failed gh call must fail the gate: a gate that cannot see the PR must not decide")
	}
}
