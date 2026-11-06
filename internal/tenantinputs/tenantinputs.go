// Package tenantinputs fetches the digest-pinned artifact in which a
// platform publishes a company's non-secret inputs, so that a company's own
// CI validates a candidate against what the platform will actually use.
//
// The reference is the identity: `<registry>/<name>@sha256:<hex>`, and
// nothing else. A tag, a tag together with a digest, a reference without a
// registry host: all refused before any request is made. Every byte that is
// written was checked against a digest: the manifest against the reference,
// each layer against the manifest. A registry that answers a digest with
// other bytes is a refusal that writes nothing.
//
// It only reads. The HTTP client refuses every method but GET and HEAD, so
// no code path here, now or later, can write to a registry, and the token
// it is given is only ever a pull credential. It reads nothing from the
// company repository: the result is written to a directory outside the
// workspace, so a copy committed in the repository can neither stand in for
// the artifact nor change the answer.
//
// A refusal is `::error::tenant-inputs: <rule>: <static sentence>`; nothing
// the registry or the artifact says is echoed. The token is masked before
// anything else is printed.
package tenantinputs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrFailed marks a refusal whose ::error:: line has already been written.
var ErrFailed = errors.New("tenant-inputs: failed")

// The rules. A rule is a fixed token, safe to print.
const (
	RuleNotPinned         = "reference-not-pinned"
	RuleDigestMismatch    = "digest-mismatch"
	RuleUnsupported       = "unsupported-manifest"
	RuleUnsafeLayer       = "unsafe-layer"
	RuleTooLarge          = "too-large"
	RuleRegistryRefused   = "registry-refused"
	RuleOutputInWorkspace = "output-in-workspace"
	RuleOutputNotEmpty    = "output-not-empty"
)

var sentences = map[string]string{
	RuleNotPinned:         "the reference must be <registry>/<name>@sha256:<64 lower-case hex>",
	RuleDigestMismatch:    "the registry returned bytes that are not the digest asked for",
	RuleUnsupported:       "the artifact is not an image manifest of titled files or tar layers",
	RuleUnsafeLayer:       "a layer would write outside the output directory or is not a plain file",
	RuleTooLarge:          "the artifact exceeds the size limit",
	RuleRegistryRefused:   "the registry did not serve the artifact",
	RuleOutputInWorkspace: "the output directory must be outside the workspace",
	RuleOutputNotEmpty:    "the output directory must not exist or must be empty",
}

const (
	defaultMaxBytes = 64 << 20
	maxManifest     = 1 << 20
	// maxLayers and maxEntries bound what is held in memory besides bytes:
	// a compressed layer of tar headers costs almost nothing to fetch and a
	// great deal to hold.
	maxLayers  = 128
	maxEntries = 10000
	titleKey   = "org.opencontainers.image.title"
)

// Options are the action's inputs and the runner's files.
type Options struct {
	Reference string // <registry>/<name>@sha256:<hex>
	Token     string // a pull credential; optional for a public registry
	Username  string // with a Token: Basic auth (e.g. a registry that wants a user); empty: Bearer
	OutputDir string // where the verified files are written; must be outside Workspace
	Workspace string // GITHUB_WORKSPACE: the company checkout, never read
	// RequireWorkspace makes an empty Workspace a usage error: under
	// GitHub Actions the guard against a committed copy must not rest on
	// the caller having set it.
	RequireWorkspace bool
	// OutputPath is GITHUB_OUTPUT and SummaryPath GITHUB_STEP_SUMMARY.
	OutputPath  string
	SummaryPath string
	MaxBytes    int64        // total size limit; 0 means 64 MiB
	HTTPClient  *http.Client // nil means a default client; either way it is made read-only
	Out         io.Writer
}

var (
	refRe    = regexp.MustCompile(`^([A-Za-z0-9.-]+(?::[0-9]{1,5})?|\[[0-9a-fA-F:]+\](?::[0-9]{1,5})?)/((?:[a-z0-9]+(?:[._-]+[a-z0-9]+)*)(?:/[a-z0-9]+(?:[._-]+[a-z0-9]+)*)*)@(sha256:[0-9a-f]{64})$`)
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	titleRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type fail struct{ rule string }

func (f fail) Error() string { return f.rule }

func refuse(rule string) error { return fail{rule} }

// readOnlyTransport is the guarantee that nothing here writes to a registry.
type readOnlyTransport struct{ next http.RoundTripper }

func (t readOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return nil, fmt.Errorf("tenant-inputs: %s refused: the client is read-only", r.Method)
	}
	return t.next.RoundTrip(r)
}

// readOnlyClient wraps a client so that only GET and HEAD can be sent.
func readOnlyClient(c *http.Client) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	cp := *c
	cp.Transport = readOnlyTransport{next}
	return &cp
}

