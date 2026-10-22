package setupdevbox

import (
	"context"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"

	"github.com/truvity/ci-actions/internal/runnerenv"
)

// gradleInitScript is the init script that points a Gradle build at the
// caches. It is written into the Gradle user home only when a cache answered,
// and it does nothing unless the variables this step exports are set.
//
//go:embed gradlecache/ci-cache.init.gradle
var gradleInitScript []byte

const (
	// trustStorePassword protects nothing: a trust store holds only public
	// certificates, and the JVM wants a password on a PKCS12 file.
	trustStorePassword = "changeit"
	initScriptName     = "ci-cache.init.gradle"
	trustStoreName     = "ci-cache-truststore.p12"
	probeTimeout       = 5 * time.Second
)

// systemCABundles are where a runner keeps the OS trust store as one PEM
// file, in the order tried when SSL_CERT_FILE does not name one.
var systemCABundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"}

// GradleCache points the job's Gradle builds at the estate's caches when the
// runner or the caller names them, and says so out loud:
//
//   - the Maven pull-through cache (CI_RUNNER_MAVEN_REGISTRY, else the
//     maven-registry input), exported to the job as CI_MAVEN_REGISTRY;
//   - the Gradle remote build cache (CI_RUNNER_GRADLE_BUILD_CACHE, else the
//     gradle-build-cache input), exported as CI_GRADLE_BUILD_CACHE.
//
// The precedence is that of ResolveEndpoints (the runner wins). A value is used
// only if its cache ANSWERS: a GET of <value>/ must return 200, which the cache
// does for exactly the front-ends it has enabled. A cache that is down, not
// resolving, not trusted, or an older release without the front-end is a
// warning and the job runs against the public repositories, as before: this
// step never fails a job for an endpoint (a malformed runner value is the one
// exception, as everywhere in this action).
//
// With at least one cache answering, the step
//   - writes the init script into $GRADLE_USER_HOME/init.d (default
//     ~/.gradle/init.d), which every Gradle build of the job reads, and
//   - for an https cache, writes a PKCS12 trust store made of the runner's OS
//     trust store (which holds the estate's private CA, installed on the image)
//     and names it in $GRADLE_USER_HOME/gradle.properties. A JVM does not read
//     the OS store; without this every request to a private-CA cache fails the
//     TLS handshake in Gradle although this step's own probe, which uses the OS
//     store, passed.
func GradleCache(ctx context.Context, e Env) error {
	e.defaults()
	type cache struct {
		what, runnerName, caller, output, export string
	}
	caches := []cache{
		{"maven registry", runnerenv.MavenRegistry, e.Getenv("CALLER_MAVEN_REGISTRY"), "maven-registry", "CI_MAVEN_REGISTRY"},
		{"gradle build cache", runnerenv.GradleBuildCache, e.Getenv("CALLER_GRADLE_BUILD_CACHE"), "gradle-build-cache", "CI_GRADLE_BUILD_CACHE"},
	}
	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}

	var usable []string // the https ones decide whether the JVM needs a trust store
	var answered int
	for _, c := range caches {
		v, src, err := runnerenv.Pick(c.runnerName, e.Getenv(c.runnerName), c.caller, false)
		if err != nil {
			return e.fail("%v", err)
		}
		e.printf("%s\n", runnerenv.Describe(c.what, src, v, c.runnerName))
		if err := e.setOutput(c.output+"-source", string(src)); err != nil {
			return err
		}
		if v == "" {
			continue
		}
		base, err := cacheURL(v)
		if err != nil {
			if src == runnerenv.FromRunner {
				return e.fail("%s: %v", c.runnerName, err)
			}
			e.printf("::warning::gradle-cache: the %s %q is not used: %v\n", c.what, runnerenv.Redact(v), err)
			continue
		}
		if err := probe(ctx, client, base+"/"); err != nil {
			e.printf("::warning::gradle-cache: the %s %s does not answer (%v); this job's Gradle builds use the public repositories and no remote build cache\n", c.what, runnerenv.Redact(base), err)
			continue
		}
		e.printf("gradle-cache: the %s %s answers\n", c.what, runnerenv.Redact(base))
		if err := e.exportEnv(c.export, base); err != nil {
			return err
		}
		if err := e.setOutput(c.output, base); err != nil {
			return err
		}
		answered++
		usable = append(usable, base)
	}
	if answered == 0 {
		return nil
	}

	home := e.gradleUserHome()
	if home == "" {
		e.printf("::warning::gradle-cache: neither GRADLE_USER_HOME nor HOME is set; the init script is not installed\n")
		return nil
	}
	if err := installInitScript(home); err != nil {
		e.printf("::warning::gradle-cache: cannot install the init script: %v\n", err)
		return nil
	}
	e.printf("gradle-cache: init script %s\n", filepath.Join(home, "init.d", initScriptName))

	for _, u := range usable {
		if strings.HasPrefix(u, "https://") {
			if err := e.trustSystemStore(home); err != nil {
				e.printf("::warning::gradle-cache: Gradle's JVM may not trust the cache's certificate: %v\n", err)
			}
			break
		}
	}
	return nil
}

