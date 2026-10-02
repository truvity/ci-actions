package cluster

import (
	"encoding/json"
	"fmt"
	"os"
)

// ForkGuard: mode: shared refuses a fork pull request. First, before
// anything else in that mode resolves a kubeconfig, reads an aws.ini or logs
// into ECR: a fork's pull request receives no secrets from GitHub already,
// but the check belongs before the thing it is guarding, not after, so a
// caller who copies mode: shared into the wrong workflow is told in seconds
// rather than finding out when a credential exchange succeeds for code it
// should never have reached.
//
// It reads $GITHUB_EVENT_PATH directly rather than trusting an input, so a
// caller cannot pass `fork: false` by accident (or a stale cached value) and
// defeat the refusal it exists to make. No event payload, or an event that
// is not a pull_request, is not a fork PR: pushes, schedules and
// workflow_dispatch all take this path. A payload that cannot be read is
// refused: this is the one guard that must fail closed.
func ForkGuard(o Options) error {
	o.defaults()
	path := o.Getenv("GITHUB_EVENT_PATH")
	if path == "" {
		o.printf("no event payload — not a pull request, mode: shared may proceed\n")
		return nil
	}
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		o.printf("no event payload — not a pull request, mode: shared may proceed\n")
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(o.Err, err)
		return &ExitError{Code: 1}
	}
	var ev struct {
		PullRequest struct {
			Head struct {
				Repo struct {
					Fork any `json:"fork"`
				} `json:"repo"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		fmt.Fprintf(o.Err, "the event payload is not JSON: %v\n", err)
		return &ExitError{Code: 1}
	}
	// jq's `// false` makes null and false the same; `jq -r` prints a string
	// as it is, so the string "true" is a fork as well.
	isFork := false
	switch v := ev.PullRequest.Head.Repo.Fork.(type) {
	case bool:
		isFork = v
	case string:
		isFork = v == "true"
	}
	if isFork {
		return o.fail("mode: shared refuses a fork pull request — a fork's code must never receive this repository's cluster credentials, ECR login or kubeconfig. Use mode: kind for pull requests, and mode: shared only for pushes/merges this repository trusts.")
	}
	o.printf("not a fork pull request — mode: shared may proceed\n")
	return nil
}
