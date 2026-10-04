package pklfleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DiscoverOptions are the environment of the two discover-job steps.
type DiscoverOptions struct {
	Common
	Token   string
	API     string
	Source  string // SOURCE: owner/name of the contracts library
	Version string // VERSION: the input of `target`, the resolved version of `consumers`
	Repos   string // REPOS: JSON array of owner/name (`consumers`)
	HTTP    *http.Client
}

func (o *DiscoverOptions) api(accept string) api {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return api{o: &PublishOptions{Token: o.Token}, http: o.HTTP, accept: accept}
}

// Target resolves the version to move consumers to: the input (a leading `v`
// allowed), or the latest release of the source. It must be a version and a
// published release. Outputs `version`.
func Target(ctx context.Context, o DiscoverOptions) error {
	o.init()
	a := o.api("")
	if !sourceOnly.MatchString(o.Source) {
		return o.fail("source '%s' is not owner/name", o.Source)
	}
	version := strings.TrimPrefix(o.Version, "v")
	if version == "" {
		raw, err := a.do(ctx, "GET", o.API+"/repos/"+o.Source+"/releases/latest", nil)
		var rel struct {
			TagName *string `json:"tag_name"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &rel)
		}
		if err != nil {
			return o.fail("could not read the latest release of %s", o.Source)
		}
		if rel.TagName == nil || *rel.TagName == "" {
			return o.fail("%s has no release", o.Source)
		}
		version = strings.TrimPrefix(*rel.TagName, "v")
	}
	if !verOnly.MatchString(version) {
		return o.fail("'%s' is not a version (X.Y.Z, optionally -prerelease)", version)
	}
	if _, err := a.do(ctx, "GET", o.API+"/repos/"+o.Source+"/releases/tags/v"+version, nil); err != nil {
		return o.fail("%s has no published release v%s — nothing to move to", o.Source, version)
	}
	if err := appendFile(o.Output, "version="+version+"\n"); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "moving consumers of %s to v%s\n", o.Source, version)
	return nil
}

var pklProjectPath = regexp.MustCompile(`(^|/)PklProject$`)

// escapeURI is jq's @uri, per path segment.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		var b strings.Builder
		for _, c := range []byte(s) {
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte("-_.~", c) >= 0 {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
		segs[i] = b.String()
	}
	return strings.Join(segs, "/")
}

// Consumers narrows the discovered repositories to those whose PklProject
// files depend on the source at a version other than the target (and have a
// devbox.json to run their recipes in). A repository that cannot be read is an
// error annotation, and the rest still run. Outputs `repositories` and `count`.
func Consumers(ctx context.Context, o DiscoverOptions) error {
	o.init()
	var repos []string
	if err := json.Unmarshal([]byte(o.Repos), &repos); err != nil {
		return fmt.Errorf("REPOS is not a JSON array of repository names: %w", err)
	}
	rest := o.api("")
	raw := o.api("application/vnd.github.raw+json")
	find := regexp.MustCompile(`(?:project)?package://github\.com/` + regexp.QuoteMeta(o.Source) + `/releases/download/[^"' #]*`)
	out := []string{}
	for _, repo := range repos {
		if repo == "" {
			continue
		}
		base, err := defaultBranch(ctx, rest, o.API, repo)
		if err != nil || base == "" || base == "null" {
			fmt.Fprintf(o.Out, "::error::%s: could not read the repository\n", repo)
			continue
		}
		body, err := rest.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", o.API, repo, base), nil)
		var tree struct {
			Truncated bool `json:"truncated"`
			Tree      []struct {
				Path string `json:"path"`
				Type string `json:"type"`
			} `json:"tree"`
		}
		if err == nil {
			err = unmarshal(body, &tree)
		}
		if err != nil {
			fmt.Fprintf(o.Out, "::error::%s: could not read the tree of %s\n", repo, base)
			continue
		}
		if tree.Truncated {
			fmt.Fprintf(o.Out, "::warning::%s: the tree of %s is truncated; a PklProject may be missed\n", repo, base)
		}
		var projects []string
		hasDevbox := false
		for _, e := range tree.Tree {
			if e.Type == "blob" && pklProjectPath.MatchString(e.Path) {
				projects = append(projects, e.Path)
			}
			if e.Path == "devbox.json" {
				hasDevbox = true
			}
		}
		if len(projects) == 0 {
			fmt.Fprintf(o.Out, "%s: no PklProject\n", repo)
			continue
		}
		uses, stale, unread := 0, 0, false
		for _, p := range projects {
			content, err := raw.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", o.API, repo, escapePath(p), base), nil)
			if err != nil {
				fmt.Fprintf(o.Out, "::error::%s: could not read %s\n", repo, p)
				unread = true
				break
			}
			for _, line := range strings.Split(string(content), "\n") {
				for _, uri := range find.FindAllString(line, -1) {
					uses++
					if !strings.Contains(uri, "/v"+o.Version+"/") {
						stale++
					}
				}
			}
		}
		if unread {
			continue
		}
		if uses == 0 {
			fmt.Fprintf(o.Out, "%s: PklProject files do not depend on %s\n", repo, o.Source)
			continue
		}
		if stale == 0 {
			fmt.Fprintf(o.Out, "%s: already at v%s\n", repo, o.Version)
			continue
		}
		if !hasDevbox {
			fmt.Fprintf(o.Out, "::error::%s: depends on %s but has no devbox.json to run its recipes in\n", repo, o.Source)
			continue
		}
		fmt.Fprintf(o.Out, "%s: %d of %d dependency URIs to move\n", repo, stale, uses)
		out = append(out, repo)
	}
	list := jsonCompact(out)
	return appendFile(o.Output, fmt.Sprintf("repositories=%s\ncount=%d\n", list, len(out)))
}

func defaultBranch(ctx context.Context, a api, apiURL, repo string) (string, error) {
	b, err := a.do(ctx, "GET", apiURL+"/repos/"+repo, nil)
	if err != nil {
		return "", err
	}
	var r struct {
		DefaultBranch json.RawMessage `json:"default_branch"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if len(r.DefaultBranch) == 0 {
		return "null", nil
	}
	var s string
	if json.Unmarshal(r.DefaultBranch, &s) == nil {
		return s, nil
	}
	return string(r.DefaultBranch), nil
}

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func jsonCompact(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}