// gradleUserHome is where Gradle reads its user-level files from.
func (e Env) gradleUserHome() string {
	if h := e.Getenv("GRADLE_USER_HOME"); h != "" {
		return h
	}
	if h := e.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".gradle")
	}
	return ""
}

// cacheURL checks a cache address and returns it without a trailing slash.
// It is placed into a Gradle script and a properties file by name, so only a
// plain http(s) URL is accepted.
func cacheURL(v string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil {
		return "", errors.New("not a URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("not an http(s) URL")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("a cache URL has no query, fragment or credentials")
	}
	for _, r := range v {
		if r < 0x21 || r == 0x7f || r == '\'' || r == '"' || r == '\\' || r == '$' {
			return "", errors.New("holds a character that is not part of a plain URL")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// probe asks the cache whether it serves: 200, nothing else.
func probe(ctx context.Context, c *http.Client, u string) error {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			last = errors.New(runnerenv.Redact(strings.ReplaceAll(err.Error(), "\"", "")))
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		last = fmt.Errorf("GET / answered %s", resp.Status)
		if resp.StatusCode == http.StatusNotFound {
			// The front-end is not there (an older release, or it is switched
			// off): asking again changes nothing.
			break
		}
	}
	return last
}

func installInitScript(home string) error {
	dir := filepath.Join(home, "init.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, initScriptName), gradleInitScript, 0o644)
}

// trustSystemStore writes the OS trust store as a PKCS12 trust store beside the
// Gradle user home's properties and names it there as the JVM's trust store.
// The Gradle build JVM reads systemProp.* from the user home's
// gradle.properties; the project's own gradle.properties (org.gradle.jvmargs
// and the like) is not touched, and a trust store the user home already names
// is left alone.
func (e Env) trustSystemStore(home string) error {
	props := filepath.Join(home, "gradle.properties")
	if b, err := os.ReadFile(props); err == nil && strings.Contains(string(b), "systemProp.javax.net.ssl.trustStore=") {
		return errors.New(props + " already names a trust store; the cache's CA is not added to it")
	}
	bundle := e.Getenv("SSL_CERT_FILE")
	if bundle == "" {
		for _, p := range systemCABundles {
			if _, err := os.Stat(p); err == nil {
				bundle = p
				break
			}
		}
	}
	if bundle == "" {
		return errors.New("no OS trust store file found (SSL_CERT_FILE, /etc/ssl/certs/ca-certificates.crt)")
	}
	certs, err := readCertificates(bundle)
	if err != nil {
		return err
	}
	der, err := pkcs12.Modern.EncodeTrustStore(certs, trustStorePassword)
	if err != nil {
		return fmt.Errorf("encoding the trust store: %w", err)
	}
	p12 := filepath.Join(home, trustStoreName)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p12, der, 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(props, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# Written by truvity/ci-actions setup-devbox (gradle-cache): the JVM does not read the OS trust store, which holds the CI plane's CA.\nsystemProp.javax.net.ssl.trustStore=%s\nsystemProp.javax.net.ssl.trustStoreType=PKCS12\nsystemProp.javax.net.ssl.trustStorePassword=%s\n",
		escapeProperty(p12), trustStorePassword)
	if err != nil {
		return err
	}
	e.printf("gradle-cache: Gradle's trust store %s (%d certificates of %s)\n", p12, len(certs), bundle)
	return nil
}

// escapeProperty escapes a path for a Java properties file: a backslash starts
// an escape there.
func escapeProperty(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

// readCertificates parses every CERTIFICATE block of a PEM bundle. A block that
// does not parse is skipped: bundles carry the odd extension a stricter parser
// would refuse, and one bad entry must not leave the JVM without the rest.
func readCertificates(file string) ([]*x509.Certificate, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
			certs = append(certs, c)
		}
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("%s holds no certificate", file)
	}
	return certs, nil
}
