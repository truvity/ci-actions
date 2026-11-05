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

	"go.yaml.in/yaml/v3"
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
	if !strings.HasPrefix(log, "::add-mask::"+testToken+"\n") || strings.Count(log, testToken) != 1 {
		t.Errorf("the token appears other than in the one mask command, the step's first line:\n%s", log)
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
	log := x.out.String()
	if !strings.HasPrefix(log, "::add-mask::two words\n") || strings.Count(log, "two words") != 1 || !strings.Contains(log, "::error::CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN holds whitespace") {
		t.Errorf("the refused token is not masked first, or appears beyond its mask:\n%s", log)
	}
}

// A step that fails before it reaches the build cache (here on a malformed
// runner value for the Maven registry) has masked the token already: a later
// `if: failure()` step that dumps the environment shows *** for it.
func TestGradleCacheMasksTheTokenBeforeAnEarlyFailure(t *testing.T) {
	x := newH(t)
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = "ftp://cache.invalid/maven"
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN"] = " " + testToken + "\r\nsecond-line-of-it\n"
	if err := runGradleCache(x, http.DefaultClient); err == nil {
		t.Fatal("accepted a malformed runner value")
	}
	log := x.out.String()
	if want := "::add-mask::" + testToken + "\n::add-mask::second-line-of-it\n"; !strings.HasPrefix(log, want) {
		t.Errorf("the step's output does not start with the masks %q:\n%s", want, log)
	}
	if strings.Count(log, testToken) != 1 || !strings.Contains(log, "::error::") {
		t.Errorf("log: %s", log)
	}
}

