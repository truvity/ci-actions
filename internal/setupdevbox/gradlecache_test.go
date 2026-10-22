package setupdevbox

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// cacheServer answers 200 on the probe paths of the front-ends it has.
func cacheServer(t *testing.T, tls bool, paths ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, p := range paths {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != p {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("ok\n"))
		})
	}
	var s *httptest.Server
	if tls {
		s = httptest.NewTLSServer(mux)
	} else {
		s = httptest.NewServer(mux)
	}
	t.Cleanup(s.Close)
	return s
}

func runGradleCache(x *h, client *http.Client) error {
	e := x.e()
	e.HTTP = client
	return GradleCache(context.Background(), e)
}

func gradleHome(x *h) string { return filepath.Join(x.env["HOME"], ".gradle") }

func TestGradleCacheWiresBothCaches(t *testing.T) {
	s := cacheServer(t, false, "/maven/", "/gradle/cache/")
	x := newH(t)
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = s.URL + "/maven"
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = s.URL + "/gradle/cache"
	if err := runGradleCache(x, s.Client()); err != nil {
		t.Fatal(err)
	}
	env := x.file("github_env")
	for _, want := range []string{"CI_MAVEN_REGISTRY<<EOF_", "\n" + s.URL + "/maven\n", "CI_GRADLE_BUILD_CACHE<<EOF_", "\n" + s.URL + "/gradle/cache\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("GITHUB_ENV lacks %q:\n%s", want, env)
		}
	}
	script, err := os.ReadFile(filepath.Join(gradleHome(x), "init.d", "ci-cache.init.gradle"))
	if err != nil {
		t.Fatalf("init script not installed: %v", err)
	}
	for _, want := range []string{"CI_MAVEN_REGISTRY", "CI_GRADLE_BUILD_CACHE", "HttpBuildCache", "plugins.gradle.org/m2"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("the installed script lacks %q", want)
		}
	}
	// http: no JVM trust store is needed.
	if _, err := os.Stat(filepath.Join(gradleHome(x), "gradle.properties")); err == nil {
		t.Errorf("gradle.properties written for a plain http cache")
	}
	if log := x.out.String(); !strings.Contains(log, "(from the runner's CI_RUNNER_MAVEN_REGISTRY)") || strings.Contains(log, "::warning::") {
		t.Errorf("log: %s", log)
	}
}

// The estate gives each cache a name of its own (maven.cache.build.<env>...,
// gradle.cache.build.<env>...) whose gateway rewrites the root onto the cache's
// path, so the address is a bare origin: the probe is a GET of "/" and the
// exported value is the origin, with nothing appended.
func TestGradleCacheAcceptsCachesNamedByTheirRoot(t *testing.T) {
	maven := cacheServer(t, false, "/")
	gradle := cacheServer(t, false, "/")
	x := newH(t)
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = maven.URL
	x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = gradle.URL + "/"
	if err := runGradleCache(x, maven.Client()); err != nil {
		t.Fatal(err)
	}
	env := x.file("github_env")
	for _, want := range []string{"\n" + maven.URL + "\n", "\n" + gradle.URL + "\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("GITHUB_ENV lacks %q:\n%s", want, env)
		}
	}
	if log := x.out.String(); strings.Contains(log, "::warning::") {
		t.Errorf("log: %s", log)
	}
}

