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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const secretToken = "tok-read-only-1234"

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

type layer struct {
	mediaType string
	data      []byte
	title     string
}

func tarGz(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := h.Linkname
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

// reg is a read-only-by-test OCI registry that records every request it sees.
type reg struct {
	t         *testing.T
	srv       *httptest.Server
	name      string
	mu        sync.Mutex
	requests  []string
	manifests map[string][]byte
	blobs     map[string][]byte
	auth      string // required Authorization header; empty means none
	realm     bool   // answer 401 with a bearer challenge until the realm token is presented
	realmReqs []string
}

func newReg(t *testing.T) *reg {
	r := &reg{t: t, name: "acme/inputs", manifests: map[string][]byte{}, blobs: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *reg) host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func (r *reg) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.Method+" "+req.URL.RequestURI())
}

func (r *reg) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var w []string
	for _, q := range r.requests {
		if !strings.HasPrefix(q, "GET ") && !strings.HasPrefix(q, "HEAD ") {
			w = append(w, q)
		}
	}
	return w
}

func (r *reg) serve(w http.ResponseWriter, req *http.Request) {
	r.record(req)
	if req.URL.Path == "/token" {
		r.mu.Lock()
		r.realmReqs = append(r.realmReqs, req.URL.RawQuery+" "+req.Header.Get("Authorization"))
		r.mu.Unlock()
		if u, p, ok := req.BasicAuth(); !ok || u != "ci" || p != secretToken {
			http.Error(w, "no", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "bearer-from-realm"})
		return
	}
	want := r.auth
	if r.realm {
		want = "Bearer bearer-from-realm"
	}
	if want != "" && req.Header.Get("Authorization") != want {
		if r.realm {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="repository:%s:pull,push"`, r.srv.URL, r.name))
		}
		http.Error(w, "unauthorized", 401)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "read-only", 405)
		return
	}
	pre := "/v2/" + r.name
	var body []byte
	var ok bool
	switch {
	case strings.HasPrefix(req.URL.Path, pre+"/manifests/"):
		body, ok = r.manifests[strings.TrimPrefix(req.URL.Path, pre+"/manifests/")]
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	case strings.HasPrefix(req.URL.Path, pre+"/blobs/"):
		body, ok = r.blobs[strings.TrimPrefix(req.URL.Path, pre+"/blobs/")]
	}
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

// publish stores an artifact and returns its pinned reference.
func (r *reg) publish(layers ...layer) (ref, digest string) {
	type desc struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	cfg := []byte("{}")
	r.blobs[sum(cfg)] = cfg
	m := struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        desc   `json:"config"`
		Layers        []desc `json:"layers"`
	}{SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json", Config: desc{"application/vnd.oci.empty.v1+json", sum(cfg), int64(len(cfg)), nil}}
	for _, l := range layers {
		d := desc{MediaType: l.mediaType, Digest: sum(l.data), Size: int64(len(l.data))}
		if l.title != "" {
			d.Annotations = map[string]string{"org.opencontainers.image.title": l.title}
		}
		r.blobs[d.Digest] = l.data
		m.Layers = append(m.Layers, d)
	}
	b, _ := json.Marshal(m)
	digest = sum(b)
	r.manifests[digest] = b
	return r.host() + "/" + r.name + "@" + digest, digest
}

const (
	tarGzType = "application/vnd.oci.image.layer.v1.tar+gzip"
	fileType  = "application/vnd.acme.tenant.file.v1"
)

type env struct {
	out, workspace, outDir, ghOutput, summary string
}

func newEnv(t *testing.T) env {
	t.Helper()
	d := t.TempDir()
	e := env{workspace: filepath.Join(d, "workspace"), outDir: filepath.Join(d, "runner-temp", "tenant-inputs"), ghOutput: filepath.Join(d, "github-output"), summary: filepath.Join(d, "summary")}
	if err := os.MkdirAll(e.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func run(t *testing.T, e env, ref string, mod func(*Options)) (string, error) {
	t.Helper()
	var out bytes.Buffer
	o := Options{Reference: ref, Token: secretToken, OutputDir: e.outDir, Workspace: e.workspace, OutputPath: e.ghOutput, SummaryPath: e.summary, HTTPClient: http.DefaultClient, Out: &out}
	if mod != nil {
		mod(&o)
	}
	err := Run(context.Background(), o)
	return out.String(), err
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func wantRefusal(t *testing.T, out string, err error, rule string) {
	t.Helper()
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("want ErrFailed, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "::error::tenant-inputs: "+rule+":") {
		t.Fatalf("want rule %q in:\n%s", rule, out)
	}
}

func goodArtifact(t *testing.T, r *reg) (string, string) {
	return r.publish(
		layer{mediaType: tarGzType, data: tarGz(t, tar.Header{Name: "tenant/", Typeflag: tar.TypeDir, Mode: 0o755}, tar.Header{Name: "tenant/projects.yaml", Typeflag: tar.TypeReg, Linkname: "projects: [acme]\n"})},
		layer{mediaType: fileType, data: []byte("version: 1\n"), title: "provider-names.json"},
	)
}

func TestFetchesTheArtifactByDigestAndWritesIt(t *testing.T) {
	r := newReg(t)
	r.auth = "Bearer " + secretToken
	ref, digest := goodArtifact(t, r)
	e := newEnv(t)
	out, err := run(t, e, ref, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := read(t, filepath.Join(e.outDir, "tenant", "projects.yaml")); got != "projects: [acme]\n" {
		t.Errorf("tar layer content: %q", got)
	}
	if got := read(t, filepath.Join(e.outDir, "provider-names.json")); got != "version: 1\n" {
		t.Errorf("titled layer content: %q", got)
	}
	gh := read(t, e.ghOutput)
	if !strings.Contains(gh, "digest="+digest+"\n") || !strings.Contains(gh, "path="+e.outDir+"\n") {
		t.Errorf("outputs: %q", gh)
	}
	if !strings.Contains(read(t, e.summary), digest) {
		t.Errorf("the summary should name the digest")
	}
	if w := r.writes(); len(w) != 0 {
		t.Errorf("registry saw writes: %v", w)
	}
}

func TestTheTokenIsMaskedBeforeAnythingElseAndNeverPrinted(t *testing.T) {
	r := newReg(t)
	ref, _ := goodArtifact(t, r)
	r.manifests[strings.SplitN(ref, "@", 2)[1]] = nil // force a failure path as well
	e := newEnv(t)
	out, _ := run(t, e, ref, nil)
	if !strings.HasPrefix(out, "::add-mask::"+secretToken+"\n") {
		t.Fatalf("the first line must mask the token:\n%s", out)
	}
	if strings.Contains(strings.TrimPrefix(out, "::add-mask::"+secretToken+"\n"), secretToken) {
		t.Fatalf("the token is printed after its mask:\n%s", out)
	}
}

func TestAReferenceThatIsNotADigestPinIsRefusedBeforeTheNetwork(t *testing.T) {
	r := newReg(t)
	_, digest := goodArtifact(t, r)
	hex := strings.TrimPrefix(digest, "sha256:")
	for name, ref := range map[string]string{
		"tag only":              r.host() + "/acme/inputs:v1",
		"no tag and no digest":  r.host() + "/acme/inputs",
		"tag and digest":        r.host() + "/acme/inputs:v1@" + digest,
		"short digest":          r.host() + "/acme/inputs@sha256:abc123",
		"upper-case digest":     r.host() + "/acme/inputs@sha256:" + strings.ToUpper(hex),
		"another algorithm":     r.host() + "/acme/inputs@sha512:" + hex + hex,
		"no registry host":      "acme/inputs@" + digest,
		"scheme":                "https://" + r.host() + "/acme/inputs@" + digest,
		"empty":                 "",
		"path traversal":        r.host() + "/acme/../inputs@" + digest,
		"digest with a newline": r.host() + "/acme/inputs@" + digest + "\n::error::x",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			out, err := run(t, e, ref, nil)
			wantRefusal(t, out, err, "reference-not-pinned")
			r.mu.Lock()
			n := len(r.requests)
			r.mu.Unlock()
			if n != 0 {
				t.Fatalf("a refused reference must not reach the registry, saw %v", r.requests)
			}
			if _, err := os.Stat(e.outDir); err == nil {
				t.Fatalf("nothing may be written for a refused reference")
			}
		})
	}
}

func TestADigestMismatchIsRefusedAndWritesNothing(t *testing.T) {
	r := newReg(t)
	ref, digest := goodArtifact(t, r)
	// The registry answers the requested digest with a different manifest.
	other, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "layers": []any{}})
	r.manifests[digest] = other
	e := newEnv(t)
	out, err := run(t, e, ref, nil)
	wantRefusal(t, out, err, "digest-mismatch")
	if _, err := os.Stat(e.outDir); err == nil {
		t.Fatalf("a mismatched artifact must leave no output directory")
	}
	if gh, _ := os.ReadFile(e.ghOutput); strings.Contains(string(gh), "digest=") {
		t.Fatalf("a refusal must not set outputs: %q", gh)
	}
}

func TestALayerThatDoesNotMatchItsDescriptorIsRefused(t *testing.T) {
	r := newReg(t)
	ref, _ := r.publish(layer{mediaType: fileType, data: []byte("a: 1\n"), title: "a.yaml"})
	for d := range r.blobs {
		if r.blobs[d] != nil && string(r.blobs[d]) == "a: 1\n" {
			r.blobs[d] = []byte("a: 2\n")
		}
	}
	e := newEnv(t)
	out, err := run(t, e, ref, nil)
	wantRefusal(t, out, err, "digest-mismatch")
	if _, err := os.Stat(filepath.Join(e.outDir, "a.yaml")); err == nil {
		t.Fatalf("an unverified layer must not be written")
	}
}

func TestUnsafeOrUnsupportedContentIsRefused(t *testing.T) {
	r := newReg(t)
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	r.manifests[sum(index)] = index
	cases := map[string]struct {
		ref  string
		rule string
	}{}
	add := func(name, rule string, layers ...layer) {
		ref, _ := r.publish(layers...)
		cases[name] = struct{ ref, rule string }{ref, rule}
	}
	cases["an image index"] = struct{ ref, rule string }{r.host() + "/acme/inputs@" + sum(index), "unsupported-manifest"}
	add("tar path traversal", "unsafe-layer", layer{mediaType: tarGzType, data: tarGz(t, tar.Header{Name: "../escape.yaml", Typeflag: tar.TypeReg, Linkname: "x"})})
	add("tar absolute path", "unsafe-layer", layer{mediaType: tarGzType, data: tarGz(t, tar.Header{Name: "/etc/x.yaml", Typeflag: tar.TypeReg, Linkname: "x"})})
	add("tar symlink", "unsafe-layer", layer{mediaType: tarGzType, data: tarGz(t, tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})})
	add("tar hard link", "unsafe-layer", layer{mediaType: tarGzType, data: tarGz(t, tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "a"})})
	add("title that is a path", "unsafe-layer", layer{mediaType: fileType, data: []byte("x"), title: "../x"})
	add("two layers, one title", "unsafe-layer", layer{mediaType: fileType, data: []byte("x"), title: "a"}, layer{mediaType: fileType, data: []byte("y"), title: "a"})
	add("a layer without a title or a tar type", "unsupported-manifest", layer{mediaType: fileType, data: []byte("x")})
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			out, err := run(t, e, c.ref, nil)
			wantRefusal(t, out, err, c.rule)
			if _, err := os.Stat(e.outDir); err == nil {
				entries, _ := os.ReadDir(e.outDir)
				if len(entries) != 0 {
					t.Fatalf("a refused artifact must leave nothing behind, found %v", entries)
				}
			}
		})
	}
}

func TestTooLargeIsRefused(t *testing.T) {
	r := newReg(t)
	ref, _ := r.publish(layer{mediaType: fileType, data: bytes.Repeat([]byte("x"), 4096), title: "big"})
	e := newEnv(t)
	out, err := run(t, e, ref, func(o *Options) { o.MaxBytes = 1024 })
	wantRefusal(t, out, err, "too-large")
}

func TestARegistryRefusalIsARefusal(t *testing.T) {
	r := newReg(t)
	ref, _ := goodArtifact(t, r)
	r.auth = "Bearer something-else"
	e := newEnv(t)
	out, err := run(t, e, ref, nil)
	wantRefusal(t, out, err, "registry-refused")
	r2 := newReg(t)
	_, _ = goodArtifact(t, r2)
	missing := r2.host() + "/acme/inputs@sha256:" + strings.Repeat("0", 64)
	out, err = run(t, newEnv(t), missing, nil)
	wantRefusal(t, out, err, "registry-refused")
}

func TestARegistryThatChallengesIsAnsweredWithAPullOnlyToken(t *testing.T) {
	r := newReg(t)
	r.realm = true
	ref, _ := goodArtifact(t, r)
	e := newEnv(t)
	out, err := run(t, e, ref, func(o *Options) { o.Username = "ci" })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(r.realmReqs) != 1 {
		t.Fatalf("want one realm request, got %v", r.realmReqs)
	}
	q := r.realmReqs[0]
	if !strings.Contains(q, "scope=repository%3Aacme%2Finputs%3Apull") || strings.Contains(q, "push") {
		t.Errorf("the realm must be asked for pull alone, whatever the challenge said: %s", q)
	}
	if w := r.writes(); len(w) != 0 {
		t.Errorf("registry saw writes: %v", w)
	}
}

func TestNeverAWriteToTheRegistry(t *testing.T) {
	r := newReg(t)
	r.auth = "Bearer " + secretToken
	ref, _ := goodArtifact(t, r)
	_, _ = run(t, newEnv(t), ref, nil)
	_, _ = run(t, newEnv(t), r.host()+"/acme/inputs:latest", nil)
	if w := r.writes(); len(w) != 0 {
		t.Fatalf("zero write requests expected, got %v", w)
	}
	// The client itself refuses a write, whatever calls it.
	var got Options
	got.HTTPClient = http.DefaultClient
	c := readOnlyClient(got.HTTPClient)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req, _ := http.NewRequest(m, r.srv.URL+"/v2/"+r.name+"/blobs/uploads/", strings.NewReader("x"))
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("%s was sent", m)
		}
	}
	if w := r.writes(); len(w) != 0 {
		t.Fatalf("the guard let a write through: %v", w)
	}
}

func TestACommittedCopyIsNeverRead(t *testing.T) {
	r := newReg(t)
	ref, digest := goodArtifact(t, r)
	e := newEnv(t)
	// A copy committed in the company repository, tampered, in every place
	// it might be looked for.
	for _, p := range []string{"tenant/projects.yaml", "tenant-inputs/tenant/projects.yaml", "provider-names.json", "inputs.lock"} {
		full := filepath.Join(e.workspace, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("projects: [evil]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := run(t, e, ref, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := read(t, filepath.Join(e.outDir, "tenant", "projects.yaml")); got != "projects: [acme]\n" {
		t.Errorf("the result followed the committed copy: %q", got)
	}
	if !strings.Contains(read(t, e.ghOutput), "digest="+digest) {
		t.Errorf("the digest followed the committed copy")
	}
}

func TestTheOutputDirectoryMayNotBeInsideTheWorkspace(t *testing.T) {
	r := newReg(t)
	ref, _ := goodArtifact(t, r)
	e := newEnv(t)
	out, err := run(t, e, ref, func(o *Options) { o.OutputDir = filepath.Join(e.workspace, "tenant-inputs") })
	wantRefusal(t, out, err, "output-in-workspace")
}

func TestAnExistingOutputDirectoryIsNotReused(t *testing.T) {
	r := newReg(t)
	ref, _ := goodArtifact(t, r)
	e := newEnv(t)
	if err := os.MkdirAll(e.outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.outDir, "stale.yaml"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, e, ref, nil)
	wantRefusal(t, out, err, "output-not-empty")
	if got := read(t, filepath.Join(e.outDir, "stale.yaml")); got != "old" {
		t.Errorf("the refusal must not touch what is there: %q", got)
	}
}

// bigTarGz is a gzipped tar of one regular file of n zero bytes.
func bigTarGz(t *testing.T, name string, n int) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(n)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

// Many small, highly compressible layers: the compressed sum is far under the
// limit while the decompressed sum is far over it. The limit is one running
// total of what is held, not one per layer.
func TestDecompressedBytesAreSummedAcrossLayers(t *testing.T) {
	r := newReg(t)
	var layers []layer
	for i := 0; i < 20; i++ {
		layers = append(layers, layer{mediaType: tarGzType, data: bigTarGz(t, fmt.Sprintf("f%d.yaml", i), 1<<20)})
	}
	ref, _ := r.publish(layers...)
	e := newEnv(t)
	out, err := run(t, e, ref, func(o *Options) { o.MaxBytes = 4 << 20 })
	wantRefusal(t, out, err, "too-large")
	if _, err := os.Stat(e.outDir); err == nil {
		t.Errorf("a refusal must write nothing")
	}
}

func TestTheNumberOfLayersIsCapped(t *testing.T) {
	r := newReg(t)
	var layers []layer
	for i := 0; i <= maxLayers; i++ {
		layers = append(layers, layer{mediaType: fileType, data: []byte("x"), title: fmt.Sprintf("f%d", i)})
	}
	ref, _ := r.publish(layers...)
	out, err := run(t, newEnv(t), ref, nil)
	wantRefusal(t, out, err, "too-large")
}

func TestTheNumberOfTarEntriesIsCapped(t *testing.T) {
	r := newReg(t)
	var hs []tar.Header
	for i := 0; i <= maxEntries; i++ {
		hs = append(hs, tar.Header{Name: fmt.Sprintf("d%d/", i), Typeflag: tar.TypeDir, Mode: 0o755})
	}
	ref, _ := r.publish(layer{mediaType: tarGzType, data: tarGz(t, hs...)})
	out, err := run(t, newEnv(t), ref, nil)
	wantRefusal(t, out, err, "too-large")
}

// The guard compares where the filesystem puts the paths, not how they are
// spelled.
func TestASymlinkedOutputDirectoryIntoTheWorkspaceIsRefused(t *testing.T) {
	r := newReg(t)
	ref, _ := goodArtifact(t, r)
	e := newEnv(t)
	link := filepath.Join(filepath.Dir(filepath.Dir(e.outDir)), "link")
	if err := os.Symlink(e.workspace, link); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, e, ref, func(o *Options) { o.OutputDir = filepath.Join(link, "tenant-inputs") })
	wantRefusal(t, out, err, "output-in-workspace")
	if _, err := os.Stat(filepath.Join(e.workspace, "tenant-inputs")); err == nil {
		t.Errorf("a refusal must write nothing into the workspace")
	}
}

func TestASymlinkedWorkspaceIsResolvedToo(t *testing.T) {
	r := newReg(t)
	ref, _ := goodArtifact(t, r)
	e := newEnv(t)
	link := filepath.Join(filepath.Dir(filepath.Dir(e.outDir)), "wslink")
	if err := os.Symlink(e.workspace, link); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, e, ref, func(o *Options) {
		o.Workspace = link
		o.OutputDir = filepath.Join(e.workspace, "tenant-inputs")
	})
	wantRefusal(t, out, err, "output-in-workspace")
}

func TestUnderActionsAnEmptyWorkspaceIsAUsageError(t *testing.T) {
	r := newReg(t)
	ref, _ := goodArtifact(t, r)
	e := newEnv(t)
	out, err := run(t, e, ref, func(o *Options) { o.Workspace = ""; o.RequireWorkspace = true })
	if err == nil || errors.Is(err, ErrFailed) {
		t.Fatalf("want a usage error, got %v\n%s", err, out)
	}
	if n := len(r.requests); n != 0 {
		t.Errorf("no request may be made before the guard, got %d", n)
	}
	// Outside Actions an empty workspace is still allowed.
	if out, err := run(t, newEnv(t), ref, func(o *Options) { o.Workspace = "" }); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
