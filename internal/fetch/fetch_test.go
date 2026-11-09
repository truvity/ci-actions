package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fast(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := Sleep
	Sleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { Sleep = old })
	return &slept
}

func TestDownloadRetriesServerErrors(t *testing.T) {
	slept := fast(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits++; hits < 4 {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f")
	if err := Download(context.Background(), nil, srv.URL, dest, time.Minute); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "ok" || hits != 4 || len(*slept) != 3 || (*slept)[2] != 3*Backoff {
		t.Errorf("%q hits %d slept %v", b, hits, *slept)
	}
}

func TestDownloadGivesUpAfterAttempts(t *testing.T) {
	fast(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
	}))
	defer srv.Close()
	if err := Download(context.Background(), nil, srv.URL, filepath.Join(t.TempDir(), "f"), time.Minute); err == nil || hits != Attempts {
		t.Errorf("%v hits %d", err, hits)
	}
}

func TestDownloadDoesNotRetryA4xx(t *testing.T) {
	slept := fast(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if err := Download(context.Background(), nil, srv.URL, filepath.Join(t.TempDir(), "f"), time.Minute); err == nil || hits != 1 || len(*slept) != 0 {
		t.Errorf("%v hits %d slept %v", err, hits, *slept)
	}
}
