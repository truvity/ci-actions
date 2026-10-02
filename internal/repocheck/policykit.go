package repocheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// PolicyKitOptions configures the check.
type PolicyKitOptions struct {
	Root    string // the repository root
	BaseURL string // empty is https://raw.githubusercontent.com
	HTTP    *http.Client
	Out     io.Writer
}

// PolicyKit: is caller-parity/kits/golangci-depguard.yaml still the file it
// claims to be a copy of?
//
// golangci-lint v2 cannot extend a configuration from a URL, so the import bans
// are a hand copy: in every consuming repository, and in this action's own
// canonical copy for caller-parity, one level up. A hand copy with nothing
// keeping it in step has the half-life the parity check exists to close
// everywhere else; this closes it here, against the source, rather than leaving
// "does this still match policy" to whoever next compares the two by eye.
//
// The pin lives beside the kit, in kits.yaml's `source:` field
// (`owner/repo@tag`), not a constant here, so bumping the pin is the one-line
// edit kit changes already are. No token: truvity/policy is public, and this
// reads the tagged blob over HTTPS the same way tagged-pins reads tags over git.
func PolicyKit(ctx context.Context, o PolicyKitOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://raw.githubusercontent.com"
	}
	kit := filepath.Join(o.Root, "caller-parity", "kits", "golangci-depguard.yaml")
	manifest := filepath.Join(o.Root, "caller-parity", "kits", "kits.yaml")

	var m map[string]struct {
		Source string `yaml:"source"`
	}
	if b, err := os.ReadFile(manifest); err != nil {
		return err
	} else if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}
	source := m["golangci-depguard.yaml"].Source
	if source == "" {
		fmt.Fprintln(o.Out, "::error::kits.yaml has no source: for golangci-depguard.yaml")
		return ErrFailed
	}
	repo, tag := source, source
	if i := strings.LastIndex(source, "@"); i >= 0 {
		repo = source[:i]
	}
	tag = source[strings.LastIndex(source, "@")+1:]
	if repo == "" || tag == "" || repo == tag {
		fmt.Fprintf(o.Out, "::error::kits.yaml's source (%s) is not owner/repo@tag\n", source)
		return ErrFailed
	}

	url := o.BaseURL + "/" + repo + "/" + tag + "/lint/golangci-depguard.yaml"
	upstream, err := get(ctx, o.HTTP, url)
	if err != nil {
		fmt.Fprintf(o.Out, "::error::could not read %s — is the tag right, and is lint/golangci-depguard.yaml still there?\n", url)
		return ErrFailed
	}
	ours, err := os.ReadFile(kit)
	if err != nil {
		return err
	}
	if bytes.Equal(ours, upstream) {
		fmt.Fprintf(o.Out, "policy-kit-current: caller-parity/kits/golangci-depguard.yaml matches %s verbatim\n", source)
		return nil
	}
	fmt.Fprintf(o.Out, "::error::caller-parity/kits/golangci-depguard.yaml has drifted from %s — update the kit, or bump the pin if %s moved on purpose\n", source, repo)
	tmp, err := os.MkdirTemp("", "policy-kit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	up := filepath.Join(tmp, "upstream")
	if err := os.WriteFile(up, upstream, 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "diff", "-u", "--label", source+":lint/golangci-depguard.yaml", "--label", "caller-parity/kits/golangci-depguard.yaml", up, kit)
	cmd.Stdout = o.Out
	var ee *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &ee) {
		return err
	}
	return ErrFailed
}

func get(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
