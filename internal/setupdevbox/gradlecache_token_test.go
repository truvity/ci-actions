package setupdevbox

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "k3yMaterialOfTheRunner0123456789abcdefABCDEF"

// The token is masked, never exported and never printed beyond the mask, and
// the step says it is sent to the runner's own https cache, by origin only.
func TestGradleCacheTokenIsMaskedAndNeverExported(t *testing.T) {
	s := cacheServer(t, true, "/")
	x := newH(t)
	writeCA(t, x, s.Certificate())
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = s.URL + "/"
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN"] = testToken + "\n" // a task definition's trailing newline
	if err := runGradleCache(x, s.Client()); err != nil {
		t.Fatal(err)
	}
	log := x.out.String()
	if !strings.HasPrefix(firstLineWith(log, testToken), "::add-mask::"+testToken) || strings.Count(log, testToken) != 1 {
		t.Errorf("the token appears other than in the one mask command:\n%s", log)
	}
	if !strings.Contains(log, "build cache writes are authenticated with the runner's credential (CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN), sent to "+s.URL+" only") {
		t.Errorf("log: %s", log)
	}
	for _, f := range []string{"github_env", "github_output", "summary"} {
		if strings.Contains(x.file(f), testToken) {
			t.Errorf("%s holds the token", f)
		}
	}
	walkNoToken(t, x.env["HOME"])
}

// A caller's cache never gets the runner's credential; a plain http one does
// not either, with a warning; no build cache in use means nothing is sent.
func TestGradleCacheTokenIsWithheld(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(x *h, url string)
		tls  bool
		want string
	}{
		"a caller's cache": {
			set:  func(x *h, url string) { x.env["CALLER_GRADLE_BUILD_CACHE"] = url },
			tls:  true,
			want: "the build cache in use is not the runner's own; the runner's write credential (CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN) is not sent to it",
		},
		"plain http": {
			set:  func(x *h, url string) { x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = url },
			want: "::warning::gradle-cache: the runner's build cache is plain http; its write credential (CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN) is not sent",
		},
		"no build cache": {
			set:  func(x *h, url string) { x.env["CI_RUNNER_MAVEN_REGISTRY"] = url },
			tls:  true,
			want: "no build cache is in use; it is not sent",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := cacheServer(t, tc.tls, "/")
			x := newH(t)
			if tc.tls {
				writeCA(t, x, s.Certificate())
			}
			tc.set(x, s.URL)
			x.env["CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN"] = testToken
			if err := runGradleCache(x, s.Client()); err != nil {
				t.Fatal(err)
			}
			log := x.out.String()
			if !strings.Contains(log, tc.want) || strings.Count(log, testToken) != 1 {
				t.Errorf("log: %s", log)
			}
		})
	}
}

func TestGradleCacheTokenWithWhitespaceFailsTheStep(t *testing.T) {
	s := cacheServer(t, true, "/")
	x := newH(t)
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = s.URL
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN"] = "two words"
	if err := runGradleCache(x, s.Client()); err == nil {
		t.Fatal("accepted a token with whitespace")
	}
	if log := x.out.String(); strings.Contains(log, "two words") || !strings.Contains(log, "::error::CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN holds whitespace") {
		t.Errorf("log: %s", log)
	}
}

func TestGradleCacheWithoutATokenMasksNothing(t *testing.T) {
	s := cacheServer(t, true, "/")
	x := newH(t)
	writeCA(t, x, s.Certificate())
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = s.URL
	if err := runGradleCache(x, s.Client()); err != nil {
		t.Fatal(err)
	}
	if log := x.out.String(); strings.Contains(log, "::add-mask::") || strings.Contains(log, "credential") {
		t.Errorf("log: %s", log)
	}
}

