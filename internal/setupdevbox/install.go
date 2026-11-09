package setupdevbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/truvity/ci-actions/internal/fetch"
	"github.com/truvity/ci-actions/internal/ghmirror"
)

// InstallDevbox installs the devbox release binary into $RUNNER_TEMP/bin, with
// no privilege.
//
// It replaces `curl https://get.jetify.com/devbox | bash`, which is a script
// somebody else can change between two runs, run unverified as the job, and
// which installs into /usr/local/bin through root. This fetches ONE named
// release asset, checks it against the release's own checksums.txt, unpacks the
// single `devbox` binary it holds into a directory the job owns, and puts that
// directory on PATH for the steps that follow. It runs only when devbox is not
// already on the runner.
//
//	DEVBOX_VERSION        a release tag such as 0.18.4, or `latest` (the default),
//	                      which the releases page redirects to the newest tag
//	RUNNER_TEMP           where the binary lands, under bin/ (required)
//	GITHUB_PATH           the runner's PATH file, appended to when set
//	GITHUB_TOKEN          optional; when the releases page cannot be reached, `latest`
//	                      is asked of the authenticated API instead (anonymous
//	                      traffic is what GitHub sheds first in an incident)
//	DEVBOX_RELEASE_BASE   default https://github.com/jetify-com/devbox/releases
//	DEVBOX_API_BASE       default https://api.github.com/repos/jetify-com/devbox
//	DEVBOX_UNAME_M        the machine name to pick the asset for (a test seam)
//	CI_RUNNER_GITHUB_RELEASES
//	                      the runner's GitHub releases mirror, asked before
//	                      github.com for the archive and checksums.txt; unset
//	                      is today's behaviour, and a mirror error falls back
func InstallDevbox(ctx context.Context, e Env) error {
	e.defaults()
	version := orDefault(e.Getenv("DEVBOX_VERSION"), "latest")
	base := strings.TrimRight(orDefault(e.Getenv("DEVBOX_RELEASE_BASE"), "https://github.com/jetify-com/devbox/releases"), "/")
	tmp := e.Getenv("RUNNER_TEMP")
	if tmp == "" {
		fmt.Fprintln(e.Err, "RUNNER_TEMP is not set")
		return ErrFailed
	}
	dest := filepath.Join(tmp, "bin")

	var osName string
	switch runtime.GOOS {
	case "linux", "darwin":
		osName = runtime.GOOS
	default:
		return e.fail("cannot install devbox for %s: no release binary for it", runtime.GOOS)
	}
	machine := e.Getenv("DEVBOX_UNAME_M")
	if machine == "" {
		switch runtime.GOARCH {
		case "amd64":
			machine = "x86_64"
		case "arm64":
			machine = "aarch64"
		default:
			machine = runtime.GOARCH
		}
	}
	var arch string
	switch machine {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		return e.fail("cannot install devbox for %s: no release binary for it", machine)
	}

	if version == "latest" {
		tag, err := e.latestTag(ctx, base, strings.TrimRight(orDefault(e.Getenv("DEVBOX_API_BASE"), "https://api.github.com/repos/jetify-com/devbox"), "/"))
		if err != nil {
			return err
		}
		version = tag
	}

	name := fmt.Sprintf("devbox_%s_%s_%s.tar.gz", version, osName, arch)
	url := base + "/download/" + version

	work, err := os.MkdirTemp(tmp, "devbox-install.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// A runner that offers a GitHub releases mirror (CI_RUNNER_GITHUB_RELEASES)
	// is asked first for both files; an error from it falls back to GitHub.
	mirror := e.Getenv(ghmirror.EnvVar)
	if err := ghmirror.Download(ctx, e.HTTP, mirror, url+"/"+name, filepath.Join(work, name), 5*time.Minute, e.Out); err != nil {
		return e.fail("could not download %s/%s", url, name)
	}
	if err := ghmirror.Download(ctx, e.HTTP, mirror, url+"/checksums.txt", filepath.Join(work, "checksums.txt"), 3*time.Minute, e.Out); err != nil {
		return e.fail("could not download %s/checksums.txt", url)
	}

	// The checksum comes from the same release page as the archive, so it
	// proves the download is the file the release published (no truncated or
	// swapped transfer), not that the release itself is trustworthy: pin
	// devbox-version to make the choice of release a reviewed one.
	sum, err := fetch.Verify(filepath.Join(work, name), filepath.Join(work, "checksums.txt"), name)
	switch {
	case errors.Is(err, fetch.ErrNotListed):
		return e.fail("checksums.txt lists no %s; refusing to install it", name)
	case errors.Is(err, fetch.ErrMismatch):
		return e.fail("checksum mismatch for %s; refusing to install it", name)
	case err != nil:
		return err
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := extractOne(filepath.Join(work, name), "devbox", filepath.Join(dest, "devbox")); err != nil {
		return fmt.Errorf("unpacking %s: %w", name, err)
	}
	if err := appendTo(e.Getenv("GITHUB_PATH"), dest+"\n"); err != nil {
		return err
	}
	e.printf("devbox %s installed to %s/devbox (sha256 %s verified, no privilege)\n", version, dest, sum)
	return nil
}

// latestTag names the newest devbox release. The releases page redirects
// `latest` to the newest tag, with no API call, so no token and no rate limit;
// but it is anonymous traffic, and on 2026-11-09 GitHub answered anonymous
// requests, release downloads included, with 504 for hours while the status
// page said so too. So: try the page a few times, pausing longer each time, and
// when it still fails and the job has a token, ask the authenticated API.
func (e Env) latestTag(ctx context.Context, base, api string) (string, error) {
	const attempts = 5
	var last error
	for i := 1; i <= attempts; i++ {
		final, err := finalURL(ctx, base+"/latest")
		if err == nil {
			tag := final[strings.LastIndex(final, "/")+1:]
			if tag == "" || tag == "latest" {
				return "", e.fail("the latest devbox release at %s/latest named no tag (%s)", base, final)
			}
			return tag, nil
		}
		last = err
		if i < attempts {
			e.printf("::warning::could not look up the latest devbox release at %s/latest (attempt %d of %d): %v\n", base, i, attempts, err)
			e.Sleep(time.Duration(i) * 5 * time.Second)
		}
	}
	if token := e.Getenv("GITHUB_TOKEN"); token != "" {
		tag, err := apiLatestTag(ctx, api+"/releases/latest", token)
		if err == nil {
			e.printf("::notice::the releases page was unreachable; the latest devbox release (%s) was looked up through the authenticated API\n", tag)
			return tag, nil
		}
		last = fmt.Errorf("%v; the API fallback failed too: %w", last, err)
	}
	return "", e.fail("could not look up the latest devbox release at %s/latest (%v); set devbox-version to a release tag to skip the lookup", base, last)
}

// apiLatestTag is tag_name of the releases/latest answer, asked with a token.
func apiLatestTag(ctx context.Context, url, token string) (string, error) {
	c := &http.Client{Timeout: time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var v struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return "", err
	}
	if v.TagName == "" || strings.Contains(v.TagName, "/") {
		return "", fmt.Errorf("GET %s: no usable tag_name", url)
	}
	return v.TagName, nil
}

// finalURL is where a releases/latest URL ends up: the Location it redirects
// to, or the URL itself when it does not redirect. A 4xx or 5xx without a
// Location is an error.
func finalURL(ctx context.Context, url string) (string, error) {
	c := &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return url, nil
}

// extractOne writes the named regular file of a .tar.gz to dest, mode 0755.
func extractOne(archive, member, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s not in the archive", member)
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || strings.TrimPrefix(h.Name, "./") != member {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
}
