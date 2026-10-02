#!/usr/bin/env bash
# TEMPORARY, removed before merge: the inline-shell devbox-parity (the run
# bodies of the action.yaml at the base the pull request branched from)
# against the Go port, on the same fixture repositories with stand-ins for
# devbox, gh, golangci-lint, yarn and go.dev. Compared per scenario: the log
# (minus ::group:: lines), the stub call log, the step outputs, and the
# resulting git state (branch contents, author config, the pushed ref).
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
server=""
trap '[ -n "$server" ] && kill "$server" 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$work"; [ -z "${KEEP:-}" ] || echo "kept $work"' EXIT
command -v yq >/dev/null || { echo "::error::yq is required"; exit 2; }
git -C "$here" show "$base:devbox-parity/action.yaml" >"$work/old-action.yaml" || { echo "::error::cannot read the old action at $base"; exit 2; }
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2

# ---- stand-ins ----------------------------------------------------------
stubs="$work/stubs"
mkdir -p "$stubs"
cat >"$stubs/devbox" <<'SH'
#!/usr/bin/env bash
# devbox update: move devbox.lock. devbox run -- cmd...: run cmd.
case "$1" in
  update) printf '\n' >>devbox.lock; echo "devbox: updated" ;;
  run) shift; [ "$1" = "--" ] && shift; exec "$@" ;;
  *) echo "devbox stub: $*" >&2; exit 2 ;;
esac
SH
cat >"$stubs/golangci-lint" <<'SH'
#!/usr/bin/env bash
[ -n "${GLCI_BUILT:-}" ] || exit 1
echo "golangci-lint has version 1.64.8 built with go${GLCI_BUILT} from abc on 2026-01-01"
SH
cat >"$stubs/yarn" <<'SH'
#!/usr/bin/env bash
echo "# regenerated: yarn $*" >>yarn.lock
echo "yarn stub: $*"
SH
cat >"$stubs/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$STUB_LOG"
case "$1 $2" in
  "--version "*|"--version") echo "gh version 9.9.9 (stub)"; exit 0 ;;
  "label create") exit 0 ;;
  "pr list") printf '%s' "${GH_EXISTING_URL:-}"; [ -n "${GH_EXISTING_URL:-}" ] && echo; exit 0 ;;
  "pr create") echo "https://github.com/acme/app/pull/7"; exit 0 ;;
  "pr merge") exit 0 ;;
  "repo view") echo "acme/app"; exit 0 ;;