// Run fetches and verifies the artifact. It returns ErrFailed when it
// refused (the ::error:: line is written) and another error when it could
// not read or write at all.
func Run(ctx context.Context, opts Options) error {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	for _, line := range strings.Split(opts.Token, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			fmt.Fprintf(out, "::add-mask::%s\n", line)
		}
	}
	err := fetch(ctx, opts, out)
	var f fail
	if errors.As(err, &f) {
		fmt.Fprintf(out, "::error::tenant-inputs: %s: %s\n", f.rule, sentences[f.rule])
		return ErrFailed
	}
	return err
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Layers        []descriptor `json:"layers"`
}

func fetch(ctx context.Context, opts Options, out io.Writer) error {
	m := refRe.FindStringSubmatch(opts.Reference)
	if m == nil {
		return refuse(RuleNotPinned)
	}
	host, name, digest := m[1], m[2], m[3]
	// The first component is a registry only if it says so (as container
	// clients decide): a dot, a port or localhost. There is no default registry.
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return refuse(RuleNotPinned)
	}
	max := opts.MaxBytes
	if max <= 0 {
		max = defaultMaxBytes
	}
	if opts.OutputDir == "" {
		return errors.New("tenant-inputs: no output directory")
	}
	outDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return err
	}
	if opts.Workspace == "" && opts.RequireWorkspace {
		return errors.New("tenant-inputs: the workspace is not set (GITHUB_WORKSPACE), so the output directory cannot be checked")
	}
	if opts.Workspace != "" {
		ws, err := filepath.Abs(opts.Workspace)
		if err != nil {
			return err
		}
		// Compared by where the filesystem puts them, not by how they are
		// spelled: a link that leads into the workspace is inside it.
		ws, err = resolveExisting(ws)
		if err != nil {
			return err
		}
		real, err := resolveExisting(outDir)
		if err != nil {
			return err
		}
		if r, err := filepath.Rel(ws, real); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
			return refuse(RuleOutputInWorkspace)
		}
	}
	if entries, err := os.ReadDir(outDir); err == nil && len(entries) > 0 {
		return refuse(RuleOutputNotEmpty)
	}

	c := &client{
		http: readOnlyClient(opts.HTTPClient), base: scheme(host) + "://" + host, name: name,
		token: opts.Token, user: opts.Username, max: max,
	}
	mb, err := c.get(ctx, "/v2/"+name+"/manifests/"+digest, maxManifest, "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	if err != nil {
		return err
	}
	if digestOf(mb) != digest {
		return refuse(RuleDigestMismatch)
	}
	var man manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		return refuse(RuleUnsupported)
	}
	if man.SchemaVersion != 2 || (man.MediaType != "application/vnd.oci.image.manifest.v1+json" && man.MediaType != "application/vnd.docker.distribution.manifest.v2+json") {
		return refuse(RuleUnsupported)
	}

	// Everything is fetched and verified in memory before the first byte
	// reaches the disk: a refusal leaves no output directory behind.
	type file struct {
		name string
		data []byte
	}
	var files []file
	var total int64 // the sizes the manifest declares (what is fetched)
	var held int64  // the bytes held for writing, after any decompression
	entries := 0
	titles := map[string]bool{}
	if len(man.Layers) > maxLayers {
		return refuse(RuleTooLarge)
	}
	for _, l := range man.Layers {
		if !digestRe.MatchString(l.Digest) || l.Size < 0 {
			return refuse(RuleUnsupported)
		}
		total += l.Size
		if total > max {
			return refuse(RuleTooLarge)
		}
		tarLayer := strings.HasSuffix(l.MediaType, "tar") || strings.HasSuffix(l.MediaType, "tar+gzip")
		title := l.Annotations[titleKey]
		if !tarLayer && title == "" {
			return refuse(RuleUnsupported)
		}
		if title != "" && !tarLayer {
			if !titleRe.MatchString(title) || titles[title] {
				return refuse(RuleUnsafeLayer)
			}
			titles[title] = true
		}
		b, err := c.get(ctx, "/v2/"+name+"/blobs/"+l.Digest, l.Size, "")
		if err != nil {
			return err
		}
		if digestOf(b) != l.Digest || int64(len(b)) != l.Size {
			return refuse(RuleDigestMismatch)
		}
		if !tarLayer {
			held += int64(len(b))
			entries++
			if held > max || entries > maxEntries {
				return refuse(RuleTooLarge)
			}
			files = append(files, file{title, b})
			continue
		}
		ex, err := untar(b, strings.HasSuffix(l.MediaType, "gzip"), max-held, &entries)
		if err != nil {
			return err
		}
		for _, f := range ex {
			held += int64(len(f.data))
			if titles[f.name] {
				return refuse(RuleUnsafeLayer)
			}
			titles[f.name] = true
			files = append(files, file{f.name, f.data})
		}
	}

	for _, f := range files {
		if !strings.HasPrefix(filepath.Join(outDir, filepath.FromSlash(f.name)), outDir+string(os.PathSeparator)) {
			return refuse(RuleUnsafeLayer)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(outDir, filepath.FromSlash(f.name))
		if !strings.HasPrefix(p, outDir+string(os.PathSeparator)) {
			return refuse(RuleUnsafeLayer)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "tenant-inputs: %s@%s: %d files, written to %s\n", name, digest, len(files), outDir)
	if opts.OutputPath != "" {
		if err := appendFile(opts.OutputPath, fmt.Sprintf("path=%s\ndigest=%s\n", outDir, digest)); err != nil {
			return err
		}
	}
	if opts.SummaryPath != "" {
		if err := appendFile(opts.SummaryPath, fmt.Sprintf("tenant inputs: `%s@%s`, %d files, read-only.\n", name, digest, len(files))); err != nil {
			return err
		}
	}
	return nil
}

// resolveExisting resolves the symlinks of the deepest part of p that
// exists and appends the rest, so a path that does not exist yet (the
// output directory) is still judged by where it would land.
func resolveExisting(p string) (string, error) {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func appendFile(p, s string) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// scheme is https, except for a loopback registry (a test double, a local
// registry), as every container client does.
func scheme(host string) string {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return "http"
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return "http"
	}
	return "https"
}

type extracted struct {
	name string
	data []byte
}

// untar reads a tar layer in memory. Only regular files and directories are
// accepted, under relative names that stay inside the layer.
//
// budget is what the layer may add to the bytes already held (so the limit
// is one running total across layers, not one per layer), and entries counts
// every header of every layer against maxEntries.
func untar(b []byte, gz bool, budget int64, entries *int) ([]extracted, error) {
	var r io.Reader = bytes.NewReader(b)
	if gz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, refuse(RuleUnsupported)
		}
		defer zr.Close()
		r = zr
	}
	tr := tar.NewReader(r)
	var out []extracted
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, refuse(RuleUnsupported)
		}
		if *entries++; *entries > maxEntries {
			return nil, refuse(RuleTooLarge)
		}
		// No entry name may climb, however it is spelled (checked again at the write).
		if strings.Contains(h.Name, "..") {
			return nil, refuse(RuleUnsafeLayer)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if !safeName(strings.TrimSuffix(h.Name, "/")) {
				return nil, refuse(RuleUnsafeLayer)
			}
			continue
		case tar.TypeReg:
		default:
			return nil, refuse(RuleUnsafeLayer)
		}
		if !safeName(h.Name) {
			return nil, refuse(RuleUnsafeLayer)
		}
		total += h.Size
		if h.Size < 0 || total > budget {
			return nil, refuse(RuleTooLarge)
		}
		data, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil || int64(len(data)) != h.Size {
			return nil, refuse(RuleUnsupported)
		}
		out = append(out, extracted{path.Clean(h.Name), data})
	}
}

