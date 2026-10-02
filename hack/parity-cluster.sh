#!/usr/bin/env bash
# TEMPORARY, removed before merge: the shell cluster scripts (read from the
# base the pull request branched from) against the Go port, scenario by
# scenario, on identical fixture directories. Compared: stdout, the exit
# status (exactly), GITHUB_ENV, GITHUB_OUTPUT and the files the kind
# launch leaves under its state dir (outputs.env, log, exit-code).
#
# The box is a stand-in up.sh in a pre-seeded state dir (so nothing is
# fetched), docker and kubectl are stand-ins on PATH, and the real thing is
# proved end to end by the separate cluster-kind job.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
trap '[ -n "${KEEP:-}" ] || rm -rf "$work"' EXIT
mkdir -p "$work/old"
for f in emit fork-guard kind-launch kind-wait shared-connect shared-finish; do
  git -C "$here" show "$base:cluster/$f.sh" >"$work/old/$f.sh" || { echo "::error::cannot read cluster/$f.sh at $base"; exit 2; }
done
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2

stubs="$work/stubs"; mkdir -p "$stubs"
cat >"$stubs/docker" <<'SH'
#!/usr/bin/env bash
[ "$1" = ps ] || exit 2
[ -z "${DOCKER_FAIL:-}" ] || exit 1
echo "${DOCKER_PORTS-0.0.0.0:5001->5000/tcp}"
SH
cat >"$stubs/kubectl" <<'SH'
#!/usr/bin/env bash
n=$(cat "$KUBECTL_COUNT" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" >"$KUBECTL_COUNT"
if [ "$n" -le "${KUBECTL_FAILS:-0}" ]; then echo "Unable to connect (try $n)" >&2; exit 1; fi
echo "Info: banner before the json"
printf '{\n  "kind": "SelfSubjectReview",\n  "status": {"userInfo": {"username": "%s"}}\n}\n' "${KUBECTL_USER:-github:acme/app}"
SH
# the old scripts' sleeps would make the failure scenarios slow
cat >"$stubs/sleep" <<'SH'
#!/usr/bin/env bash
case "$1" in 5|10) exit 0 ;; esac
exec /bin/sleep "$@"
SH
chmod +x "$stubs"/*

fail=0
n=0
# scenario <name> <kind: old-script-name> <new-step> [VAR=value ...]
#   MODE=bg runs kind in the background and then wait, as two invocations.
scenario() {
  local name=$1 script=$2 step=$3
  shift 3
  n=$((n + 1))
  local which
  for which in old new; do
    local d="$work/s$n-$which"
    mkdir -p "$d/temp" "$d/ws/.kube"
    : >"$d/github_env"; : >"$d/github_output"
    printf 'apiVersion: v1\n' >"$d/ws/.kube/config"; printf '[default]\n' >"$d/ws/aws.ini"
    printf '{"pull_request":{"head":{"repo":{"fork":true}}}}' >"$d/event-fork.json"
    printf '{"pull_request":{"head":{"repo":{"fork":false}}}}' >"$d/event-same.json"
    printf '<<<' >"$d/event-bad.json"
    # a pre-seeded box, so nothing is fetched
    local box="$d/temp/cluster-action/policy-kind-box"
    mkdir -p "$box"
    cat >"$box/up.sh" <<'SH'
#!/usr/bin/env bash
echo "up: starting (KUBECONFIG=${KUBECONFIG##*/})"
sleep "${BOX_SLEEP:-0}"
echo "up: done"
exit "${BOX_EXIT:-0}"
SH
    chmod +x "$box/up.sh"
    (
      export GITHUB_ENV="$d/github_env" GITHUB_OUTPUT="$d/github_output" RUNNER_TEMP="$d/temp" GITHUB_WORKSPACE="$d/ws"
      export KUBECTL_COUNT="$d/kubectl-count" PATH="$stubs:$PATH"
      for kv in "$@"; do export "${kv//@D@/$d}"; done
      cd "$d" || exit 1
      run1() { # old script or new step
        if [ $which = old ]; then bash "$work/old/$script.sh"; else "$work/ci-actions" cluster "$step"; fi
      }
      if [ "${LAUNCH:-}" = both ]; then
        # background launch, then wait, as two steps
        step=kind script=kind-launch BACKGROUND=true run1 >"$d/stdout" 2>"$d/stderr"; echo "rc1=$?" >"$d/rc"
        if [ $which = old ]; then script=kind-wait; else step=wait; fi
        run1 >>"$d/stdout" 2>>"$d/stderr"; echo "rc2=$?" >>"$d/rc"
      else
        run1 >"$d/stdout" 2>"$d/stderr"; echo "rc=$?" >"$d/rc"
      fi
      # jq exits 5 on a payload that is not JSON, the port exits 1: both refuse
      [ -z "${RC_LOOSE:-}" ] || sed -i -E 's/rc=[1-9][0-9]*/rc=nonzero/' "$d/rc"
    )
    sed -E -e "s#$d#D#g" "$d/stdout" >"$d/stdout.norm"
    sed -E -e "s#$d#D#g" "$d/github_env" >"$d/env.norm"
    sed -E -e "s#$d#D#g" "$d/github_output" >"$d/output.norm"
    { for f in outputs.env log exit-code kubeconfig; do [ -e "$d/temp/cluster-action/$f" ] && { echo "== $f"; sed -E "s#$d#D#g" "$d/temp/cluster-action/$f"; }; done; } >"$d/state.norm" 2>/dev/null
    sed -E -e "s#$d#D#g" "$d/stderr" >"$d/stderr.norm"
  done
  local o="$work/s$n-old" w="$work/s$n-new" same=1 part
  for part in stdout.norm env.norm output.norm state.norm rc; do cmp -s "$o/$part" "$w/$part" || same=0; done
  if [ $same = 1 ]; then printf 'same  %s\n' "$name"; else
    printf 'DIFF  %s\n' "$name"
    for part in stdout.norm env.norm output.norm state.norm rc; do diff "$o/$part" "$w/$part" | head -20; done
    fail=1
  fi
}