esac
if [ "$1" = api ]; then
  case "$2" in
    repos/*/rules/branches/*) echo "${RULES_COUNT:-0}"; exit 0 ;;
    repos/*/branches/*/protection) [ "${CLASSIC_COUNT:-0}" -ge 1 ] && { echo "$CLASSIC_COUNT"; exit 0; }; exit 1 ;;
    graphql) exit 1 ;;
  esac
fi
echo "gh stub: unhandled $*" >&2
exit 1
SH
# curl: the old script's go.dev lookup only
cat >"$stubs/curl" <<'SH'
#!/usr/bin/env bash
case "$*" in *go.dev/dl*) cat "$GODEV_JSON" ;; *) echo "curl stub: unhandled $*" >&2; exit 22 ;; esac
SH
chmod +x "$stubs"/*

cat >"$work/godev.json" <<'JSON'
[{"version":"go1.26rc1"},{"version":"go1.25.7"},{"version":"go1.25.6"},{"version":"go1.24.13"},{"version":"go1.24.2"}]
JSON

cat >"$work/api.py" <<'PY'
import http.server, json, os, sys
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, st, body, raw=False):
        b = body if raw else json.dumps(body).encode()
        self.send_response(st); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        p = self.path.split("?")[0]
        if p == "/dl": return self.send(200, open(os.environ["GODEV_JSON"], "rb").read(), True)
        n = int(os.environ.get("RULES_COUNT", "0"))
        if "/rules/branches/" in p:
            return self.send(200, [{"type": "required_status_checks", "parameters": {"required_status_checks": [{"context": "c%d" % i} for i in range(n)]}}] if n else [])
        if p.endswith("/protection"):
            c = int(os.environ.get("CLASSIC_COUNT", "0"))
            if c: return self.send(200, {"required_status_checks": {"contexts": ["c%d" % i for i in range(c)]}})
            return self.send(404, {"message": "Branch not protected"})
        self.send(404, {"message": "Not Found"})
s = http.server.HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(s.server_address[1]))
s.serve_forever()
PY

# ---- fixtures -----------------------------------------------------------
# make_repo <dir> <scenario-name>: a repository on master with a bare origin.
make_repo() {
  local d=$1 name=$2
  git init -q -b master "$d.git" --bare
  git init -q -b master "$d"
  (
    cd "$d" || exit 1
    git config user.name fixture && git config user.email fixture@example.invalid
    printf '{"packages":["go@1.25"]}\n' >devbox.json
    cp "$work/fixtures/$name/"* . 2>/dev/null || true
    git add -A && git commit -q -m fixture
    git remote add origin "$d.git" && git push -q origin master
  )
}

mkdir -p "$work/fixtures"
mk() { mkdir -p "$work/fixtures/$1"; }
lock='{"lockfile_version":"1","packages":{"go@1.25":{"version":"1.25.4"},"playwright-driver@latest":{"version":"1.49.1"}}}'
pkg=$'{\n  "name": "x",\n  "packageManager": "yarn@4.5.0",\n  "dependencies": {\n    "left-pad": "1.3.0"\n  },\n  "devDependencies": {\n    "@playwright/test": "1.40.0",\n    "zod": "^3"\n  }\n}\n'
mk bump;      printf 'module x\n\ngo 1.25\n\ntoolchain go1.25.1\n' >"$work/fixtures/bump/go.mod"; printf '%s\n' "$lock" >"$work/fixtures/bump/devbox.lock"
mk parity;    printf 'module x\n\ngo 1.25\n\ntoolchain go1.25.7\n' >"$work/fixtures/parity/go.mod"
mk pw;        printf 'module x\n\ngo 1.25\n\ntoolchain go1.25.7\n' >"$work/fixtures/pw/go.mod"; printf '%s\n' "$lock" >"$work/fixtures/pw/devbox.lock"; printf '%s' "$pkg" >"$work/fixtures/pw/package.json"; : >"$work/fixtures/pw/yarn.lock"
mk above;     printf 'module x\n\ngo 1.26\n' >"$work/fixtures/above/go.mod"
mk dropdown;  printf 'module x\n\ngo 1.25.7\n\ntoolchain go1.25.1\n' >"$work/fixtures/dropdown/go.mod"
mk modules;   printf 'module x\n\ngo 1.25\n\ntoolchain go1.25.1\n' >"$work/fixtures/modules/go.mod"; mkdir -p "$work/fixtures/modules/tools"; printf 'module t\n\ngo 1.25\n\ntoolchain go1.25.6\n' >"$work/fixtures/modules/tools/go.mod"

# ---- the old action, executed step by step -------------------------------
# old_run <repo> <workdir-input> ... uses the INPUT_* variables
old_run() {
  local repo=$1 out=$2 steps count i name run cond k val dir stepout
  steps="$work/old-action.yaml"
  count=$(yq '.runs.steps | length' "$steps")
  : >"$out/old.out"
  full=false
  for ((i = 0; i < count; i++)); do
    name=$(I=$i yq -r '.runs.steps[env(I)].name // "step"' "$steps")
    run=$(I=$i yq -r '.runs.steps[env(I)].run' "$steps")
    cond=$(I=$i yq -r '.runs.steps[env(I)]["if"] // ""' "$steps")
    if [ -n "$cond" ] && [ "$full" != true ]; then continue; fi
    printf '%s\n' "$run" >"$out/step.sh"
    stepout="$out/step.output"; : >"$stepout"
    {
      echo "export GITHUB_OUTPUT=$stepout GITHUB_PATH=$out/path RUNNER_TEMP=$out/tmp"
      while IFS= read -r k; do
        [ -n "$k" ] || continue
        val=$(I=$i K="$k" yq -r '.runs.steps[env(I)].env[strenv(K)]' "$steps")
        val=$(sed -e "s|\${{ inputs.token }}|$INPUT_token|g" -e "s|\${{ inputs.mode }}|$INPUT_mode|g" -e "s|\${{ inputs.full-update-day }}|$INPUT_full_day|g" \
          -e "s|\${{ inputs.module-dirs }}|$INPUT_module_dirs|g" -e "s|\${{ inputs.base }}|master|g" -e "s|\${{ inputs.label }}|dependencies|g" \
          -e "s|\${{ inputs.git-user }}|github-actions[bot]|g" -e "s|\${{ inputs.git-email }}|41898282+github-actions[bot]@users.noreply.github.com|g" <<<"$val")
        printf 'export %s=%q\n' "$k" "$val"
      done < <(I=$i yq -r '.runs.steps[env(I)].env // {} | keys | .[]' "$steps")
    } >"$out/step.env"
    # steps with a working-directory run in the repository, the others (Ensure gh) outside it
    if [ "$(I=$i yq -r '.runs.steps[env(I)]["working-directory"] // ""' "$steps")" != "" ]; then dir=$repo; else dir=$out; fi
    ( cd "$dir" && . "$out/step.env" && bash --noprofile --norc -e -o pipefail "$out/step.sh" ) >>"$out/old.out" 2>&1 || { echo "(step '$name' exited non-zero)" >>"$out/old.out"; break; }
    [ "$(I=$i yq -r '.runs.steps[env(I)].id // ""' "$steps")" = mode ] && grep -q '^full=true' "$stepout" && full=true
    cat "$stepout" >>"$out/old.outputs"
  done
}

fail=0
n=0
# scenario <name> <fixture> <mode> <full-day> <module-dirs> [ENV=VAL...]
scenario() {
  local name=$1 fixture=$2 mode=$3 fday=$4 mdirs=$5
  shift 5
  n=$((n + 1))
  local which
  for which in old new; do
    local d="$work/s$n-$which"
    mkdir -p "$d/tmp" "$d/home"
    make_repo "$d/repo" "$fixture"
    : >"$d/log"; : >"$d/$which.outputs"; : >"$d/path"
    export STUB_LOG="$d/log" GODEV_JSON="$work/godev.json" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null HOME="$d/home" GOTOOLCHAIN=local GOFLAGS=-mod=mod
    local envs=("$@")
    ( 
      for kv in "${envs[@]}"; do export "${kv?}"; done
      export PATH="$stubs:$PATH"
      if [ $which = old ]; then
        INPUT_token=stub-token INPUT_mode=$mode INPUT_full_day=$fday INPUT_module_dirs=$mdirs old_run "$d/repo" "$d"
      else
        : >"$d/new.outputs"
        pf="$d/port"; python3 "$work/api.py" "$pf" & srv=$!
        for _ in $(seq 1 100); do [ -s "$pf" ] && break; sleep 0.1; done
        port=$(cat "$pf")
        env TOKEN=stub-token WORKDIR="$d/repo" BASE=master LABEL=dependencies MODE=$mode FULL_DAY=$fday MODULE_DIRS=$mdirs \
          GIT_USER='github-actions[bot]' GIT_EMAIL='41898282+github-actions[bot]@users.noreply.github.com' \
          RUNNER_TEMP="$d/tmp" GITHUB_OUTPUT="$d/new.outputs" GITHUB_PATH="$d/path" GITHUB_API_URL="http://127.0.0.1:$port" \
          CI_ACTIONS_GO_DL_URL="http://127.0.0.1:$port/dl" "$work/ci-actions" devbox-parity >"$d/new.out" 2>&1 \
          || echo "(exited non-zero)" >>"$d/new.out"
        kill $srv 2>/dev/null
      fi
    )
    # normalise: no group markers; the old script's gh-install step prints "gh present"
    # hashes, temp paths and the per-run directory differ by construction
    norm() { sed -E -e 's#\[chore/devbox-update [0-9a-f]+\]#[chore/devbox-update H]#' -e "s#$d#D#g" -e 's#index [0-9a-f]+\.\.[0-9a-f]+#index H..H#'; }
    grep -v '^::\(end\)\?group::' "$d/$which.out" | norm >"$d/$which.norm"
    grep -v '^full=' "$d/$which.outputs" >"$d/$which.outputs.norm"
    # the old script asked the CLI, the port asks the API: the answer is compared, not the transport
    grep -v '^gh api' "$d/log" >"$d/log.norm"
    {
      echo "--- branches"; git -C "$d/repo" branch --format='%(refname:short)' | sort
      echo "--- tree"; (cd "$d/repo" && find . -path ./.git -prune -o -type f -print | sort | xargs -I{} sh -c 'echo "== {}"; cat "{}"')
      echo "--- config"; git -C "$d/repo" config user.name; git -C "$d/repo" config user.email
      echo "--- pushed"; git -C "$d/repo.git" log --format='%s|%an|%ae' chore/devbox-update 2>/dev/null | head -3
      echo "--- pushed diff"; git -C "$d/repo.git" diff master chore/devbox-update 2>/dev/null | norm
    } >"$d/$which.state"
  done
  local o="$work/s$n-old" w="$work/s$n-new" same=1
  cmp -s "$o/old.norm" "$w/new.norm" || same=0
  cmp -s "$o/old.outputs.norm" "$w/new.outputs.norm" || same=0
  cmp -s "$o/log.norm" "$w/log.norm" || same=0
  cmp -s "$o/old.state" "$w/new.state" || same=0
  if [ $same = 1 ]; then
    printf 'same  %s\n' "$name"
  else
    printf 'DIFF  %s\n' "$name"
    diff "$o/old.norm" "$w/new.norm" | head -20
    diff "$o/old.outputs.norm" "$w/new.outputs.norm" | head
    diff "$o/log.norm" "$w/log.norm" | head -20
    diff "$o/old.state" "$w/new.state" | head -20
    fail=1
  fi
}

today=$(date -u +%u)
other=$(( today % 7 + 1 ))
scenario "full: update, toolchain raised, PR armed by a ruleset" bump full 1 '[]' GLCI_BUILT=1.25.4 RULES_COUNT=1
scenario "auto on its day behaves as full" bump auto "$today" '[]' GLCI_BUILT=1.25.4 CLASSIC_COUNT=2
scenario "auto off its day aligns only" parity auto "$other" '[]' GLCI_BUILT=1.25.4
scenario "at parity" parity align 1 '[]' GLCI_BUILT=1.25.4
scenario "no required check: left for a human" bump align 1 '[]' GLCI_BUILT=1.25.4
scenario "an open pull request is updated in place" bump align 1 '[]' GLCI_BUILT=1.25.4 RULES_COUNT=1 GH_EXISTING_URL=https://github.com/acme/app/pull/3
scenario "playwright follows devbox (yarn berry)" pw align 1 '[]' GLCI_BUILT=1.25.4 RULES_COUNT=1
scenario "language above the cap is left alone" above align 1 '[]' GLCI_BUILT=1.25.4
scenario "target equal to the language drops the toolchain" dropdown align 1 '[]' GLCI_BUILT=1.25.4 RULES_COUNT=1
scenario "go.dev has no release of the line" bump align 1 '[]' GLCI_BUILT=1.23.4
scenario "several modules, one lookup" modules align 1 '["tools","gone","tools"]' GLCI_BUILT=1.25.4 CLASSIC_COUNT=1
scenario "no golangci-lint: the devbox go decides" parity align 1 '[]'
echo "$n scenarios"
exit $fail
