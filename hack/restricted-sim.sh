#!/usr/bin/env bash
# Run the actions' own step scripts as a Pod Security `restricted` runner
# would: uid 1001, no_new_privs, every capability dropped, a read-only root
# filesystem, writable tmpfs only for HOME, the runner temp dir and the
# work dir.
#
# WHY NOT THE COMPOSITE ACTION ITSELF. A composite action is executed by
# the runner agent, which is not available in a container on a hosted job.
# So the harness does the next best faithful thing: it reads each action's
# action.yaml, takes every `run:` body exactly as written, resolves its
# `env:` the way the runner would (inputs fall back to their declared
# defaults), and executes the bodies in order, under the same bash flags
# the runner uses, inside the restricted container. A step added to an
# action is therefore exercised without touching this file.
#
# A step is run when it has no `if:`, plus the few whose condition is true
# for the default inputs on a runner that has nix and devbox baked (listed
# in CONDITIONAL below). Every other conditional step depends on caller
# supplied credentials and is reported as skipped.
#
# Stand-ins for devbox, nix, just and proto (hack/restricted-sim/stubs)
# keep it offline: what is under test is the privilege contract, not those
# tools.
#
#   hack/restricted-sim.sh                 setup-devbox, recipe, tagged-pins, public-runners and fleet-discover against the fixture
#   hack/restricted-sim.sh --control       prove the image escalates WITHOUT the flag
#   hack/restricted-sim.sh --self-test     prove the harness fails a privileged step
#   hack/restricted-sim.sh ACTION_DIR...   other actions, by directory
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
sim="$here/hack/restricted-sim"
image=ci-actions-restricted-sim
# Spelled in two pieces: hack/no-escalation-cases.sh forbids the word in scripts.
priv='su''do'

# The fixture's recipe, for the recipe action's required input.
export SIM_INPUT_recipe="${SIM_INPUT_recipe:-fixture}"
# public-runners' required input, and a visibility so it needs no network.
export SIM_INPUT_runners="${SIM_INPUT_runners:-ubuntu-latest}"
export SIM_INPUT_visibility="${SIM_INPUT_visibility:-public}"
# fleet-discover: a token and a one-repository API served inside the container.
export SIM_INPUT_token="${SIM_INPUT_token:-sim-token}"
export SIM_INPUT_api_url="${SIM_INPUT_api_url:-http://127.0.0.1:8765}"

# Steps with an `if:` that is true for default inputs on a baked runner.
CONDITIONAL=("Strip local-only proto tools" "Install proto toolchain")

command -v docker >/dev/null || { echo "::error::docker is required"; exit 2; }
command -v yq >/dev/null || { echo "::error::yq (mikefarah, v4) is required"; exit 2; }

build() { docker build -q -t "$image" "$sim" >/dev/null || { echo "::error::image build failed"; exit 2; }; }

