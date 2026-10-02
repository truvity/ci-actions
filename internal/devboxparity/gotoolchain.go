package devboxparity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Pair: go. Align the TOOLCHAIN directive, never the language directive.
// Rule: toolchain = newest patch of the line golangci-lint was built
// against, never downward; the `go` line is read only, and a language minor
// ABOVE the cap means a human created a mismatch: report, touch nothing.
// Repeated for every module directory.
func (r *run) alignGo(ctx context.Context) error {
	var dirs []string
	var in []string
	if err := json.Unmarshal([]byte(r.o.ModuleDirs), &in); err != nil {
		return fmt.Errorf("module-dirs is not a JSON array of strings: %w", err)
	}
	set := map[string]bool{".": true}
	for _, d := range in {
		set[d] = true
	}
	for d := range set {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs) // jq's `unique`

	latestCache := map[string]string{}
	for _, dir := range dirs {
		gomod := filepath.Join(r.o.WorkDir, dir, "go.mod")
		data, err := os.ReadFile(gomod)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				r.printf("%s: no go.mod — nothing to align\n", dir)
				continue
			}
			return err
		}
		lang, tc := goDirectives(string(data))
		if tc == "" {
			tc = lang
		}

		var built string
		var glci bytes.Buffer
		if err := r.devbox(ctx, dir, &glci, io.Discard, "golangci-lint", "version"); err == nil {
			m := regexp.MustCompile(`built with go([0-9.]*)`).FindStringSubmatch(glci.String())
			if m == nil || m[1] == "" {
				return fmt.Errorf("%s: golangci-lint did not say which Go it was built with", dir)
			}
			built = m[1]
		} else {
			var gv bytes.Buffer
			if err := r.devbox(ctx, dir, &gv, r.stderr(), "go", "env", "GOVERSION"); err != nil {
				return fmt.Errorf("%s: go env GOVERSION: %w", dir, err)
			}
			built = regexp.MustCompile(`[0-9][0-9.]*`).FindString(gv.String())
			if built == "" {
				return fmt.Errorf("%s: could not read the devbox Go version from %q", dir, strings.TrimSpace(gv.String()))
			}
		}
		cap := firstTwo(built)
		r.printf("%s: language %s, toolchain %s; cap: go %s line (golangci built with %s)\n", dir, lang, tc, cap, built)

		if maxVersion(firstTwo(lang), cap) != cap {
			r.printf("::warning::%s: language directive %s is above the golangci-lint cap %s — a human set this; not touching go.mod\n", dir, lang, cap)
			continue
		}

		// One go.dev lookup per cap line, shared across modules.
		latest, cached := latestCache[cap]
		if !cached {
			latest = r.latestGo(ctx, cap)
			latestCache[cap] = latest
		}
		if latest == "" {
			r.printf("%s: go.dev unreachable — toolchain stays %s this run\n", dir, tc)
			continue
		}
		target := strings.TrimPrefix(latest, "go")

		if maxVersion(tc, target) == tc {
			r.printf("%s: toolchain %s already at or above %s — no change\n", dir, tc, target)
			continue
		}
		r.printf("%s: aligning toolchain %s -> %s (language stays %s)\n", dir, tc, target, lang)
		flag := "-toolchain=go" + target
		if target == lang {
			flag = "-toolchain=none"
		}
		if err := r.devbox(ctx, dir, r.stdout(), r.stderr(), "go", "mod", "edit", flag); err != nil {
			return fmt.Errorf("%s: go mod edit: %w", dir, err)
		}
		if err := r.cmd(ctx, ".", r.stdout(), r.stderr(), "git", "diff", "--", dir+"/go.mod"); err != nil {
			return fmt.Errorf("git diff: %w", err)
		}
	}
	return nil
}

// goDirectives is the `go` and `toolchain` lines of a go.mod, the
// toolchain without its "go" prefix.
func goDirectives(mod string) (lang, toolchain string) {
	for _, line := range strings.Split(mod, "\n") {
		f := strings.Fields(strings.TrimRight(line, "\r"))
		if len(f) < 2 {
			continue
		}
		switch {
		case lang == "" && strings.HasPrefix(line, "go ") && f[0] == "go":
			lang = f[1]
		case toolchain == "" && strings.HasPrefix(line, "toolchain ") && f[0] == "toolchain":
			toolchain = strings.TrimPrefix(f[1], "go")
		}
	}
	return
}

// latestGo is the newest release of a Go line (go1.25.4 for "1.25") from
// go.dev, or "" when it cannot be had: the toolchain then stays where it is.
func (r *run) latestGo(ctx context.Context, cap string) string {
	url := r.o.GoDLURL
	if url == "" {
		url = "https://go.dev/dl/?mode=json&include=all"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ""
	}
	var rel []struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&rel) != nil {
		return ""
	}
	prefix := "go" + cap + "."
	for _, v := range rel {
		if strings.HasPrefix(v.Version, prefix) {
			return v.Version
		}
	}
	return ""
}
