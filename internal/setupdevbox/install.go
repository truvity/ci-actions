package setupdevbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
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
//	DEVBOX_RELEASE_BASE   default https://github.com/jetify-com/devbox/releases
//	DEVBOX_UNAME_M        the machine name to pick the asset for (a test seam)
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
		// The releases page redirects `latest` to the newest tag; the Location
		// names it. No API call, so no token and no rate limit.
		final, err := finalURL(ctx, base+"/latest")
		if err != nil {
			return e.fail("could not look up the latest devbox release at %s/latest", base)
		}
		tag := final[strings.LastIndex(final, "/")+1:]
		if tag == "" || tag == "latest" {
			return e.fail("the latest devbox release at %s/latest named no tag (%s)", base, final)
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

	if err := fetch.Download(ctx, nil, url+"/"+name, filepath.Join(work, name), 5*time.Minute); err != nil {
		return e.fail("could not download %s/%s", url, name)
	}
	if err := fetch.Download(ctx, nil, url+"/checksums.txt", filepath.Join(work, "checksums.txt"), time.Minute); err != nil {
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

// finalURL is where a releases/latest URL ends up: the Location it redirects
// to, or the URL itself when it does not redirect.
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