# --- fork guard ---
scenario "fork pull request refused" fork-guard fork-guard GITHUB_EVENT_PATH=@D@/event-fork.json
scenario "same-repository pull request" fork-guard fork-guard GITHUB_EVENT_PATH=@D@/event-same.json
scenario "no event payload file" fork-guard fork-guard GITHUB_EVENT_PATH=@D@/nope.json
scenario "event path unset" fork-guard fork-guard GITHUB_EVENT_PATH=
scenario "payload that is not JSON (both refuse)" fork-guard fork-guard GITHUB_EVENT_PATH=@D@/event-bad.json RC_LOOSE=1

# --- kind, in the foreground ---
scenario "kind: box comes up" kind-launch kind POLICY_VERSION=v0.8.1 NAMESPACE=e2e-test GITHUB_JOB=integration GITHUB_RUN_ID=77 GITHUB_RUN_ATTEMPT=2
scenario "kind: defaults for namespace and release" kind-launch kind POLICY_VERSION=v0.8.1
scenario "kind: explicit release" kind-launch kind POLICY_VERSION=v0.8.1 RELEASE=my-release
scenario "kind: the box fails with exit 3" kind-launch kind POLICY_VERSION=v0.8.1 BOX_EXIT=3
scenario "kind: no registry on 5001" kind-launch kind POLICY_VERSION=v0.8.1 DOCKER_PORTS=0.0.0.0:5002-\>5000/tcp
scenario "kind: docker cannot answer" kind-launch kind POLICY_VERSION=v0.8.1 DOCKER_FAIL=1
scenario "kind: policy-version missing" kind-launch kind POLICY_VERSION=
scenario "kind: a state-dir of its own" kind-launch kind POLICY_VERSION=v0.8.1 STATE_DIR=@D@/temp/cluster-action

# --- kind in the background, then wait ---
scenario "background kind, then wait" kind-launch kind POLICY_VERSION=v0.8.1 LAUNCH=both BOX_SLEEP=1
scenario "background kind that fails, then wait" kind-launch kind POLICY_VERSION=v0.8.1 LAUNCH=both BOX_SLEEP=1 BOX_EXIT=5
scenario "wait with nothing launched" kind-wait wait

# --- shared ---
IDENT="EXPECTED_IDENTITY=github:acme/app KUBECONFIG_INPUT=.kube/config AWS_CONFIG_FILE_INPUT=aws.ini"
scenario "shared-connect: identity matches" shared-connect shared-connect $IDENT
scenario "shared-connect: identity differs" shared-connect shared-connect $IDENT KUBECTL_USER=github:acme/other
scenario "shared-connect: works on the second try" shared-connect shared-connect $IDENT KUBECTL_FAILS=1
scenario "shared-connect: fails three times" shared-connect shared-connect $IDENT KUBECTL_FAILS=3
scenario "shared-connect: no expected identity" shared-connect shared-connect KUBECONFIG_INPUT=.kube/config AWS_CONFIG_FILE_INPUT=aws.ini
scenario "shared-connect: kubeconfig input missing" shared-connect shared-connect AWS_CONFIG_FILE_INPUT=aws.ini
scenario "shared-connect: aws input missing" shared-connect shared-connect KUBECONFIG_INPUT=.kube/config
scenario "shared-connect: kubeconfig not found" shared-connect shared-connect KUBECONFIG_INPUT=nope AWS_CONFIG_FILE_INPUT=aws.ini
scenario "shared-connect: aws config not found" shared-connect shared-connect KUBECONFIG_INPUT=.kube/config AWS_CONFIG_FILE_INPUT=nope
scenario "shared-finish: one registry" shared-finish shared-finish KUBECONFIG=/w/kc ECR_REGISTRY=111.dkr.ecr.eu.amazonaws.com NAMESPACE=ns1 RELEASE=rel1
scenario "shared-finish: several registries, defaults" shared-finish shared-finish KUBECONFIG=/w/kc ECR_REGISTRY=111.dkr,222.dkr GITHUB_JOB=lane GITHUB_RUN_ID=5 GITHUB_RUN_ATTEMPT=3
scenario "shared-finish: no registry" shared-finish shared-finish KUBECONFIG=/w/kc
echo "$n scenarios"
exit $fail