// TestGradleSendsTheTokenToTheCacheOnly runs a real Gradle against a cache
// that answers a hit the way cache-redirect does, a 302 to an object store on
// ANOTHER host, and proves end to end:
//
//   - the store (PUT) carries the token as the Basic password, and a cache
//     that refuses anonymous writes accepts it;
//   - the redirect of a hit is followed WITHOUT the credential: the object
//     store's host sees no Authorization header, and the task comes back
//     FROM-CACHE;
//   - Gradle's output names the cache by origin and never holds the token.
//
// It needs a Gradle and a JDK, so it runs only where GRADLE names the gradle
// launcher (or one is on PATH) and skips elsewhere; it also needs 127.0.0.2
// (the loopback /8, as on Linux) to put the object store on another host.
func TestGradleSendsTheTokenToTheCacheOnly(t *testing.T) {
	gradle := os.Getenv("GRADLE")
	if gradle == "" {
		gradle, _ = exec.LookPath("gradle")
	}
	if gradle == "" {
		t.Skip("no Gradle: set GRADLE to a gradle launcher to run the end-to-end check")
	}

	caPEM, serverCert := testPKI(t, "127.0.0.1", "127.0.0.2")

	store := &recorder{}
	objects := map[string][]byte{}
	var mu sync.Mutex

	s3 := listenTLS(t, "127.0.0.2:0", serverCert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.add(r)
		mu.Lock()
		b, ok := objects[strings.TrimPrefix(r.URL.Path, "/obj/")]
		mu.Unlock()
		if !ok || r.URL.Query().Get("X-Amz-Signature") == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))

	cache := &recorder{}
	front := listenTLS(t, "127.0.0.1:0", serverCert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cache.add(r)
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case r.Method == http.MethodGet && key == "":
			_, _ = w.Write([]byte("ok\n"))
		case r.Method == http.MethodPut:
			if _, pw, ok := r.BasicAuth(); !ok || pw != testToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="cache"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[key] = b
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet:
			mu.Lock()
			_, ok := objects[key]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			http.Redirect(w, r, "https://"+s3+"/obj/"+key+"?X-Amz-Signature=presigned", http.StatusFound)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	// The step itself installs the init script and the trust store.
	x := newH(t)
	if err := os.WriteFile(filepath.Join(x.dir, "ca-bundle.pem"), caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	x.env["SSL_CERT_FILE"] = filepath.Join(x.dir, "ca-bundle.pem")
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = "https://" + front
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN"] = testToken
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	if err := runGradleCache(x, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}); err != nil {
		t.Fatalf("%v\n%s", err, x.out.String())
	}

	project := filepath.Join(x.dir, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// No local cache, so the second build can only be served by the remote one.
	writeFile(t, filepath.Join(project, "settings.gradle"), "rootProject.name = 'probe'\nbuildCache { local { enabled = false } }\n")
	writeFile(t, filepath.Join(project, "build.gradle"), `
@CacheableTask
abstract class Gen extends DefaultTask {
    @Input abstract Property<String> getText()
    @OutputFile abstract RegularFileProperty getOut()
    @TaskAction void run() { out.get().asFile.text = text.get() }
}
tasks.register('gen', Gen) {
    text = 'cached by the remote build cache'
    out = layout.buildDirectory.file('gen.txt')
}
`)

	run := func() string {
		cmd := exec.Command(gradle, "--no-daemon", "--build-cache", "--console=plain", "-i", "gen")
		cmd.Dir = project
		cmd.Env = append(os.Environ(),
			"GRADLE_USER_HOME="+filepath.Join(x.env["HOME"], ".gradle"),
			"CI_GRADLE_BUILD_CACHE=https://"+front,
			"CI_RUNNER_GRADLE_BUILD_CACHE=https://"+front,
			"CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN="+testToken,
		)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("gradle: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), testToken) {
			t.Errorf("Gradle's output holds the token:\n%s", out.String())
		}
		return out.String()
	}

	first := run()
	if !strings.Contains(first, "ci-cache: remote build cache on https://"+front+", writes authenticated") {
		t.Errorf("first build does not say the writes are authenticated:\n%s", first)
	}
	if !cache.sawBasic(http.MethodPut, testToken) {
		t.Fatalf("no authenticated store reached the cache: %v\n%s", cache.all(), first)
	}

	if err := os.RemoveAll(filepath.Join(project, "build")); err != nil {
		t.Fatal(err)
	}
	second := run()
	if !strings.Contains(second, "> Task :gen FROM-CACHE") {
		t.Errorf("the second build was not served by the remote cache:\n%s", second)
	}
	if !store.any() {
		t.Fatalf("the redirect of the hit never reached the object store:\n%s", second)
	}
	if h := store.authorizations(); len(h) != 0 {
		t.Errorf("the object store on another host received Authorization %q", h)
	}
}

// ---- helpers ----------------------------------------------------------------

type (
	seen struct {
		method, path, auth string
	}
	recorder struct {
		mu   sync.Mutex
		reqs []seen
	}
)

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, seen{req.Method, req.URL.Path, req.Header.Get("Authorization")})
}

func (r *recorder) all() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.reqs...)
}

func (r *recorder) any() bool { return len(r.all()) > 0 }

func (r *recorder) authorizations() []string {
	var out []string
	for _, s := range r.all() {
		if s.auth != "" {
			out = append(out, s.auth)
		}
	}
	return out
}

func (r *recorder) sawBasic(method, password string) bool {
	for _, s := range r.all() {
		req, _ := http.NewRequest(method, "https://x/", nil)
		req.Header.Set("Authorization", s.auth)
		if _, pw, ok := req.BasicAuth(); s.method == method && ok && pw == password {
			return true
		}
	}
	return false
}

// testPKI is a CA and a server certificate for the given IP addresses.
func testPKI(t *testing.T, ips ...string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "cache"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// listenTLS serves h on addr over TLS and returns the address it listens on.
func listenTLS(t *testing.T, addr string, cert tls.Certificate, h http.Handler) string {
	t.Helper()
	l, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Log(err)
		}
	}()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return l.Addr().String()
}

func writeCA(t *testing.T, x *h, cert *x509.Certificate) {
	t.Helper()
	ca := filepath.Join(x.dir, "ca-bundle.pem")
	writeFile(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})))
	x.env["SSL_CERT_FILE"] = ca
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func firstLineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// walkNoToken fails if any file under dir holds the token: the step writes the
// init script, the trust store and gradle.properties, none of which may.
func walkNoToken(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testToken)) {
			t.Errorf("%s holds the token", p)
		}
		return nil
	})
}
