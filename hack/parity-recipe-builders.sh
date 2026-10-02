#!/usr/bin/env bash
# TEMPORARY, removed before merge: the inline-shell recipe and
# setup-remote-builders (the run bodies of the action.yaml files at the base the
# pull request branched from) against the Go port, scenario by scenario, with
# stand-ins for devbox, the task runners and docker. Compared: stdout, the exit
# status, the docker calls, GITHUB_ENV, the plugin link and the saved inspect
# output.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
trap '[ -n "${KEEP:-}" ] || rm -rf "$work"' EXIT
command -v yq >/dev/null || { echo "::error::yq is required"; exit 2; }
git -C "$here" show "$base:recipe/action.yaml" >"$work/recipe.yaml" || exit 2
git -C "$here" show "$base:setup-remote-builders/action.yaml" >"$work/rb.yaml" || exit 2
yq -r '.runs.steps[0].run' "$work/recipe.yaml" >"$work/recipe-old.sh"
yq -r '.runs.steps[0].run' "$work/rb.yaml" >"$work/rb-old.sh"
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2

stubs="$work/stubs"; mkdir -p "$stubs"
cat >"$stubs/devbox" <<'SH'
#!/usr/bin/env bash
echo "devbox $*" >>"${DOCKER_LOG:-/dev/null}"
[ "$1" = run ] && shift; [ "$1" = -- ] && shift
exec "$@"
SH
for tool in just moon; do cat >"$stubs/$tool" <<'SH'
#!/usr/bin/env bash
echo "$(basename "$0") $*"
case "$1" in
  fail) echo "boom" >&2; exit 2 ;;
  dirty) echo generated >generated.txt; echo changed >>tracked.txt ;;
  untracked) echo more >new-file.txt ;;
  ignored) echo x >ignored.log ;;
esac
SH
done
cat >"$stubs/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >>"$DOCKER_LOG"
case "$*" in
  "buildx rm ci") [ -z "${RM_FAILS:-}" ] || { echo "no such builder" >&2; exit 1; } ;;
  "buildx create"*) [ -z "${CREATE_FAILS:-}" ] || { echo "create failed" >&2; exit 3; } ;;
  "buildx inspect --bootstrap ci") printf 'Name: ci\nDriver: remote\nPlatforms: %s\n' "${OFFERS:-linux/amd64, linux/arm64}" ;;
esac
SH
chmod +x "$stubs"/*

fail=0; n=0
# scenario <kind: recipe|rb> <name> [VAR=value ...]
scenario() {
  local kind=$1 name=$2
  shift 2
  n=$((n + 1))
  local which
  for which in old new; do
    local d="$work/s$n-$which"
    mkdir -p "$d/ws" "$d/home" "$d/temp"
    : >"$d/github_env"; : >"$d/docker.log"
    (
      cd "$d/ws" || exit 1
      git init -q -b master . && printf 'ignored.log\n' >.gitignore && echo a >tracked.txt && git add -A \
        && git -c user.name=p -c user.email=p@example.invalid commit -q -m init
      export HOME="$d/home" RUNNER_TEMP="$d/temp" GITHUB_ENV="$d/github_env" DOCKER_LOG="$d/docker.log" PATH="$stubs:$PATH"
      for kv in "$@"; do export "${kv?}"; done
      [ -z "${DEVBOX_JSON:-}" ] || echo '{}' >devbox.json
      if [ "${PLUGIN:-}" = 1 ]; then mkdir -p "$d/plugin"; printf '#!/bin/sh\n' >"$d/plugin/docker-buildx"; chmod +x "$d/plugin/docker-buildx"; export PATH="$d/plugin:$PATH"; fi
      if [ $kind = recipe ]; then
        if [ $which = old ]; then bash --noprofile --norc -e -o pipefail "$work/recipe-old.sh" >"$d/stdout" 2>"$d/stderr"; else ACTION_PATH=x "$work/ci-actions" recipe >"$d/stdout" 2>"$d/stderr"; fi
      else
        if [ $which = old ]; then bash --noprofile --norc -e -o pipefail "$work/rb-old.sh" >"$d/stdout" 2>"$d/stderr"; else ACTION_PATH=x "$work/ci-actions" remote-builders >"$d/stdout" 2>"$d/stderr"; fi
      fi
      echo "rc=$?" >"$d/rc"
    )
    sed -E "s#$d#D#g" "$d/stdout" >"$d/stdout.norm"
    { cat "$d/docker.log"; echo "== env"; cat "$d/github_env"; echo "== link"; readlink "$d/home/.docker/cli-plugins/docker-buildx" 2>/dev/null | sed -E "s#$d#D#g" || true
      echo "== report"; cat "$d"/temp/buildx-*.txt 2>/dev/null; } | sed -E "s#$d#D#g" >"$d/state.norm"
  done
  local o="$work/s$n-old" w="$work/s$n-new" same=1 part
  for part in stdout.norm state.norm rc; do cmp -s "$o/$part" "$w/$part" || same=0; done
  if [ $same = 1 ]; then printf 'same  %s\n' "$name"; else
    printf 'DIFF  %s\n' "$name"
    for part in stdout.norm state.norm rc; do diff "$o/$part" "$w/$part" | head -12; done
    fail=1
  fi
}

# --- recipe ---
scenario recipe "a clean recipe" RECIPE=build COMMAND=just
scenario recipe "another task runner" RECIPE=lint COMMAND=moon
scenario recipe "a failing recipe" RECIPE=fail COMMAND=just
scenario recipe "a recipe that dirties the tree" RECIPE=dirty COMMAND=just
scenario recipe "a recipe that leaves an untracked file" RECIPE=untracked COMMAND=moon
scenario recipe "a gitignored output is not a change" RECIPE=ignored COMMAND=just
# --- setup-remote-builders ---
N1="linux/arm64=tcp://a:1234 linux/amd64=tcp://b:1234"
scenario rb "two nodes, no devbox" NODES="$N1" OFFERS="linux/amd64, linux/arm64"
scenario rb "two nodes through devbox" NODES="$N1" DEVBOX_JSON=1
scenario rb "one node with several platforms" NODES="linux/arm64,linux/amd64=tcp://one:1234" OFFERS="linux/amd64, linux/arm64"
scenario rb "the plugin is linked when found" NODES="linux/amd64=tcp://a:1" OFFERS="linux/amd64" PLUGIN=1
scenario rb "an absent builder to remove" NODES="linux/amd64=tcp://a:1" OFFERS="linux/amd64" RM_FAILS=1
scenario rb "empty nodes" NODES=""
scenario rb "only spaces" NODES="   "
scenario rb "a platform the builder does not offer" NODES="$N1" OFFERS="linux/arm64"
scenario rb "a registration that fails" NODES="$N1" CREATE_FAILS=1
scenario rb "newline and tab separated entries" NODES=$'linux/arm64=tcp://a:1\n\tlinux/amd64=tcp://b:1'
echo "$n scenarios"
exit $fail
