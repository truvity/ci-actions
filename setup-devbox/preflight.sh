#!/usr/bin/env bash
# What the rest of setup-devbox needs from the machine it runs on, checked
# FIRST and said out loud.
#
# This action runs with no privilege by contract: no root, no privilege escalation, no
# relinking of /bin/sh. A runner under the Pod Security `restricted`
# profile (uid 1001, no_new_privs, every capability dropped) is the
# reference environment. When that contract is not met the failure used to
# surface a minute later as a cryptic error from nix or devbox; here it is
# one `::error::` naming what is missing and what to do about it.
#
# Facts are printed always. Only conditions the rest of the action cannot
# work without fail the step:
#   - HOME, RUNNER_TEMP or the work directory is not writable
#   - nix is expected and its daemon/store does not answer
#   - nix is absent AND the runner cannot escalate privilege: the install
#     step would run the nix installer, which needs root
# Not failures, because the action no longer depends on them:
#   - no_new_privs set        (escalation cannot work; nothing here asks for it)
#   - /bin/sh is not bash     (steps say `shell: bash` explicitly)
#
# THE ONE ROOT STEP. Everything else here is privilege-free (devbox is the
# checksum-verified release binary in $RUNNER_TEMP/bin). The exception is the
# nix installer, which needs root and runs ONLY when nix is not baked into the
# runner image: that is a GitHub-hosted runner, which has root. A runner that
# is restricted (no_new_privs) and has no nix cannot be made to work, and this
# says so here, in one line, instead of a minute later in the installer.
#
# The probe locations can be redirected through PREFLIGHT_* variables so
# hack/preflight-cases.sh can exercise every branch without being root.
set -uo pipefail

proc_status="${PREFLIGHT_PROC_STATUS:-/proc/self/status}"
sh_path="${PREFLIGHT_SH_PATH:-/bin/sh}"
expect_nix="${PREFLIGHT_EXPECT_NIX:-auto}" # auto | true | false
workdir="${GITHUB_WORKSPACE:-$PWD}"
# Where nix and devbox are, "" for absent. Unset means ask PATH.
nix_bin="${PREFLIGHT_NIX_BIN-$(command -v nix 2>/dev/null || true)}"
devbox_bin="${PREFLIGHT_DEVBOX_BIN-$(command -v devbox 2>/dev/null || true)}"

problems=()

echo "preflight: uid=$(id -u) gid=$(id -g) user=$(id -un 2>/dev/null || echo '?')"

# no_new_privs: informational. Set means setuid binaries cannot
# raise privilege, which is the point of the restricted profile.
nnp=$(awk '$1 == "NoNewPrivs:" { print $2 }' "$proc_status" 2>/dev/null || true)
case "$nnp" in
1) echo "preflight: no_new_privs=1 (privilege escalation is off; this action never asks for it)" ;;
0) echo "preflight: no_new_privs=0 (privilege escalation is possible; this action does not use it)" ;;
*) echo "preflight: no_new_privs=unknown (could not read NoNewPrivs from $proc_status)" ;;
esac

# /bin/sh: informational. Steps declare `shell: bash` themselves.
sh_real=$(readlink -f "$sh_path" 2>/dev/null || echo "$sh_path")
bash_real=$(readlink -f "$(command -v bash 2>/dev/null || echo /bin/bash)" 2>/dev/null || true)
if [ -n "$bash_real" ] && [ "$sh_real" = "$bash_real" ]; then
  echo "preflight: $sh_path -> $sh_real (bash)"
else
  echo "preflight: $sh_path -> $sh_real (not bash; steps use 'shell: bash', recipes that need bash as sh must say so themselves)"
fi

# Writable means a file can actually be created there: a read-only mount
# passes a mode-bit test and fails a write.
writable() {
  local label=$1 dir=$2 probe
  if [ -z "$dir" ]; then
    echo "preflight: $label is not set"
    problems+=("$label is not set")
    return
  fi
  if [ -d "$dir" ] && probe=$(mktemp "$dir/.preflight.XXXXXX" 2>/dev/null); then
    rm -f "$probe"
    echo "preflight: $label=$dir writable"
  else
    echo "preflight: $label=$dir NOT writable"
    problems+=("$label ($dir) is not writable: mount a writable volume there")
  fi
}

writable HOME "${HOME:-}"
writable RUNNER_TEMP "${RUNNER_TEMP:-}"
writable workdir "$workdir"

# devbox: baked into the image, or installed by this action from the release
# binary, checksum-verified, into $RUNNER_TEMP/bin. Neither needs privilege.
if [ -n "$devbox_bin" ]; then
  echo "preflight: devbox is baked ($devbox_bin); nothing to install"
else
  echo "preflight: devbox is not baked; the install step will fetch the release binary into \$RUNNER_TEMP/bin (checksum-verified, no privilege)"
fi

# nix: expected when it is already on PATH (a runner image that bakes it)
# or the caller says so. A hosted runner has none before the install step.
if [ "$expect_nix" = auto ]; then
  if [ -n "$nix_bin" ]; then expect_nix=true; else expect_nix=false; fi
fi
if [ "$expect_nix" = true ]; then
  if [ -z "$nix_bin" ]; then
    echo "preflight: nix expected but not on PATH"
    problems+=("nix is expected but not on PATH: use a runner image that bakes nix, or unset the expectation")
  elif timeout 30 nix store ping >/dev/null 2>&1; then
    echo "preflight: nix store reachable (daemon or local store answers)"
  else
    echo "preflight: nix store NOT reachable"
    problems+=("the nix store/daemon does not answer 'nix store ping': check that the daemon socket (/nix/var/nix/daemon-socket/socket) is mounted into this job and readable by uid $(id -u)")
  fi
else
  if [ -n "$nix_bin" ]; then
    echo "preflight: nix is baked ($nix_bin); the nix installer will not run"
  elif [ "$nnp" = 1 ]; then
    echo "preflight: nix is NOT baked and no_new_privs=1: the nix installer needs root and cannot run here"
    problems+=("nix is not on PATH and this runner cannot escalate privilege (no_new_privs=1), which the nix installer needs: use a runner image that bakes nix. The nix installer is the one root step in setup-devbox, and it exists for GitHub-hosted runners only")
  else
    echo "preflight: nix is NOT baked: the install step will run the nix installer, which needs root. That is the one root step in this action, used only when nix is not baked (a GitHub-hosted runner); a runner that cannot escalate must bake nix into its image"
  fi
fi

if [ ${#problems[@]} -gt 0 ]; then
  joined=$(printf '%s; ' "${problems[@]}")
  echo "::error::runner preflight failed: ${joined%; }. This action runs without root or privilege escalation; see the setup-devbox notes in the README."
  exit 1
fi

echo "preflight: ok"
