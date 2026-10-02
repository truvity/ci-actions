#!/usr/bin/env bash
# setup-devbox's preflight, one branch at a time.
#
# A preflight that prints "ok" everywhere cannot be told apart from one
# that checks nothing, so each failure it is meant to catch is produced
# here and the verdict and the message are both read. No root needed: the
# probe locations are redirected through PREFLIGHT_* variables and the
# unwritable directory is a read-only mount-free stand-in (a directory
# that does not exist).
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/setup-devbox/preflight.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }

mkdir -p "$work/home" "$work/temp" "$work/ws" "$work/bin"
printf 'Name:\tx\nNoNewPrivs:\t1\n' >"$work/status-nnp1"
printf 'Name:\tx\nNoNewPrivs:\t0\n' >"$work/status-nnp0"
ln -s "$(command -v bash)" "$work/sh-is-bash"
ln -s "$(command -v dash || command -v busybox || echo /bin/true)" "$work/sh-is-dash"
printf '#!/bin/sh\nexit 0\n' >"$work/bin/nix-ok"
printf '#!/bin/sh\nexit 1\n' >"$work/bin/nix-dead"
chmod +x "$work/bin/"*

# run <extra env...> -- runs the script with a clean, writable baseline.
run() {
  env -i PATH="$PATH" HOME="$work/home" RUNNER_TEMP="$work/temp" GITHUB_WORKSPACE="$work/ws" \
    PREFLIGHT_PROC_STATUS="$work/status-nnp1" PREFLIGHT_SH_PATH="$work/sh-is-bash" \
    PREFLIGHT_EXPECT_NIX=false "$@" bash "$script" 2>&1
}

has() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }

log=$(run)
rc=$?
[ $rc = 0 ] && ok "a restricted-shaped environment passes" || bad "a restricted-shaped environment passes: $log"
has "$log" "no_new_privs=1" && ok "it reports no_new_privs" || bad "it reports no_new_privs: $log"
has "$log" "uid=" && ok "it reports uid and gid" || bad "it reports uid and gid: $log"
has "$log" "(bash)" && ok "it reports sh resolving to bash" || bad "it reports sh resolving to bash: $log"

log=$(run PREFLIGHT_SH_PATH="$work/sh-is-dash" PREFLIGHT_PROC_STATUS="$work/status-nnp0")
rc=$?
[ $rc = 0 ] && ok "sh not being bash, or no_new_privs unset, is not a failure" || bad "sh not being bash is not a failure: $log"
has "$log" "not bash" && ok "it says sh is not bash" || bad "it says sh is not bash: $log"
has "$log" "no_new_privs=0" && ok "it reports no_new_privs=0" || bad "it reports no_new_privs=0: $log"

for var in HOME RUNNER_TEMP GITHUB_WORKSPACE; do
  log=$(run "$var=$work/does-not-exist")
  rc=$?
  label=$var
  [ "$var" = GITHUB_WORKSPACE ] && label=workdir
  if [ $rc != 0 ] && [ "$(grep -c '^::error::' <<<"$log")" = 1 ] && has "$log" "$label"; then
    ok "an unwritable $label fails with ONE ::error:: naming it"
  else
    bad "an unwritable $label fails with ONE ::error:: naming it (rc=$rc): $log"
  fi
done

# Several problems still make ONE error line.
log=$(run HOME="$work/nope" RUNNER_TEMP="$work/nope2")
n=$(grep -c '^::error::' <<<"$log")
if [ "$n" = 1 ] && has "$log" "HOME" && has "$log" "RUNNER_TEMP"; then
  ok "several problems are reported in a single ::error::"
else
  bad "several problems are reported in a single ::error:: (got $n): $log"
fi

mkdir "$work/bin-ok" "$work/bin-dead"
cp "$work/bin/nix-ok" "$work/bin-ok/nix"
cp "$work/bin/nix-dead" "$work/bin-dead/nix"
log=$(run PREFLIGHT_EXPECT_NIX=true PATH="$work/bin-ok:$PATH")
[ $? = 0 ] && has "$log" "nix store reachable" && ok "a reachable nix passes" || bad "a reachable nix passes: $log"
log=$(run PREFLIGHT_EXPECT_NIX=true PATH="$work/bin-dead:$PATH")
if [ $? != 0 ] && has "$log" "daemon-socket" && [ "$(grep -c '^::error::' <<<"$log")" = 1 ]; then
  ok "an unreachable nix daemon fails with one error naming the socket"
else
  bad "an unreachable nix daemon fails with one error naming the socket: $log"
fi
log=$(run PREFLIGHT_EXPECT_NIX=auto PATH="$work/bin-dead:$PATH")
[ $? != 0 ] && ok "auto expects nix when it is on PATH" || bad "auto expects nix when it is on PATH: $log"

echo
if [ "$fail" = 0 ]; then echo "all cases pass"; else echo "::error::preflight cases failed"; fi
exit "$fail"
