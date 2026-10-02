package setupdevbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// Preflight is what the rest of setup-devbox needs from the machine it runs
// on, checked FIRST and said out loud.
//
// This action runs with no privilege by contract: no root, no privilege
// escalation, no relinking of /bin/sh. A runner under the Pod Security
// `restricted` profile (uid 1001, no_new_privs, every capability dropped) is
// the reference environment. When that contract is not met the failure used to
// surface a minute later as a cryptic error from nix or devbox; here it is one
// `::error::` naming what is missing and what to do about it.
//
// Facts are printed always. Only conditions the rest of the action cannot work
// without fail the step:
//   - HOME, RUNNER_TEMP or the work directory is not writable
//   - nix is expected and its daemon/store does not answer
//   - nix is absent AND the runner cannot escalate privilege: the install step
//     would run the nix installer, which needs root
//
// Not failures, because the action no longer depends on them: no_new_privs set
// (escalation cannot work; nothing here asks for it), /bin/sh not being bash
// (steps say `shell: bash` explicitly).
//
// THE ONE ROOT STEP. Everything else here is privilege-free (devbox is the
// checksum-verified release binary in $RUNNER_TEMP/bin). The exception is the
// nix installer, which needs root and runs ONLY when nix is not baked into the
// runner image: that is a GitHub-hosted runner, which has root. A runner that
// is restricted (no_new_privs) and has no nix cannot be made to work, and this
// says so here, in one line, instead of a minute later in the installer.
//
// The probe locations can be redirected through PREFLIGHT_* variables so the
// tests can exercise every branch without being root.
func Preflight(ctx context.Context, e Env) error {
	e.defaults()
	procStatus := orDefault(e.lookupSet("PREFLIGHT_PROC_STATUS"), "/proc/self/status")
	shPath := orDefault(e.lookupSet("PREFLIGHT_SH_PATH"), "/bin/sh")
	expectNix := orDefault(e.lookupSet("PREFLIGHT_EXPECT_NIX"), "auto") // auto | true | false
	workdir := e.Getenv("GITHUB_WORKSPACE")
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	nixBin, nixSet := e.lookup("PREFLIGHT_NIX_BIN")
	if !nixSet {
		nixBin = pathLookup("nix", e.Getenv("PATH"))
	}
	devboxBin, devSet := e.lookup("PREFLIGHT_DEVBOX_BIN")
	if !devSet {
		devboxBin = pathLookup("devbox", e.Getenv("PATH"))
	}

	var problems []string
	uname := "?"
	if u, err := user.Current(); err == nil {
		uname = u.Username
	}
	e.printf("preflight: uid=%d gid=%d user=%s\n", os.Getuid(), os.Getgid(), uname)

	// no_new_privs: informational. Set means setuid binaries cannot raise
	// privilege, which is the point of the restricted profile.
	nnp := ""
	if b, err := os.ReadFile(procStatus); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "NoNewPrivs:" {
				nnp = f[1]
				break
			}
		}
	}
	switch nnp {
	case "1":
		e.printf("preflight: no_new_privs=1 (privilege escalation is off; this action never asks for it)\n")
	case "0":
		e.printf("preflight: no_new_privs=0 (privilege escalation is possible; this action does not use it)\n")
	default:
		e.printf("preflight: no_new_privs=unknown (could not read NoNewPrivs from %s)\n", procStatus)
	}

	// /bin/sh: informational. Steps declare `shell: bash` themselves.
	shReal := realpath(shPath)
	bashPath := pathLookup("bash", e.Getenv("PATH"))
	if bashPath == "" {
		bashPath = "/bin/bash"
	}
	bashReal := realpath(bashPath)
	if bashReal != "" && shReal == bashReal {
		e.printf("preflight: %s -> %s (bash)\n", shPath, shReal)
	} else {
		e.printf("preflight: %s -> %s (not bash; steps use 'shell: bash', recipes that need bash as sh must say so themselves)\n", shPath, shReal)
	}

	// Writable means a file can actually be created there: a read-only mount
	// passes a mode-bit test and fails a write.
	writable := func(label, dir string) {
		if dir == "" {
			e.printf("preflight: %s is not set\n", label)
			problems = append(problems, label+" is not set")
			return
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			if f, err := os.CreateTemp(dir, ".preflight.*"); err == nil {
				f.Close()
				os.Remove(f.Name())
				e.printf("preflight: %s=%s writable\n", label, dir)
				return
			}
		}
		e.printf("preflight: %s=%s NOT writable\n", label, dir)
		problems = append(problems, fmt.Sprintf("%s (%s) is not writable: mount a writable volume there", label, dir))
	}
	writable("HOME", e.Getenv("HOME"))
	writable("RUNNER_TEMP", e.Getenv("RUNNER_TEMP"))
	writable("workdir", workdir)

	// devbox: baked into the image, or installed by this action from the
	// release binary, checksum-verified, into $RUNNER_TEMP/bin. Neither needs
	// privilege.
	if devboxBin != "" {
		e.printf("preflight: devbox is baked (%s); nothing to install\n", devboxBin)
	} else {
		e.printf("preflight: devbox is not baked; the install step will fetch the release binary into $RUNNER_TEMP/bin (checksum-verified, no privilege)\n")
	}

	// nix: expected when it is already on PATH (a runner image that bakes it)
	// or the caller says so. A hosted runner has none before the install step.
	if expectNix == "auto" {
		if nixBin != "" {
			expectNix = "true"
		} else {
			expectNix = "false"
		}
	}
	if expectNix == "true" {
		switch {
		case nixBin == "":
			e.printf("preflight: nix expected but not on PATH\n")
			problems = append(problems, "nix is expected but not on PATH: use a runner image that bakes nix, or unset the expectation")
		default:
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := exec.CommandContext(pctx, "nix", "store", "ping").Run()
			cancel()
			if err == nil {
				e.printf("preflight: nix store reachable (daemon or local store answers)\n")
			} else {
				e.printf("preflight: nix store NOT reachable\n")
				problems = append(problems, fmt.Sprintf("the nix store/daemon does not answer 'nix store ping': check that the daemon socket (/nix/var/nix/daemon-socket/socket) is mounted into this job and readable by uid %d", os.Getuid()))
			}
		}
	} else {
		switch {
		case nixBin != "":
			e.printf("preflight: nix is baked (%s); the nix installer will not run\n", nixBin)
		case nnp == "1":
			e.printf("preflight: nix is NOT baked and no_new_privs=1: the nix installer needs root and cannot run here\n")
			problems = append(problems, "nix is not on PATH and this runner cannot escalate privilege (no_new_privs=1), which the nix installer needs: use a runner image that bakes nix. The nix installer is the one root step in setup-devbox, and it exists for GitHub-hosted runners only")
		default:
			e.printf("preflight: nix is NOT baked: the install step will run the nix installer, which needs root. That is the one root step in this action, used only when nix is not baked (a GitHub-hosted runner); a runner that cannot escalate must bake nix into its image\n")
		}
	}

	if len(problems) > 0 {
		e.printf("::error::runner preflight failed: %s. This action runs without root or privilege escalation; see the setup-devbox notes in the README.\n", strings.Join(problems, "; "))
		return ErrFailed
	}
	e.printf("preflight: ok\n")
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// lookup is `${VAR-}` distinguishing unset from empty.
func (e Env) lookup(name string) (string, bool) {
	if f, ok := e.Getenv0(name); ok {
		return f, true
	}
	return "", false
}

func (e Env) lookupSet(name string) string { v, _ := e.lookup(name); return v }

// Getenv0 distinguishes an unset variable from an empty one when the process
// environment is the source; a test supplies its own Getenv and cannot, so an
// empty answer there means unset.
func (e Env) Getenv0(name string) (string, bool) {
	if e.Lookup != nil {
		return e.Lookup(name)
	}
	v := e.Getenv(name)
	return v, v != ""
}

func pathLookup(name, pathEnv string) string {
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// realpath is `readlink -f`, falling back to the path itself.
func realpath(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	if a, err := filepath.Abs(r); err == nil {
		return a
	}
	return r
}