// The action masks the runner's secrets in its very first step, a plain bash
// one, so nothing the action prints (the binary's fetch included) and no path
// out of it, a failure included, precedes the mask. The step is run here as
// the runner runs it (bash -e), on a well-formed, a multi-line and an absent
// value.
func TestSetupDevboxMasksTheRunnerSecretsFirst(t *testing.T) {
	b, err := os.ReadFile("../../setup-devbox/action.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Runs struct {
			Steps []struct {
				Name, Shell, Run, Uses, If string
				Env                        map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(b, &action); err != nil {
		t.Fatal(err)
	}
	if len(action.Runs.Steps) == 0 {
		t.Fatal("no steps")
	}
	first := action.Runs.Steps[0]
	if first.Name != "Mask the runner's secrets" || first.Shell != "bash" || first.Uses != "" || first.If != "" || len(first.Env) != 0 {
		t.Fatalf("the first step is %q (shell %q, uses %q, if %q), not the unconditional bash mask step", first.Name, first.Shell, first.Uses, first.If)
	}
	if strings.Contains(first.Run, "ACTION_PATH") || strings.Contains(first.Run, "${{") {
		t.Errorf("the mask step runs something other than its own bash:\n%s", first.Run)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for name, tc := range map[string]struct {
		token string
		set   bool
		want  string
	}{
		"a token":            {testToken, true, "::add-mask::" + testToken + "\n"},
		"a trailing newline": {testToken + "\n", true, "::add-mask::" + testToken + "\n"},
		"lines and blanks":   {"  " + testToken + "\r\n\n \t\nsecond-line-of-it", true, "::add-mask::" + testToken + "\n::add-mask::second-line-of-it\n"},
		"empty":              {"", true, ""},
		"absent":             {"", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(bash, "--noprofile", "--norc", "-eo", "pipefail", "-c", first.Run)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			if tc.set {
				cmd.Env = append(cmd.Env, "CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN="+tc.token)
			}
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("the mask step failed: %v\n%s", err, stderr.String())
			}
			if out.String() != tc.want || stderr.Len() != 0 {
				t.Errorf("stdout %q (want %q), stderr %q", out.String(), tc.want, stderr.String())
			}
		})
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
	gradle := gradleOrSkip(t)

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

	project := gradleProbeProject(t, x)
	run := func() string {
		return runGradle(t, gradle, project, x,
			"CI_GRADLE_BUILD_CACHE=https://"+front,
			"CI_RUNNER_GRADLE_BUILD_CACHE=https://"+front,
		)
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

// TestGradleWithholdsTheTokenFromAnyOtherCache runs a real Gradle, the
// runner's credential in its environment, against a build cache the init
// script must NOT send it to, and proves that cache receives no Authorization
// header on any request while Gradle does read and store through it:
//
//   - a caller's cache (the caller's gradle-build-cache) on a runner that
//     names no build cache of its own;
//   - a cache other than the one the runner names, on the same origin, so
//     only the full address tells them apart;
//   - the runner's own cache over plain http, where the header would be
//     readable on the path.
//
// The cache accepts anonymous stores, so the builds pass either way and only
// the header decides. An init script that always sends the credential fails
// every case. Like TestGradleSendsTheTokenToTheCacheOnly, it runs only where a
// Gradle is found.
func TestGradleWithholdsTheTokenFromAnyOtherCache(t *testing.T) {
	gradle := gradleOrSkip(t)
	caPEM, serverCert := testPKI(t, "127.0.0.1")
	for name, tc := range map[string]struct {
		tls bool
		// step names the cache to the step, which installs the init script;
		// gradle is the build cache variables the Gradle build sees.
		step   func(x *h, cache string)
		gradle func(cache string) []string
		want   string
	}{
		"a caller's cache": {
			tls:    true,
			step:   func(x *h, cache string) { x.env["CALLER_GRADLE_BUILD_CACHE"] = cache },
			gradle: func(cache string) []string { return []string{"CI_GRADLE_BUILD_CACHE=" + cache} },
			want:   "writes anonymous: the runner's credential is for its own cache only",
		},
		"a cache other than the runner's, on its origin": {
			tls:  true,
			step: func(x *h, cache string) { x.env["CALLER_GRADLE_BUILD_CACHE"] = cache },
			gradle: func(cache string) []string {
				return []string{"CI_GRADLE_BUILD_CACHE=" + cache, "CI_RUNNER_GRADLE_BUILD_CACHE=" + cache + "/runner"}
			},
			want: "writes anonymous: the runner's credential is for its own cache only",
		},
		"the runner's own cache over plain http": {
			step: func(x *h, cache string) { x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = cache },
			gradle: func(cache string) []string {
				return []string{"CI_GRADLE_BUILD_CACHE=" + cache, "CI_RUNNER_GRADLE_BUILD_CACHE=" + cache + "/"}
			},
			want: "writes anonymous: the runner's credential is not sent over plain http",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cache := &recorder{}
			objects := map[string][]byte{}
			var mu sync.Mutex
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cache.add(r)
				key := strings.TrimPrefix(r.URL.Path, "/")
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && key == "":
					_, _ = w.Write([]byte("ok\n"))
				case r.Method == http.MethodPut:
					objects[key], _ = io.ReadAll(r.Body)
					w.WriteHeader(http.StatusCreated)
				case r.Method == http.MethodGet:
					b, ok := objects[key]
					if !ok {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write(b)
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			})
			x := newH(t)
			client := http.DefaultClient
			var url string
			if tc.tls {
				url = "https://" + listenTLS(t, "127.0.0.1:0", serverCert, h)
				writeFile(t, filepath.Join(x.dir, "ca-bundle.pem"), string(caPEM))
				x.env["SSL_CERT_FILE"] = filepath.Join(x.dir, "ca-bundle.pem")
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(caPEM)
				client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
			} else {
				url = "http://" + listen(t, "127.0.0.1:0", nil, h)
			}
			tc.step(x, url)
			x.env["CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN"] = testToken
			if err := runGradleCache(x, client); err != nil {
				t.Fatalf("%v\n%s", err, x.out.String())
			}

			project := gradleProbeProject(t, x)
			first := runGradle(t, gradle, project, x, tc.gradle(url)...)
			if !strings.Contains(first, "ci-cache: remote build cache on "+url+", "+tc.want) {
				t.Errorf("the build does not say %q:\n%s", tc.want, first)
			}
			if err := os.RemoveAll(filepath.Join(project, "build")); err != nil {
				t.Fatal(err)
			}
			second := runGradle(t, gradle, project, x, tc.gradle(url)...)
			if !strings.Contains(second, "> Task :gen FROM-CACHE") {
				t.Errorf("the second build was not served by the cache, so it was never really used:\n%s", second)
			}
			var stored bool
			for _, s := range cache.all() {
				stored = stored || s.method == http.MethodPut
			}
			if !stored {
				t.Fatalf("no store reached the cache, so nothing was checked: %v\n%s", cache.all(), first)
			}
			if a := cache.authorizations(); len(a) != 0 {
				t.Errorf("the cache received Authorization %q", a)
			}
		})
	}
}

// ---- helpers ----------------------------------------------------------------

// gradleOrSkip is the gradle launcher GRADLE names, else the one on PATH; the
// test skips without one.
func gradleOrSkip(t *testing.T) string {
	t.Helper()
	gradle := os.Getenv("GRADLE")
	if gradle == "" {
		gradle, _ = exec.LookPath("gradle")
	}
	if gradle == "" {
		t.Skip("no Gradle: set GRADLE to a gradle launcher to run the end-to-end check")
	}
	return gradle
}

// gradleProbeProject is a build with one cacheable task and no local build
// cache, so a second build can only be served by the remote one.
func gradleProbeProject(t *testing.T, x *h) string {
	t.Helper()
	project := filepath.Join(x.dir, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
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
	return project
}

// runGradle builds the probe project with the step's Gradle user home, the
// runner's credential and the given build cache variables in its environment,
// and fails if Gradle's output holds the credential.
func runGradle(t *testing.T, gradle, project string, x *h, env ...string) string {
	t.Helper()
	cmd := exec.Command(gradle, "--no-daemon", "--build-cache", "--console=plain", "-i", "gen")
	cmd.Dir = project
	cmd.Env = append(os.Environ(),
		"GRADLE_USER_HOME="+filepath.Join(x.env["HOME"], ".gradle"),
		"CI_RUNNER_GRADLE_BUILD_CACHE_TOKEN="+testToken,
	)
	cmd.Env = append(cmd.Env, env...)
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
	return listen(t, addr, &cert, h)
}

// listen serves h on addr, over TLS with cert or plain http without one, and
// returns the address it listens on.
func listen(t *testing.T, addr string, cert *tls.Certificate, h http.Handler) string {
	t.Helper()
	var l net.Listener
	var err error
	if cert != nil {
		l, err = tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{*cert}})
	} else {
		l, err = net.Listen("tcp", addr)
	}
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
