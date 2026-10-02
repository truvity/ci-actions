#!/usr/bin/env bash
# The thin tagged-pins wrapper: which binary it picks, and that it hands
# the input through and returns the binary's own status. The rule itself is
# tested in Go (internal/taggedpins); here the binary is a stub.
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
wrapper="$here/tagged-pins/run.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }
has() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }

mkdir -p "$work/ws" "$work/stubdir" "$work/tmp" "$work/min"
cat >"$work/stubdir/ci-actions" <<'STUB'
#!/usr/bin/env bash
echo "stub ran: $* LIBRARIES=${LIBRARIES-unset} cwd=${PWD##*/}"
exit "${STUB_EXIT:-0}"
STUB
chmod +x "$work/stubdir/ci-actions"

# A PATH with only what the wrapper's own shell needs: no go, no curl, no
# ci-actions.
for t in bash mkdir dirname; do ln -s "$(command -v "$t")" "$work/min/$t"; done

run() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/tagged-pins" "$@" bash "$wrapper" 2>&1); }

log=$(run PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" LIBRARIES="a/b c/d" STUB_EXIT=0)
rc=$?
[ $rc = 0 ] && has "$log" "stub ran: tagged-pins LIBRARIES=a/b c/d cwd=ws" \
  && ok "CI_ACTIONS_BIN is run as 'tagged-pins' in the caller's directory with the input" \
  || bad "CI_ACTIONS_BIN is run as 'tagged-pins' in the caller's directory with the input (rc=$rc): $log"

log=$(run PATH="$work/stubdir:$work/min" STUB_EXIT=7)
rc=$?
[ $rc = 7 ] && has "$log" "stub ran" && ok "a ci-actions on PATH is used and its exit status is the wrapper's" \
  || bad "a ci-actions on PATH is used and its exit status is the wrapper's (rc=$rc): $log"

log=$(run PATH="$work/min")
rc=$?
if [ $rc = 1 ] && has "$log" "::error::tagged-pins needs the ci-actions binary" && has "$log" "Go is not on PATH"; then
  ok "with no binary, no release and no Go it fails with one ::error:: saying what to do"
else
  bad "with no binary, no release and no Go it fails with one ::error:: (rc=$rc): $log"
fi

# Built from this checkout, when Go is available (always on a hosted
# runner). The workspace pins nothing, so the real binary says so.
if command -v go >/dev/null 2>&1; then
  gobin=$(dirname "$(command -v go)")
  # The build needs the module's one dependency, so it uses the caller's own
  # module and build caches rather than a fresh pair in the scratch dir.
  log=$(run PATH="$gobin:$work/min" GOFLAGS=-mod=mod GOCACHE="$(go env GOCACHE)" GOPATH="$(go env GOPATH)")
  rc=$?
  [ $rc = 0 ] && has "$log" "tagged-pins: 0 of 3 libraries had pins in this checkout, all naming releases" \
    && ok "without a binary or a release, Go builds it from the checkout and it runs" \
    || bad "without a binary or a release, Go builds it from the checkout (rc=$rc): $log"
else
  echo "skip  go build path: go is not on PATH"
fi

# The same resolver serves every wrapper: public-runners hands its own
# subcommand and inputs through, and names itself in the failure.
runpr() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/public-runners" "$@" bash "$here/public-runners/run.sh" 2>&1); }
log=$(runpr PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" RUNNERS="ubuntu-latest" STUB_EXIT=1)
rc=$?
[ $rc = 1 ] && has "$log" "stub ran: public-runners LIBRARIES=unset cwd=ws" \
  && ok "public-runners runs the binary as 'public-runners' and returns its status" \
  || bad "public-runners runs the binary as 'public-runners' and returns its status (rc=$rc): $log"
log=$(runpr PATH="$work/min")
rc=$?
[ $rc = 1 ] && has "$log" "::error::public-runners needs the ci-actions binary" \
  && ok "public-runners with no binary fails naming itself" \
  || bad "public-runners with no binary fails naming itself (rc=$rc): $log"

runfd() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/fleet-discover" "$@" bash "$here/fleet-discover/run.sh" 2>&1); }
log=$(runfd PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: fleet discover LIBRARIES=unset" \
  && ok "fleet-discover runs the binary as 'fleet discover'" \
  || bad "fleet-discover runs the binary as 'fleet discover': $log"
log=$(runfd PATH="$work/min")
has "$log" "::error::fleet-discover needs the ci-actions binary" \
  && ok "fleet-discover with no binary fails naming itself" \
  || bad "fleet-discover with no binary fails naming itself: $log"

runfp() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/devbox-parity" "$@" bash "$here/devbox-parity/run.sh" 2>&1); }
log=$(runfp PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: devbox-parity LIBRARIES=unset" \
  && ok "devbox-parity runs the binary as 'devbox-parity'" \
  || bad "devbox-parity runs the binary as 'devbox-parity': $log"
log=$(runfp PATH="$work/min")
has "$log" "::error::devbox-parity needs the ci-actions binary" \
  && ok "devbox-parity with no binary fails naming itself" \
  || bad "devbox-parity with no binary fails naming itself: $log"

runcp() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/caller-parity" "$@" bash "$here/caller-parity/run.sh" 2>&1); }
log=$(runcp PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: caller-parity LIBRARIES=unset" \
  && ok "caller-parity runs the binary as 'caller-parity'" \
  || bad "caller-parity runs the binary as 'caller-parity': $log"
log=$(runcp PATH="$work/min")
has "$log" "::error::caller-parity needs the ci-actions binary" \
  && ok "caller-parity with no binary fails naming itself" \
  || bad "caller-parity with no binary fails naming itself: $log"

runob() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/openbao-secrets" "$@" bash "$here/openbao-secrets/run.sh" 2>&1); }
log=$(runob PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: openbao-secrets LIBRARIES=unset" \
  && ok "openbao-secrets runs the binary as 'openbao-secrets'" \
  || bad "openbao-secrets runs the binary as 'openbao-secrets': $log"
log=$(runob PATH="$work/min")
has "$log" "::error::openbao-secrets needs the ci-actions binary" \
  && ok "openbao-secrets with no binary fails naming itself" \
  || bad "openbao-secrets with no binary fails naming itself: $log"

runcl() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/cluster" "$@" bash "$here/cluster/run.sh" "${CLUSTER_STEP:-fork-guard}" 2>&1); }
log=$(runcl PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: cluster fork-guard LIBRARIES=unset" \
  && ok "cluster runs the binary as 'cluster <step>'" \
  || bad "cluster runs the binary as 'cluster <step>': $log"
log=$(runcl PATH="$work/min")
has "$log" "::error::cluster needs the ci-actions binary" \
  && ok "cluster with no binary fails naming itself" \
  || bad "cluster with no binary fails naming itself: $log"
runpc() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/policy-conformance" "$@" bash "$here/policy-conformance/run.sh" 2>&1); }
log=$(runpc PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: policy-conformance LIBRARIES=unset cwd=ws" \
  && ok "policy-conformance runs the binary as 'policy-conformance' in the caller's directory" \
  || bad "policy-conformance runs the binary as 'policy-conformance': $log"
log=$(runpc PATH="$work/min")
has "$log" "::error::policy-conformance needs the ci-actions binary" \
  && ok "policy-conformance with no binary fails naming itself" \
  || bad "policy-conformance with no binary fails naming itself: $log"

runrc() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/recipe" "$@" bash "$here/recipe/run.sh" 2>&1); }
log=$(runrc PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: recipe LIBRARIES=unset" \
  && ok "recipe runs the binary as 'recipe'" \
  || bad "recipe runs the binary as 'recipe': $log"
log=$(runrc PATH="$work/min")
has "$log" "::error::recipe needs the ci-actions binary" \
  && ok "recipe with no binary fails naming itself" \
  || bad "recipe with no binary fails naming itself: $log"

runrb() { (cd "$work/ws" && env -i HOME="$work" RUNNER_TEMP="$work/tmp" ACTION_PATH="$here/setup-remote-builders" "$@" bash "$here/setup-remote-builders/run.sh" 2>&1); }
log=$(runrb PATH="$work/min" CI_ACTIONS_BIN="$work/stubdir/ci-actions" STUB_EXIT=0)
has "$log" "stub ran: remote-builders LIBRARIES=unset" \
  && ok "setup-remote-builders runs the binary as 'remote-builders'" \
  || bad "setup-remote-builders runs the binary as 'remote-builders': $log"
log=$(runrb PATH="$work/min")
has "$log" "::error::setup-remote-builders needs the ci-actions binary" \
  && ok "setup-remote-builders with no binary fails naming itself" \
  || bad "setup-remote-builders with no binary fails naming itself: $log"

echo
if [ "$fail" = 0 ]; then echo "all cases pass"; else echo "::error::tagged-pins wrapper cases failed"; fi
exit "$fail"