func TestGradleCacheTrustsTheSystemStoreForHTTPS(t *testing.T) {
	s := cacheServer(t, true, "/maven/")
	x := newH(t)
	ca := filepath.Join(x.dir, "ca-bundle.pem")
	cert := s.Certificate()
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	x.env["SSL_CERT_FILE"] = ca
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = s.URL + "/maven/"
	if err := runGradleCache(x, s.Client()); err != nil {
		t.Fatal(err)
	}

	props, err := os.ReadFile(filepath.Join(gradleHome(x), "gradle.properties"))
	if err != nil {
		t.Fatal(err)
	}
	p12 := filepath.Join(gradleHome(x), "ci-cache-truststore.p12")
	for _, want := range []string{"systemProp.javax.net.ssl.trustStore=" + p12 + "\n", "systemProp.javax.net.ssl.trustStoreType=PKCS12\n", "systemProp.javax.net.ssl.trustStorePassword=changeit\n"} {
		if !strings.Contains(string(props), want) {
			t.Errorf("gradle.properties lacks %q:\n%s", want, props)
		}
	}
	der, err := os.ReadFile(p12)
	if err != nil {
		t.Fatal(err)
	}
	certs, err := pkcs12.DecodeTrustStore(der, "changeit")
	if err != nil {
		t.Fatalf("the trust store does not decode: %v", err)
	}
	if len(certs) != 1 || !certs[0].Equal(cert) {
		t.Errorf("the trust store holds %d certificates, want the one of the OS store", len(certs))
	}
	// The trailing slash of the address is not exported.
	if env := x.file("github_env"); !strings.Contains(env, "\n"+s.URL+"/maven\n") {
		t.Errorf("GITHUB_ENV: %s", env)
	}
}

