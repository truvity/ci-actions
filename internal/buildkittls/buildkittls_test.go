package buildkittls

import (
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// block is a PEM block of the given type around deterministic bytes.
func block(typ string, seed byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}))
}

var (
	caPEM   = block("CERTIFICATE", 1, 120)
	certPEM = block("CERTIFICATE", 7, 150)
	keyPEM  = block("PRIVATE KEY", 0x41, 140)
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type result struct {
	src      Source
	dir      string
	err      error
	out, log string
	envFile  string
	temp     string
}

func run(t *testing.T, vars map[string]string) result {
	t.Helper()
	temp := t.TempDir()
	if err := os.Mkdir(filepath.Join(temp, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, log strings.Builder
	envFile := filepath.Join(temp, "github_env")
	src, dir, err := Run(Options{Getenv: env(vars), RunnerTemp: filepath.Join(temp, "tmp"), GithubEnv: envFile, Out: &out, Err: &log})
	return result{src, dir, err, out.String(), log.String(), envFile, temp}
}

func TestSources(t *testing.T) {
	all := map[string]string{CAVar: caPEM, CertVar: certPEM, KeyVar: keyPEM}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range all {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name    string
		vars    map[string]string
		want    Source
		wantErr string // substring of the ::error:: line
		wantLog string
	}{
		{"nothing set", map[string]string{}, None, "", "no TLS material"},
		{"dir set", map[string]string{DirVar: "/etc/tls"}, FromDir, "", "TLS material from " + DirVar},
		{"dir wins over the PEM variables", with(map[string]string{DirVar: "/etc/tls"}), FromDir, "", "TLS material from " + DirVar},
		{"dir wins over a partial set", map[string]string{DirVar: "/etc/tls", CAVar: caPEM}, FromDir, "", "TLS material from " + DirVar},
		{"all three", all, FromEnv, "", "TLS material from runner env"},
		{"whitespace-only dir is unset", with(map[string]string{DirVar: " \n"}), FromEnv, "", "TLS material from runner env"},
		{"only the CA", map[string]string{CAVar: caPEM}, None, "sets " + CAVar + " but not " + CertVar + ", " + KeyVar, ""},
		{"only the key", map[string]string{KeyVar: keyPEM}, None, "sets " + KeyVar + " but not " + CAVar + ", " + CertVar, ""},
		{"cert and key, no CA", map[string]string{CertVar: certPEM, KeyVar: keyPEM}, None, "but not " + CAVar, ""},
		{"a blank variable is missing", with(map[string]string{CertVar: "  \n"}), None, "but not " + CertVar, ""},
		{"CA is not PEM", with(map[string]string{CAVar: "hello"}), None, CAVar + " is not PEM", ""},
		{"CA is a key", with(map[string]string{CAVar: keyPEM}), None, CAVar + ` holds a "PRIVATE KEY" block, not a certificate`, ""},
		{"cert is a key", with(map[string]string{CertVar: keyPEM}), None, CertVar + ` holds a "PRIVATE KEY" block`, ""},
		{"cert is a CSR", with(map[string]string{CertVar: block("CERTIFICATE REQUEST", 3, 80)}), None, CertVar + ` holds a "CERTIFICATE REQUEST" block`, ""},
		{"key is a certificate", with(map[string]string{KeyVar: certPEM}), None, KeyVar + ` holds a "CERTIFICATE" block, not a private key`, ""},
		{"key is a public key", with(map[string]string{KeyVar: block("PUBLIC KEY", 3, 80)}), None, KeyVar + ` holds a "PUBLIC KEY" block`, ""},
		{"key is encrypted", with(map[string]string{KeyVar: block("ENCRYPTED PRIVATE KEY", 3, 80)}), None, KeyVar + " holds an encrypted private key", ""},
		{"two keys", with(map[string]string{KeyVar: keyPEM + keyPEM}), None, KeyVar + " holds 2 private key blocks", ""},
		{"truncated CA", with(map[string]string{CAVar: strings.TrimSuffix(strings.TrimSpace(caPEM), "-----")}), None, CAVar + " is not valid PEM", ""},
		{"text after the CA", with(map[string]string{CAVar: caPEM + "trailing words\n"}), None, CAVar + " is not valid PEM", ""},
		{"text before the CA", with(map[string]string{CAVar: "note\n" + caPEM}), None, CAVar + " is not PEM", ""},
		{"CA bundle", with(map[string]string{CAVar: caPEM + "\n" + certPEM}), FromEnv, "", ""},
		{"RSA key", with(map[string]string{KeyVar: block("RSA PRIVATE KEY", 5, 90)}), FromEnv, "", ""},
		{"EC key", with(map[string]string{KeyVar: block("EC PRIVATE KEY", 5, 90)}), FromEnv, "", ""},
		{"CRLF and padding", with(map[string]string{CAVar: "\r\n" + strings.ReplaceAll(caPEM, "\n", "\r\n") + "\r\n\r\n"}), FromEnv, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := run(t, c.vars)
			if c.wantErr != "" {
				if !errors.Is(r.err, ErrFailed) {
					t.Fatalf("want ErrFailed, got %v (out %q)", r.err, r.out)
				}
				if !strings.Contains(r.out, "::error::") || !strings.Contains(r.out, c.wantErr) {
					t.Fatalf("want an ::error:: with %q, got %q", c.wantErr, r.out)
				}
				if _, err := os.Stat(r.envFile); err == nil {
					t.Fatalf("GITHUB_ENV was written on a failure")
				}
			} else if r.err != nil {
				t.Fatalf("unexpected %v (out %q)", r.err, r.out)
			}
			if r.src != c.want && c.wantErr == "" {
				t.Fatalf("source %q, want %q", r.src, c.want)
			}
			if !strings.Contains(r.log, c.wantLog) {
				t.Fatalf("log %q lacks %q", r.log, c.wantLog)
			}
			if c.want != FromEnv || c.wantErr != "" {
				if _, err := os.Stat(r.envFile); err == nil {
					t.Fatalf("GITHUB_ENV written although the PEM form did not win")
				}
				if left, _ := os.ReadDir(filepath.Join(r.temp, "tmp")); len(left) != 0 {
					t.Fatalf("RUNNER_TEMP holds %v although the PEM form did not win", left)
				}
			}
		})
	}
}

func TestWritesTheFiles(t *testing.T) {
	r := run(t, map[string]string{CAVar: caPEM, CertVar: certPEM + "\n", KeyVar: "  " + keyPEM})
	if r.err != nil || r.src != FromEnv {
		t.Fatal(r.err, r.src, r.out)
	}
	if !strings.HasPrefix(r.dir, filepath.Join(r.temp, "tmp")+string(filepath.Separator)) {
		t.Fatalf("directory %q is not under RUNNER_TEMP", r.dir)
	}
	if !plainPath.MatchString(r.dir) {
		t.Fatalf("directory %q would be refused by the consumers", r.dir)
	}
	fi, err := os.Stat(r.dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v, %v", fi.Mode().Perm(), err)
	}
	entries, _ := os.ReadDir(r.dir)
	if len(entries) != 3 {
		t.Fatalf("want exactly ca.pem, cert.pem, key.pem; got %v", entries)
	}
	for _, f := range []struct {
		name string
		want string
		mode os.FileMode
	}{{"ca.pem", caPEM, 0o644}, {"cert.pem", certPEM, 0o644}, {"key.pem", keyPEM, 0o600}} {
		b, err := os.ReadFile(filepath.Join(r.dir, f.name))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != f.want {
			t.Errorf("%s = %q, want %q", f.name, b, f.want)
		}
		fi, _ := os.Stat(filepath.Join(r.dir, f.name))
		if fi.Mode().Perm() != f.mode {
			t.Errorf("%s mode %v, want %v", f.name, fi.Mode().Perm(), f.mode)
		}
	}
	got, _ := os.ReadFile(r.envFile)
	if string(got) != DirVar+"="+r.dir+"\n" {
		t.Errorf("GITHUB_ENV = %q", got)
	}
}

func TestFreshDirectoryEachRun(t *testing.T) {
	temp := t.TempDir()
	vars := map[string]string{CAVar: caPEM, CertVar: certPEM, KeyVar: keyPEM}
	_, a, err1 := Run(Options{Getenv: env(vars), RunnerTemp: temp})
	_, b, err2 := Run(Options{Getenv: env(vars), RunnerTemp: temp})
	if err1 != nil || err2 != nil || a == b {
		t.Fatalf("two runs must not share a directory: %q %q %v %v", a, b, err1, err2)
	}
}

func TestRunnerTempMissing(t *testing.T) {
	var out strings.Builder
	_, _, err := Run(Options{Getenv: env(map[string]string{CAVar: caPEM, CertVar: certPEM, KeyVar: keyPEM}), Out: &out})
	if !errors.Is(err, ErrFailed) || !strings.Contains(out.String(), "RUNNER_TEMP") {
		t.Fatalf("%v %q", err, out.String())
	}
}

// Nothing the step prints may carry the key, and every line of it is masked
// before anything else is printed.
func TestKeyIsMaskedAndNeverPrinted(t *testing.T) {
	body := func(pemText string) []string {
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(pemText), "\n") {
			if !strings.HasPrefix(l, "-----") {
				lines = append(lines, l)
			}
		}
		return lines
	}
	secret := body(keyPEM)
	if len(secret) < 2 {
		t.Fatal("the fixture key should span several lines")
	}
	scenarios := map[string]map[string]string{
		"success":      {CAVar: caPEM, CertVar: certPEM, KeyVar: keyPEM},
		"bad CA":       {CAVar: "nope", CertVar: certPEM, KeyVar: keyPEM},
		"bad cert":     {CAVar: caPEM, CertVar: keyPEM, KeyVar: keyPEM},
		"bad key type": {CAVar: caPEM, CertVar: certPEM, KeyVar: certPEM},
		"partial":      {KeyVar: keyPEM},
	}
	for name, vars := range scenarios {
		t.Run(name, func(t *testing.T) {
			r := run(t, vars)
			mask := ""
			for _, l := range strings.Split(r.out, "\n") {
				if strings.HasPrefix(l, "::add-mask::") {
					mask += l + "\n"
				}
			}
			if vars[KeyVar] == keyPEM {
				for _, l := range secret {
					if !strings.Contains(mask, "::add-mask::"+l+"\n") {
						t.Errorf("key line %q is not masked", l)
					}
				}
				if strings.Contains(mask, "-----") {
					t.Errorf("armor lines should not be masked: %q", mask)
				}
				if first := strings.SplitN(r.out, "\n", 2)[0]; !strings.HasPrefix(first, "::add-mask::") {
					t.Errorf("the first output line is %q, not a mask", first)
				}
			}
			// Outside the mask commands, no key line appears anywhere.
			rest := strings.ReplaceAll(r.out, mask, "") + r.log
			for _, l := range secret {
				if strings.Contains(rest, l) {
					t.Errorf("key line %q leaked into the output", l)
				}
			}
			if strings.Contains(rest, "PRIVATE KEY-----") {
				t.Errorf("a key header leaked: %q", rest)
			}
		})
	}
}

func TestMaskedNothingWhenKeyUnset(t *testing.T) {
	r := run(t, map[string]string{CAVar: caPEM})
	if strings.Contains(r.out, "::add-mask::") {
		t.Fatalf("nothing to mask: %q", r.out)
	}
}

func TestExportFailure(t *testing.T) {
	temp := t.TempDir()
	var out strings.Builder
	_, _, err := Run(Options{
		Getenv:     env(map[string]string{CAVar: caPEM, CertVar: certPEM, KeyVar: keyPEM}),
		RunnerTemp: temp, GithubEnv: filepath.Join(temp, "missing", "env"), Out: &out,
	})
	if !errors.Is(err, ErrFailed) || !strings.Contains(out.String(), "cannot export "+DirVar) {
		t.Fatalf("%v %q", err, out.String())
	}
	if left, _ := os.ReadDir(temp); len(left) != 0 {
		t.Fatalf("a failed run left %v behind", left)
	}
}
