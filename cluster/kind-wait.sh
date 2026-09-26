#!/usr/bin/env bash
# mode: wait. Blocks on a `mode: kind, background: true` launch started
# earlier in the SAME job, then re-emits the same five outputs.
#
# A background launch and its wait are two separate invocations of this
# action — two separate steps, two separate shells, neither with any
# memory of the other's inputs. What connects them is state-dir: the
# launch writes its pid, its log and the resolved outputs under it, and
# wait takes no inputs of its own beyond that.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

: "${GITHUB_ENV:?}" "${GITHUB_OUTPUT:?}"
# shellcheck source=cluster/emit.sh
source "$here/emit.sh"

state_dir=${STATE_DIR:-}
[ -n "$state_dir" ] || state_dir="${RUNNER_TEMP:?RUNNER_TEMP is not set}/cluster-action"

pid_file="$state_dir/pid"
exit_file="$state_dir/exit-code"
log="$state_dir/log"
outputs_file="$state_dir/outputs.env"

if [ ! -f "$pid_file" ]; then
  echo "::error::no background kind launch found under $state_dir — call this action with mode: kind and background: true first (same state-dir input, if you set one)"
  exit 1
fi

pid=$(cat "$pid_file")

# 20 minutes: the box itself measures 3-4 on a hosted runner with nothing
# cached (hack/kind/README.md), so this is generous headroom rather than
# a number tuned to the happy path.
deadline=$((SECONDS + 1200))
while kill -0 "$pid" 2>/dev/null; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "::error::kind box did not finish within 20 minutes (pid $pid still running) — log:"
    tail -n 200 "$log" 2>/dev/null || true
    exit 1
  fi
  sleep 5
done

# The process is gone; give the exit-code file an instant to land (it is
# written by the same background wrapper right after the process exits,
# not by the process itself, so there is a small window rather than a
# guarantee of simultaneity).
for _ in 1 2 3 4 5; do
  [ -f "$exit_file" ] && break
  sleep 1
done

code=$(cat "$exit_file" 2>/dev/null || echo unknown)
if [ "$code" != 0 ]; then
  echo "::error::the kind box failed to come up (exit ${code}) — log:"
  tail -n 200 "$log" 2>/dev/null || true
  exit 1
fi

emit_from_file "$outputs_file"
echo "kind box ready"
