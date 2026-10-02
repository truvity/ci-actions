// Package gates answers "does this branch require a status check?", the
// question both fleet-discover (should a repository be processed at all)
// and devbox-parity (is there a green to wait for before arming auto-merge)
// ask, from the two unrelated places a branch can say so.
package gates

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/truvity/ci-actions/internal/ghapi"
)

// Unknown is a read that failed. It is not zero: a repository must never be
// dropped because a read failed.
const Unknown = -1

// THE REQUIRED-CHECK RULE READS TWO SOURCES.
//
// A branch can require a status check in two entirely separate ways, and a
// repository is gated when EITHER of them requires at least one:
//
//	rulesets   GET /repos/{owner}/{repo}/rules/branches/{branch}: the
//	           EFFECTIVE rules for that branch, already merged across every
//	           ruleset that applies to it, at repository and organisation
//	           level. Needs Metadata: read, which every installation token
//	           carries.
//	classic    branch protection. GET .../branches/{branch}/protection
//	           first: it is authoritative and viewer-independent, and its 404
//	           "Branch not protected" is a definitive no. It needs
//	           Administration: read, which the fleet App does not carry, so
//	           when it is not readable the reader falls back to GraphQL's
//	           refUpdateRule, which needs no permission beyond seeing the
//	           repository but reports the rule AS IT APPLIES TO THE VIEWER.
//
// Reading classic protection alone skipped every repository that had moved
// its merge gate into a ruleset, silently, since a skip is the normal
// outcome for most of an installation. Each reader returns a COUNT or
// Unknown. Unknown is not zero: a repository must never be dropped because
// a read failed.
// Rulesets counts the required status checks of the EFFECTIVE rules of a
// branch, or Unknown.
func Rulesets(ctx context.Context, c *ghapi.Client, full, branch string) int {
	status, body, err := c.Raw(ctx, "GET", fmt.Sprintf("%s/repos/%s/rules/branches/%s?per_page=100", c.BaseURL, full, escapePath(branch)), nil)
	if err != nil || status != 200 {
		return Unknown
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []json.RawMessage `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &rules); err != nil {
		return Unknown
	}
	n := 0
	for _, r := range rules {
		if r.Type == "required_status_checks" {
			n += len(r.Parameters.Checks)
		}
	}
	return n
}

// Classic counts the required status contexts of classic branch protection,
// or Unknown. byName says the GraphQL fallback addresses the branch by name
// (refs/heads/<branch>), for a branch that need not be the default one;
// otherwise it asks for the default branch's rule.
func Classic(ctx context.Context, c *ghapi.Client, full, branch string, byName bool) int {
	status, body, err := c.Raw(ctx, "GET", fmt.Sprintf("%s/repos/%s/branches/%s/protection", c.BaseURL, full, escapePath(branch)), nil)
	if err == nil {
		switch status {
		case 200:
			var p struct {
				RSC *struct {
					Contexts []json.RawMessage `json:"contexts"`
				} `json:"required_status_checks"`
			}
			if json.Unmarshal(body, &p) != nil {
				return Unknown
			}
			if p.RSC == nil {
				return 0
			}
			return len(p.RSC.Contexts)
		case 404:
			// "Branch not protected" is the endpoint saying there is no
			// classic protection. A bare "Not Found" is this token being
			// told nothing, which is not the same answer: fall through.
			var m struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(body, &m) == nil && m.Message == "Branch not protected" {
				return 0
			}
		}
	}

	owner, name := full[:strings.Index(full, "/")], full[strings.Index(full, "/")+1:]
	query := "query($o:String!,$r:String!){repository(owner:$o,name:$r){defaultBranchRef{refUpdateRule{requiredStatusCheckContexts}}}}"
	vars := map[string]string{"o": owner, "r": name}
	if byName {
		query = "query($o:String!,$r:String!,$b:String!){repository(owner:$o,name:$r){defaultBranchRef:ref(qualifiedName:$b){refUpdateRule{requiredStatusCheckContexts}}}}"
		vars["b"] = "refs/heads/" + branch
	}
	q, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	status, body, err = c.Raw(ctx, "POST", c.BaseURL+"/graphql", q)
	if err != nil || status != 200 {
		return Unknown
	}
	// GraphQL answers 200 with an `errors` array, so the status is not the
	// whole story; a null repository is a read this token did not get.
	var g struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Repository *struct {
				DefaultBranchRef struct {
					RefUpdateRule *struct {
						Contexts []json.RawMessage `json:"requiredStatusCheckContexts"`
					} `json:"refUpdateRule"`
				} `json:"defaultBranchRef"`
			} `json:"repository"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &g) != nil || len(g.Errors) > 0 || g.Data.Repository == nil {
		return Unknown
	}
	if g.Data.Repository.DefaultBranchRef.RefUpdateRule == nil {
		return 0
	}
	return len(g.Data.Repository.DefaultBranchRef.RefUpdateRule.Contexts)
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return strings.Join(segs, "/")
}

// Describe is how a count reads in a summary line.
func Describe(n int) string {
	switch n {
	case Unknown:
		return "could not read"
	case 0:
		return "none"
	}
	return fmt.Sprint(n)
}