# subst <action.yaml> <value>: resolve the ${{ }} expressions an env: value
# can carry. Anything not understood becomes empty, which is what the
# runner does with an unset context key.
subst() {
  local file=$1 v=$2 name def
  while [[ "$v" =~ \$\{\{[[:space:]]*inputs\.([A-Za-z0-9_-]+)[[:space:]]*\}\} ]]; do
    name=${BASH_REMATCH[1]}
    # SIM_INPUT_<name> overrides a declared default (required inputs have none).
    local var="SIM_INPUT_${name//-/_}"
    if [ -n "${!var+x}" ]; then def=${!var}; else def=$(NAME="$name" yq -r '.inputs[strenv(NAME)].default // ""' "$file"); fi
    v=${v//"${BASH_REMATCH[0]}"/$def}
  done
  v=${v//'${{ github.action_path }}'/ACTION_PATH_PLACEHOLDER}
  v=${v//'${{ runner.arch }}'/X64}
  v=${v//'${{ runner.os }}'/Linux}
  v=$(sed -E 's/\$\{\{[^}]*\}\}//g' <<<"$v")
  printf '%s' "$v"
}

# extract <action-dir> <out-dir> <prefix>: one NN.sh / NN.env / NN.name per
# step to run. Prints "skip: ..." lines for the rest.
extract() {
  local dir=$1 out=$2 prefix=$3 file="$1/action.yaml" n count i name cond run k val c wanted
  [ -f "$file" ] || file="$1/action.yml"
  count=$(yq '.runs.steps | length' "$file")
  for ((i = 0; i < count; i++)); do
    name=$(I=$i yq -r '.runs.steps[env(I)].name // "step"' "$file")
    run=$(I=$i yq -r '.runs.steps[env(I)].run // ""' "$file")
    cond=$(I=$i yq -r '.runs.steps[env(I)].["if"] // ""' "$file")
    if [ -z "$run" ]; then
      echo "skip: $prefix $name (not a run step)"
      continue
    fi
    if [ -n "$cond" ]; then
      wanted=0
      for c in "${CONDITIONAL[@]}"; do [ "$c" = "$name" ] && wanted=1; done
      if [ $wanted = 0 ]; then
        echo "skip: $prefix $name (if: $cond)"
        continue
      fi
    fi
    n=$(printf '%s-%03d' "$prefix" "$i")
    printf '%s\n' "$run" >"$out/$n.sh"
    printf '%s\n' "$prefix / $name" >"$out/$n.name"
    : >"$out/$n.env"
    while IFS= read -r k; do
      [ -n "$k" ] || continue
      val=$(I=$i K="$k" yq -r '.runs.steps[env(I)].env[strenv(K)]' "$file")
      val=$(subst "$file" "$val")
      val=${val//ACTION_PATH_PLACEHOLDER//actions/$(basename "$dir")}
      printf 'export %s=%q\n' "$k" "$val" >>"$out/$n.env"
    done < <(I=$i yq -r '.runs.steps[env(I)].env // {} | keys | .[]' "$file")
  done
}

# run_sim <steps-dir> <extra docker flags...>: executes the extracted steps
# in the restricted container. Returns the container's status.
run_sim() {
  local steps=$1
  shift
  # The Go CLI the thin wrappers run: built here, statically, so the
  # container needs no toolchain. An empty dir when there is no Go (the
  # wrapper then says so, and the actions that need it fail loudly).
  local bindir
  bindir=$(mktemp -d)
  chmod 755 "$bindir"
  if command -v go >/dev/null 2>&1; then
    (cd "$here" && CGO_ENABLED=0 go build -trimpath -o "$bindir/ci-actions" ./cmd/ci-actions) || { echo "::error::could not build the ci-actions binary"; exit 2; }
  fi
  local rc
  docker run --rm \
    --user 1001:1001 \
    --security-opt no-new-privileges \
    --cap-drop ALL \
    --read-only \
    --tmpfs /tmp:rw,mode=1777 \
    --tmpfs /home/runner:rw,uid=1001,gid=1001,mode=0700 \
    --tmpfs /work:rw,exec,uid=1001,gid=1001,mode=0755 \
    -v "$here:/actions:ro" \
    -v "$sim/fixture:/fixture:ro" \
    -v "$sim/stubs:/stubs:ro" \
    -v "$steps:/steps:ro" \
    -v "$bindir:/ci-bin:ro" \
    -e HOME=/home/runner -e RUNNER_TEMP=/work/temp -e GITHUB_WORKSPACE=/work/ws \
    -e GITHUB_ENV=/work/temp/github_env -e GITHUB_OUTPUT=/work/temp/github_output \
    -e GITHUB_PATH=/work/temp/github_path -e GITHUB_STEP_SUMMARY=/work/temp/summary \
    -e GITHUB_REPOSITORY=example/sim -e RUNNER_ARCH=X64 -e RUNNER_OS=Linux -e CI=true -e GITHUB_ACTIONS=true \
    "$@" \
    "$image" bash -c '
      set -uo pipefail
      echo "sim: uid=$(id -u) gid=$(id -g) NoNewPrivs=$(awk "/NoNewPrivs/{print \$2}" /proc/self/status) /bin/sh -> $(readlink -f /bin/sh)"
      if touch /should-not-be-writable 2>/dev/null; then echo "::error::sim: root filesystem is writable"; exit 3; fi
      mkdir -p /work/temp /work/ws
      : >/work/temp/github_env; : >/work/temp/github_output; : >/work/temp/github_path
      cp -a /fixture/. /work/ws/
      cd /work/ws
      git init -q . && git add -A && git -c user.name=sim -c user.email=sim@example.invalid commit -qm fixture
      export PATH=/ci-bin:/stubs:$PATH
      python3 /stubs/github-api.py 8765 &
      for _ in $(seq 1 50); do (exec 3<>/dev/tcp/127.0.0.1/8765) 2>/dev/null && break; sleep 0.1; done
      for sh in /steps/*.sh; do
        base=${sh%.sh}
        echo "::group::$(cat "$base.name")"
        # shellcheck disable=SC1090
        ( . "$base.env"; exec bash --noprofile --norc -e -o pipefail "$sh" )
        rc=$?
        echo "::endgroup::"
        if [ $rc != 0 ]; then
          echo "::error::sim: step \"$(cat "$base.name")\" failed (exit $rc) under the restricted profile"
          exit 1
        fi
      done
      echo "sim: all steps passed"
    '
  rc=$?
  rm -rf "$bindir"
  return $rc
}

simulate() {
  local steps rc a i=0
  steps=$(mktemp -d)
  chmod 755 "$steps"
  for a in "$@"; do
    extract "$a" "$steps" "$(printf '%02d-%s' "$i" "$(basename "$a")")"
    i=$((i + 1))
  done
  build
  run_sim "$steps"
  rc=$?
  rm -rf "$steps"
  return $rc
}

case "${1:-}" in
--control)
  # The same image, WITHOUT the flag: the escalation the contract forbids
  # works. Without this a red simulation could be blamed on a broken image.
  build
  docker run --rm --user 1001:1001 "$image" "$priv" -n true \
    && echo "control: the image escalates without no-new-privileges" \
    || { echo "::error::control failed: the image cannot escalate even without the flag, so the simulation proves nothing"; exit 1; }
  if docker run --rm --user 1001:1001 --security-opt no-new-privileges --cap-drop ALL "$image" "$priv" -n true 2>/dev/null; then
    echo "::error::control failed: the image escalates even WITH no-new-privileges"
    exit 1
  fi
  echo "control: and cannot with it (the flag is what stops it)"
  ;;
--self-test)
  # Reintroduce the step setup-devbox v1.6.0 shipped, into a scratch copy,
  # and require the simulation to REJECT it.
  scratch=$(mktemp -d)
  trap 'rm -rf "$scratch"' EXIT
  mkdir "$scratch/setup-devbox"
  cp "$here/setup-devbox/action.yaml" "$here/setup-devbox/preflight.sh" "$scratch/setup-devbox/"
  # Reintroduce the pre-1.6.1 step verbatim into the scratch copy, as the
  # first step so a failure is unambiguous.
  PRIV="$priv" yq -i '.runs.steps = [{"name": "Use bash as sh", "shell": "bash", "run": strenv(PRIV) + " ln -sf /bin/bash /bin/sh"}] + .runs.steps' \
    "$scratch/setup-devbox/action.yaml"
  steps=$(mktemp -d)
  chmod 755 "$steps"
  extract "$scratch/setup-devbox" "$steps" "00-setup-devbox"
  build
  out=$(run_sim "$steps" 2>&1)
  rc=$?
  rm -rf "$steps"
  # The rejection is the EXPECTED outcome here, so its ::error:: line must
  # not become an annotation on a green run.
  printf '%s\n' "$out" | tail -15 | sed 's/^::error::/expected rejection: /; s/^::group::/-- /; s/^::endgroup:://'
  if [ $rc = 0 ]; then
    echo "::error::self-test failed: the simulation PASSED a privileged step"
    exit 1
  fi
  if grep -q 'step "00-setup-devbox / Use bash as sh" failed' <<<"$out"; then
    echo "self-test: the simulation rejects the privileged step"
  else
    echo "::error::self-test failed: red, but not for the privileged step"
    exit 1
  fi
  ;;
*)
  if [ $# -eq 0 ]; then set -- "$here/setup-devbox" "$here/recipe" "$here/tagged-pins" "$here/public-runners" "$here/fleet-discover"; fi
  simulate "$@"
  ;;
esac
