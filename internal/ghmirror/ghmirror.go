// Package ghmirror sends GitHub release downloads through the runner's
// pull-through cache, when the runner offers one.
//
// GitHub's release-download edge answers 5xx for minutes at a time (2026-11-05
// and 2026-11-09). A runner pool that has a cache in front of it exports
//
//	CI_RUNNER_GITHUB_RELEASES=<cache base>/github      (no trailing slash)
//
// and a download of
//
//	https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>
//
// is asked for at
//
//	$CI_RUNNER_GITHUB_RELEASES/<owner>/<repo>/releases/download/<tag>/<asset>
//
// first. Only the leading `https://github.com/` is swapped, and only for
// /releases/download/ URLs: a query string or fragment (a flake reference's
// ?narHash=...#attr) is carried over byte for byte. With the variable unset or
// empty nothing changes. The mirror must never make a job worse, so a mirror
// that answers an error is followed by the direct GitHub URL.
package ghmirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/truvity/ci-actions/internal/fetch"
)

// EnvVar is the variable a runner exports.
const EnvVar = "CI_RUNNER_GITHUB_RELEASES"

const upstream = "https://github.com/"

// MirrorTimeout is how long one request to the mirror may take. A file the
// mirror does not hold yet is fetched from GitHub by the mirror, with up to
// two minutes of its own retries, and stored before the first byte is
// answered (a 302 to the stored object, which the client follows), so the
// wait must cover that and the transfer.
const MirrorTimeout = 10 * time.Minute

// versionTag is a tag that names a release that will not change. The mirror
// stores assets as immutable, so a moving tag (latest, nightly, edge, main)
// is never sent to it: only a tag that is a version, with or without the v.
func versionTag(tag string) bool {
	if tag == "" {
		return false
	}
	if tag[0] == 'v' {
		tag = tag[1:]
	}
	return tag != "" && tag[0] >= '0' && tag[0] <= '9'
}

// Base normalises the runner's value: surrounding whitespace and trailing
// slashes go. Anything that is not an http(s) URL with a host is treated as
// unset (""), so a malformed value can only ever mean "no mirror", never a
// rewrite to nowhere.
func Base(raw string) string {
	b := strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, scheme := range []string{"https://", "http://"} {
		if rest, ok := strings.CutPrefix(b, scheme); ok {
			if rest == "" || strings.ContainsAny(rest, " \t\r\n") || strings.HasPrefix(rest, "/") {
				return ""
			}
			return b
		}
	}
	return ""
}

// Rewrite returns the mirror's URL for a GitHub release download, or u
// unchanged when base is unset or u is not a /releases/download/ URL of
// github.com. base is the raw variable; Rewrite normalises it.
func Rewrite(base, u string) string {
	b := Base(base)
	if b == "" {
		return u
	}
	rest, ok := strings.CutPrefix(u, upstream)
	if !ok {
		return u
	}
	path := rest
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		path = rest[:i]
	}
	// owner/repo/releases/download/<tag>/<asset...>
	parts := strings.Split(path, "/")
	if len(parts) < 6 || parts[2] != "releases" || parts[3] != "download" {
		return u
	}
	for _, p := range parts {
		if p == ".." || p == "." {
			return u
		}
	}
	for _, p := range parts[:6] {
		if p == "" {
			return u
		}
	}
	if !versionTag(parts[4]) {
		return u
	}
	return b + "/" + rest
}

// Download fetches url into dest. With a mirror and a release URL it asks the
// mirror first, once, for MirrorTimeout (redirects are followed), and on any
// error (404, 413, 502 and a timeout alike) says so on warn and fetches the
// direct URL with the caller's timeout and retries. Without a mirror it is
// exactly fetch.Download.
func Download(ctx context.Context, c *http.Client, base, url, dest string, timeout time.Duration, warn io.Writer) error {
	if m := Rewrite(base, url); m != url {
		err := fetch.DownloadOnce(ctx, c, m, dest, MirrorTimeout)
		if err == nil {
			return nil
		}
		if warn != nil {
			fmt.Fprintf(warn, "::warning::the GitHub releases mirror did not serve %s (%v); fetching it from github.com\n", url, err)
		}
	}
	return fetch.Download(ctx, c, url, dest, timeout)
}

// Serves reports whether the mirror serves the whole file at u: a GET that
// answers 200 and delivers its body within timeout (at most limit bytes are
// read). It is for callers that cannot fall back mid-way, such as nix fetching
// a flake tarball by itself: they ask first and use the mirror only if it
// answered.
func Serves(ctx context.Context, c *http.Client, u string, timeout time.Duration, limit int64) error {
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("GET %s: more than %d bytes", u, limit)
	}
	return nil
}
