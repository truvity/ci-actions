package devboxparity

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ensureGH installs the GitHub CLI only when it is missing. The self-hosted
// image bakes nix, devbox and the build tools, not the GitHub CLI the hosted
// image happens to preinstall. It is pinned and fetched for the runner's own
// architecture (the pool is arm64, hosted runners are amd64: a hardcoded
// arch put an amd64 binary on an arm64 node, "Exec format error"), and
// verified against the release's checksums.
func (r *run) ensureGH(ctx context.Context) error {
	if gh := lookPath("gh", envLookup(r.env, "PATH")); gh != "" {
		var out strings.Builder
		_ = r.o.Exec(ctx, Cmd{Name: gh, Args: []string{"--version"}, Env: r.env, Stdout: &out, Stderr: io.Discard})
		first, _, _ := strings.Cut(out.String(), "\n")
		r.printf("gh present: %s\n", first)
		return nil
	}
	v := r.o.GHVersion
	arch := r.o.ArchOverride
	if arch == "" {
		switch runtime.GOARCH {
		case "amd64":
			arch = "x86_64"
		case "arm64":
			arch = "aarch64"
		default:
			arch = runtime.GOARCH
		}
	}
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		r.printf("unsupported arch: %s\n", arch)
		return errReported
	}
	tmp := r.o.RunnerTemp
	if tmp == "" {
		tmp = os.TempDir()
	}
	name := fmt.Sprintf("gh_%s_linux_%s.tar.gz", v, arch)
	base := fmt.Sprintf("%s/v%s", r.o.GHBaseURL, v)
	archive := filepath.Join(tmp, "gh.tgz")
	if err := download(ctx, base+"/"+name, archive); err != nil {
		return fmt.Errorf("downloading gh: %w", err)
	}
	sums := filepath.Join(tmp, "gh_checksums.txt")
	if err := download(ctx, fmt.Sprintf("%s/gh_%s_checksums.txt", base, v), sums); err != nil {
		return fmt.Errorf("downloading the gh checksums: %w", err)
	}
	if err := verifyChecksum(archive, sums, name); err != nil {
		r.printf("::error::checksum mismatch for %s; refusing to run it\n", name)
		return errReported
	}
	dest := filepath.Join(tmp, "gh")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := untar(archive, dest); err != nil {
		return fmt.Errorf("unpacking gh: %w", err)
	}
	bin := filepath.Join(dest, "bin")
	if p := r.o.GithubPath; p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintln(f, bin)
		f.Close()
	}
	r.env = withEnv(r.env, nil, "PATH="+bin+":"+envLookup(r.env, "PATH"))
	r.printf("gh installed: v%s\n", v)
	return nil
}

func lookPath(name, pathEnv string) string {
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	if pathEnv == "" {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func download(ctx context.Context, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func verifyChecksum(file, sums, name string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	sf, err := os.Open(sums)
	if err != nil {
		return err
	}
	defer sf.Close()
	sc := bufio.NewScanner(sf)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if fields[0] == got {
				return nil
			}
			return fmt.Errorf("mismatch")
		}
	}
	return fmt.Errorf("%s is not in the checksums", name)
}

// untar unpacks a .tar.gz into dest with the first path component stripped
// (tar --strip-components=1). Entries that would land outside dest are an
// error.
func untar(archive, dest string) error {
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
			return nil
		}
		if err != nil {
			return err
		}
		_, rel, ok := strings.Cut(h.Name, "/")
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dest, rel)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path %q in archive", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}
