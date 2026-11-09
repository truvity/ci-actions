// Package fetch downloads a release asset and checks it against the release's
// own checksums file: the part of "install a binary without a script" that
// every installer here shares.
package fetch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Attempts and Backoff set how stubborn Download is. GitHub's release downloads
// answer 5xx for minutes at a time (2026-11-05 and 2026-11-09), so the pause
// grows with each attempt: with the defaults a download gives up after about
// 75 seconds of waiting. Sleep is the test seam.
var (
	Attempts = 6
	Backoff  = 5 * time.Second
	Sleep    = time.Sleep
)

// DownloadOnce is Download with one attempt and no pause: for a source that is
// followed by a fallback, where a retry would only delay the fallback.
func DownloadOnce(ctx context.Context, c *http.Client, url, dest string, timeout time.Duration) error {
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
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
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Download GETs a URL into a file. Redirects are followed; anything but a 200
// is an error naming the URL. A transport error or a 5xx is tried again, up to
// Attempts times with a growing pause; a 4xx is final.
func Download(ctx context.Context, c *http.Client, url, dest string, timeout time.Duration) error {
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for attempt := 1; attempt <= Attempts; attempt++ {
		if attempt > 1 {
			Sleep(time.Duration(attempt-1) * Backoff)
		}
		if ctx.Err() != nil {
			break
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			last = fmt.Errorf("GET %s: %s", url, resp.Status)
			if resp.StatusCode < 500 {
				return last
			}
			continue
		}
		f, err := os.Create(dest)
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(f, resp.Body)
		resp.Body.Close()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			last = err
			continue
		}
		return nil
	}
	if last == nil {
		last = ctx.Err()
	}
	return last
}

// ErrNotListed means the checksums file does not name the asset.
var ErrNotListed = errors.New("not listed in the checksums")

// ErrMismatch means the file is not the one the checksums name.
var ErrMismatch = errors.New("checksum mismatch")

// Verify checks file against the `<sha256>  <name>` line of a checksums file.
func Verify(file, sums, name string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	sf, err := os.Open(sums)
	if err != nil {
		return "", err
	}
	defer sf.Close()
	sc := bufio.NewScanner(sf)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if fields[0] == got {
				return got, nil
			}
			return got, ErrMismatch
		}
	}
	return got, ErrNotListed
}