// safeName: relative, clean, no dot segments, no control characters.
func safeName(n string) bool {
	if n == "" || strings.HasPrefix(n, "/") || strings.ContainsAny(n, "\\\x00") || path.Clean(n) != n {
		return false
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// client is a pull-only registry client.
type client struct {
	http  *http.Client
	base  string
	name  string
	token string
	user  string
	max   int64

	bearer string // a token obtained from the registry's realm
}

func (c *client) authorize(req *http.Request) {
	switch {
	case c.bearer != "":
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	case c.token != "" && c.user != "":
		req.SetBasicAuth(c.user, c.token)
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// get reads one resource, at most limit bytes (plus one, to notice more).
func (c *client) get(ctx context.Context, p string, limit int64, accept string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+p, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		c.authorize(req)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, refuse(RuleRegistryRefused)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if ok, err := c.exchange(ctx, challenge); err != nil || !ok {
				return nil, refuse(RuleRegistryRefused)
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, refuse(RuleRegistryRefused)
		}
		if limit < 0 || limit > c.max {
			limit = c.max
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, refuse(RuleRegistryRefused)
		}
		if int64(len(b)) > limit {
			return nil, refuse(RuleTooLarge)
		}
		return b, nil
	}
	return nil, refuse(RuleRegistryRefused)
}

var paramRe = regexp.MustCompile(`([a-z]+)="([^"]*)"`)

// exchange answers a Bearer challenge: the realm is asked for a token for
// exactly `pull` on this repository, whatever scope the challenge named.
func (c *client) exchange(ctx context.Context, challenge string) (bool, error) {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return false, nil
	}
	params := map[string]string{}
	for _, m := range paramRe.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && scheme(realm.Host) != "http") {
		return false, nil
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+c.name+":pull")
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return false, err
	}
	if c.token != "" && c.user != "" {
		req.SetBasicAuth(c.user, c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return false, nil
	}
	c.bearer = body.Token
	if c.bearer == "" {
		c.bearer = body.AccessToken
	}
	return c.bearer != "", nil
}
