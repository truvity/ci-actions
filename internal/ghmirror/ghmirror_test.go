package ghmirror

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const rel = "truvity/sluis/releases/download/v1.77.5/sluisctl_1.77.5_nix-flake.tar.gz"

func TestRewrite(t *testing.T) {
	const base = "https://cache.example/github"
	for _, c := range []struct {
		name, base, in, want string
	}{
		{"unset", "", "https://github.com/" + rel, "https://github.com/" + rel},
		{"blank", " \n", "https://github.com/" + rel, "https://github.com/" + rel},
		{"not a URL", "cache.example/github", "https://github.com/" + rel, "https://github.com/" + rel},
		{"scheme only", "https://", "https://github.com/" + rel, "https://github.com/" + rel},
		{"no host", "https:///github", "https://github.com/" + rel, "https://github.com/" + rel},
		{"another scheme", "ftp://cache.example", "https://github.com/" + rel, "https://github.com/" + rel},
		{"a release download", base, "https://github.com/" + rel, base + "/" + rel},
		{"trailing slash on the base", base + "/", "https://github.com/" + rel, base + "/" + rel},
		{"padding on the base", "  " + base + "\n", "https://github.com/" + rel, base + "/" + rel},
		{"http base", "http://cache.internal:8080/github", "https://github.com/" + rel, "http://cache.internal:8080/github/" + rel},
		{"query and fragment are carried over", base, "https://github.com/" + rel + "?narHash=sha256-1Ane3celH04STagZPRX6kheUlidmCktJ%2Fc1ZLGXvlIQ%3D#sluisctl",
			base + "/" + rel + "?narHash=sha256-1Ane3celH04STagZPRX6kheUlidmCktJ%2Fc1ZLGXvlIQ%3D#sluisctl"},
		{"a prerelease tag", base, "https://github.com/a/b/releases/download/v1.77.1-rc.1/x_1.77.1-rc.1.tar.gz", base + "/a/b/releases/download/v1.77.1-rc.1/x_1.77.1-rc.1.tar.gz"},
		{"a release page", base, "https://github.com/a/b/releases/tag/v1", "https://github.com/a/b/releases/tag/v1"},
		{"latest", base, "https://github.com/a/b/releases/latest", "https://github.com/a/b/releases/latest"},
		{"latest/download", base, "https://github.com/a/b/releases/latest/download/x.tgz", "https://github.com/a/b/releases/latest/download/x.tgz"},
		{"an archive", base, "https://github.com/a/b/archive/refs/tags/v1.tar.gz", "https://github.com/a/b/archive/refs/tags/v1.tar.gz"},
		{"a git remote", base, "https://github.com/a/b", "https://github.com/a/b"},
		{"no asset", base, "https://github.com/a/b/releases/download/v1", "https://github.com/a/b/releases/download/v1"},
		{"empty tag", base, "https://github.com/a/b/releases/download//x", "https://github.com/a/b/releases/download//x"},
		{"empty owner", base, "https://github.com//b/releases/download/v1/x", "https://github.com//b/releases/download/v1/x"},
		{"dot segments", base, "https://github.com/a/b/releases/download/../x/y", "https://github.com/a/b/releases/download/../x/y"},
		{"another host", base, "https://example.com/" + rel, "https://example.com/" + rel},
		{"a host that only starts like github", base, "https://github.com.evil.example/" + rel, "https://github.com.evil.example/" + rel},
		{"plain http github", base, "http://github.com/" + rel, "http://github.com/" + rel},
		{"the mirror's own URL", base, base + "/" + rel, base + "/" + rel},
		{"a release path in a query", base, "https://github.com/a/b?x=/releases/download/v1/y", "https://github.com/a/b?x=/releases/download/v1/y"},
		{"not a URL at all", base, "devbox", "devbox"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Rewrite(c.base, c.in); got != c.want {
				t.Errorf("Rewrite(%q, %q) = %q, want %q", c.base, c.in, got, c.want)
			}
		})
	}
}

// a fake of github.com and of the mirror: both answer for what they hold.
type site struct {
	mu   sync.Mutex
	mode int // status the mirror answers with; 0 serves
	seen []string
	srv  *httptest.Server
}