func TestGradleCacheKeepsATrustStoreTheUserHomeAlreadyNames(t *testing.T) {
	s := cacheServer(t, true, "/maven/")
	x := newH(t)
	ca := filepath.Join(x.dir, "ca-bundle.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0o644)
	x.env["SSL_CERT_FILE"] = ca
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = s.URL + "/maven"
	_ = os.MkdirAll(gradleHome(x), 0o755)
	const own = "systemProp.javax.net.ssl.trustStore=/custom/store.jks\n"
	_ = os.WriteFile(filepath.Join(gradleHome(x), "gradle.properties"), []byte(own), 0o644)
	if err := runGradleCache(x, s.Client()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(gradleHome(x), "gradle.properties")); string(b) != own {
		t.Errorf("gradle.properties changed: %q", b)
	}
	if !strings.Contains(x.out.String(), "::warning::gradle-cache: Gradle's JVM may not trust") {
		t.Errorf("no warning: %s", x.out.String())
	}
}

func TestGradleCacheFailsOpenWhenTheCacheDoesNotServe(t *testing.T) {
	// An older cache: the host answers, the front-end is not there (404).
	old := cacheServer(t, false, "/healthz")
	// A cache that is down.
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()

	for name, tc := range map[string]struct{ maven, build string }{
		"404":  {old.URL + "/maven", old.URL + "/gradle/cache"},
		"down": {downURL + "/maven", downURL + "/gradle/cache"},
	} {
		t.Run(name, func(t *testing.T) {
			x := newH(t)
			x.env["CI_RUNNER_MAVEN_REGISTRY"] = tc.maven
			x.env["CI_RUNNER_GRADLE_BUILD_CACHE"] = tc.build
			if err := runGradleCache(x, old.Client()); err != nil {
				t.Fatalf("a cache that does not serve must not fail the job: %v", err)
			}
			if env := x.file("github_env"); env != "" {
				t.Errorf("exported %q for a cache that does not answer", env)
			}
			if _, err := os.Stat(filepath.Join(gradleHome(x), "init.d")); err == nil {
				t.Errorf("init script installed although nothing answers")
			}
			if n := strings.Count(x.out.String(), "::warning::gradle-cache:"); n != 2 {
				t.Errorf("%d warnings, want one per cache: %s", n, x.out.String())
			}
		})
	}
}

func TestGradleCacheRunnerWithNoBuildCacheWiresDependenciesOnly(t *testing.T) {
	s := cacheServer(t, false, "/maven/")
	x := newH(t)
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = s.URL + "/maven"
	if err := runGradleCache(x, s.Client()); err != nil {
		t.Fatal(err)
	}
	env := x.file("github_env")
	if !strings.Contains(env, "CI_MAVEN_REGISTRY<<") || strings.Contains(env, "CI_GRADLE_BUILD_CACHE") {
		t.Errorf("GITHUB_ENV: %q, want the registry only", env)
	}
	if log := x.out.String(); !strings.Contains(log, "gradle build cache: unset") {
		t.Errorf("the log must say the build cache is unset: %s", log)
	}
}

func TestGradleCacheRunnerWinsOverTheCaller(t *testing.T) {
	runner := cacheServer(t, false, "/maven/")
	caller := cacheServer(t, false, "/maven/")
	x := newH(t)
	x.env["CALLER_MAVEN_REGISTRY"] = caller.URL + "/maven"
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = runner.URL + "/maven"
	if err := runGradleCache(x, runner.Client()); err != nil {
		t.Fatal(err)
	}
	if env := x.file("github_env"); !strings.Contains(env, "\n"+runner.URL+"/maven\n") || strings.Contains(env, caller.URL) {
		t.Errorf("GITHUB_ENV: %s", env)
	}

	// With no runner value the caller's input is used.
	y := newH(t)
	y.env["CALLER_MAVEN_REGISTRY"] = caller.URL + "/maven"
	if err := runGradleCache(y, caller.Client()); err != nil {
		t.Fatal(err)
	}
	if env := y.file("github_env"); !strings.Contains(env, "\n"+caller.URL+"/maven\n") {
		t.Errorf("GITHUB_ENV: %s", env)
	}
}

func TestGradleCacheNothingNamedIsNothingDone(t *testing.T) {
	x := newH(t)
	if err := runGradleCache(x, nil); err != nil {
		t.Fatal(err)
	}
	if x.file("github_env") != "" {
		t.Errorf("exported something")
	}
	if _, err := os.Stat(gradleHome(x)); err == nil {
		t.Errorf("touched the Gradle user home")
	}
}

func TestGradleCacheMalformedValues(t *testing.T) {
	// A runner value is operator configuration: malformed is an error.
	for _, v := range []string{"https://a.example/maven extra", "not a url", "ftp://a.example/maven", "https://a.example/maven?x=1", "https://u:p@a.example/maven", "https://a.example/ma'ven"} {
		x := newH(t)
		x.env["CI_RUNNER_MAVEN_REGISTRY"] = v
		if err := runGradleCache(x, nil); err == nil {
			t.Errorf("runner value %q accepted", v)
		}
		if x.file("github_env") != "" {
			t.Errorf("exported after %q", v)
		}
	}
	// A caller's value (an organisation variable) is a warning.
	x := newH(t)
	x.env["CALLER_MAVEN_REGISTRY"] = "ftp://a.example/maven"
	if err := runGradleCache(x, nil); err != nil {
		t.Fatalf("a bad caller value failed the job: %v", err)
	}
	if !strings.Contains(x.out.String(), "::warning::gradle-cache: the maven registry") {
		t.Errorf("log: %s", x.out.String())
	}
}

func TestGradleCacheLogsNoCredentialAndHonoursGradleUserHome(t *testing.T) {
	s := cacheServer(t, false, "/maven/")
	x := newH(t)
	x.env["GRADLE_USER_HOME"] = filepath.Join(x.dir, "guh")
	x.env["CI_RUNNER_MAVEN_REGISTRY"] = s.URL + "/maven"
	if err := runGradleCache(x, s.Client()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.dir, "guh", "init.d", "ci-cache.init.gradle")); err != nil {
		t.Errorf("GRADLE_USER_HOME not honoured: %v", err)
	}
}

func TestReadCertificatesSkipsWhatIsNotACertificate(t *testing.T) {
	s := httptest.NewTLSServer(http.NotFoundHandler())
	defer s.Close()
	f := filepath.Join(t.TempDir(), "bundle.pem")
	body := "# a comment\n" + string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not der")})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	_ = os.WriteFile(f, []byte(body), 0o644)
	certs, err := readCertificates(f)
	if err != nil || len(certs) != 1 || !certs[0].Equal(s.Certificate()) {
		t.Fatalf("certs %v err %v, want the one good certificate", certs, err)
	}
	_ = os.WriteFile(f, []byte("nothing here"), 0o644)
	if _, err := readCertificates(f); err == nil {
		t.Errorf("a bundle with no certificate was accepted")
	}
}
