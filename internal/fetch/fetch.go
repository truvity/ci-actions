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

// Download GETs a URL into a file. Redirects are followed; anything but a 200
// is an error naming the URL.
func Download(ctx context.Context, c *http.Client, url, dest string, timeout time.Duration) error {
	if c == nil {
		c = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			last = err
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			last = fmt.Errorf("GET %s: %s", url, resp.Status)
			if resp.StatusCode < 500 {
				return last
			}
			time.Sleep(time.Duration(attempt) * time.Second)
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