func newSite(t *testing.T, mirrorStatus int) *site {
	s := &site{mode: mirrorStatus}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.URL.Path)
		s.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/github/") {
			if s.mode != 0 {
				http.Error(w, "mirror says no", s.mode)
				return
			}
			fmt.Fprint(w, "from the mirror")
			return
		}
		fmt.Fprint(w, "from github")
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// direct is a client that sends every github.com request to the fake.
func (s *site) client() *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "github.com" {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(s.srv.URL, "http://")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownload(t *testing.T) {
	for _, c := range []struct {
		name       string
		useMirror  bool
		status     int
		want, warn string
		paths      []string
	}{
		{"variable unset: github only, today's behaviour", false, 0, "from github", "", []string{"/" + rel}},
		{"mirror serves: github is not asked", true, 0, "from the mirror", "", []string{"/github/" + rel}},
		{"mirror answers 404: falls back to github", true, 404, "from github", "did not serve", []string{"/github/" + rel, "/" + rel}},
		{"mirror answers 403: falls back to github", true, 403, "from github", "did not serve", []string{"/github/" + rel, "/" + rel}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSite(t, c.status)
			base := ""
			if c.useMirror {
				base = s.srv.URL + "/github"
			}
			dest := filepath.Join(t.TempDir(), "f")
			var warn bytes.Buffer
			if err := Download(context.Background(), s.client(), base, "https://github.com/"+rel, dest, 10*time.Second, &warn); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(dest); string(b) != c.want {
				t.Errorf("got %q, want %q", b, c.want)
			}
			if c.warn == "" && warn.Len() != 0 || !strings.Contains(warn.String(), c.warn) {
				t.Errorf("warning %q, want it to contain %q", warn.String(), c.warn)
			}
			if c.warn != "" && !strings.HasPrefix(warn.String(), "::warning::") {
				t.Errorf("not a workflow warning: %q", warn.String())
			}
			// the first request of each path, in order (fetch retries a 5xx)
			var got []string
			for _, p := range s.seen {
				if len(got) == 0 || got[len(got)-1] != p {
					got = append(got, p)
				}
			}
			if strings.Join(got, ",") != strings.Join(c.paths, ",") {
				t.Errorf("requests %v, want %v", got, c.paths)
			}
		})
	}

	t.Run("mirror answers 504: falls back to github", func(t *testing.T) {
		s := newSite(t, 504)
		dest := filepath.Join(t.TempDir(), "f")
		if err := Download(context.Background(), s.client(), s.srv.URL+"/github", "https://github.com/"+rel, dest, 30*time.Second, nil); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(dest); string(b) != "from github" {
			t.Errorf("got %q", b)
		}
	})

	t.Run("mirror unreachable: falls back to github", func(t *testing.T) {
		s := newSite(t, 0)
		dead := httptest.NewServer(http.NotFoundHandler())
		base := dead.URL + "/github"
		dead.Close()
		dest := filepath.Join(t.TempDir(), "f")
		if err := Download(context.Background(), s.client(), base, "https://github.com/"+rel, dest, 30*time.Second, nil); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(dest); string(b) != "from github" {
			t.Errorf("got %q", b)
		}
	})

	t.Run("a URL that is not a release download never goes to the mirror", func(t *testing.T) {
		s := newSite(t, 0)
		dest := filepath.Join(t.TempDir(), "f")
		if err := Download(context.Background(), s.client(), s.srv.URL+"/github", "https://github.com/a/b/archive/v1.tar.gz", dest, 10*time.Second, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Join(s.seen, ",") != "/a/b/archive/v1.tar.gz" {
			t.Errorf("requests %v", s.seen)
		}
	})

	t.Run("both fail: the error is github's", func(t *testing.T) {
		s := newSite(t, 404)
		dest := filepath.Join(t.TempDir(), "f")
		err := Download(context.Background(), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("offline")
		})}, s.srv.URL+"/github", "https://github.com/"+rel, dest, 3*time.Second, nil)
		if err == nil || !strings.Contains(err.Error(), "github.com") {
			t.Errorf("%v", err)
		}
	})
}

func TestServes(t *testing.T) {
	s := newSite(t, 0)
	ctx := context.Background()
	if err := Serves(ctx, nil, s.srv.URL+"/github/"+rel, time.Second, 1<<20); err != nil {
		t.Errorf("a mirror that serves: %v", err)
	}
	if err := Serves(ctx, nil, s.srv.URL+"/github/"+rel, time.Second, 4); err == nil {
		t.Error("a file over the limit is not served")
	}
	s.mode = 504
	if err := Serves(ctx, nil, s.srv.URL+"/github/"+rel, time.Second, 1<<20); err == nil {
		t.Error("a 504 is not served")
	}
	s.mode = 404
	if err := Serves(ctx, nil, s.srv.URL+"/github/"+rel, time.Second, 1<<20); err == nil {
		t.Error("a 404 is not served")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	if err := Serves(ctx, nil, slow.URL+"/x", 100*time.Millisecond, 1<<20); err == nil {
		t.Error("a mirror that hangs is not served")
	}
}
