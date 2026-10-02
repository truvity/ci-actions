#!/usr/bin/env bash
# TEMPORARY, removed before merge: the inline-shell setup-devbox steps (the run
# bodies of the action.yaml, and preflight.sh and install-devbox.sh, at the base
# the pull request branched from) against the Go port, step by step, on the
# same fixtures with stand-ins for devbox, aws, nix and a release server.
# Compared per scenario: stdout, the exit status, GITHUB_ENV and GITHUB_OUTPUT
# (the heredoc delimiter normalised), the step summary (durations normalised),
# the installed binary, the git config the step wrote, and the file the strip
# step edited.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
srvpid=""
trap '[ -n "$srvpid" ] && kill "$srvpid" 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$work"' EXIT
command -v yq >/dev/null || { echo "::error::yq is required"; exit 2; }
git -C "$here" show "$base:setup-devbox/action.yaml" >"$work/action.yaml" || exit 2
git -C "$here" show "$base:setup-devbox/preflight.sh" >"$work/preflight.sh" || exit 2
git -C "$here" show "$base:setup-devbox/install-devbox.sh" >"$work/install-devbox.sh" || exit 2
body() { NAME="$1" yq -r '.runs.steps[] | select(.name == strenv(NAME)) | .run' "$work/action.yaml"; }
body "CI face of the AWS config" >"$work/aws-config.sh"
body "Detect baked tools" >"$work/detect-baked.sh"
body "Strip local-only proto tools" >"$work/strip-tools.sh"
body "Materialize devbox environment" >"$work/materialize.sh"
body "Install proto toolchain" >"$work/proto.sh"
body "Expose the token to recipes" >"$work/expose-token.sh"
body "Reach private Go modules" >"$work/go-private.sh"
body "Log in to CodeArtifact" >"$work/codeartifact.sh"
body "Warn on the retired cache-server input" >"$work/retired-cache-server.sh"
body "Guard GOPROXY against devbox.json" >"$work/guard-goproxy.sh"
body "Guard AWS config against devbox.json" >"$work/guard-aws.sh"
cp "$work/preflight.sh" "$work/preflight-old.sh"; cp "$work/install-devbox.sh" "$work/install-devbox-old.sh"
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2

stubs="$work/stubs"; mkdir -p "$stubs"
cat >"$stubs/devbox" <<'SH'
#!/usr/bin/env bash
[ "$1" = run ] && shift; [ "$1" = -- ] && shift
n=$(cat "$DEVBOX_COUNT" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" >"$DEVBOX_COUNT"
if [ "$1" = true ] || [ "$1" = proto ]; then [ "$n" -gt "${DEVBOX_FAILS:-0}" ] || { echo "devbox: not yet" >&2; exit 1; }; fi
if [ "$1" = proto ]; then printf 'node installed\ngo installed\nalready present\n'; exit 0; fi
if [ "$1" = aws ]; then printf 'Info: starting\r\nInfo: ready\n%s\r\n' "${CA_TOKEN:-tok.en-value}"; exit "${AWS_EXIT:-0}"; fi
exec "$@"
SH
cat >"$stubs/aws" <<'SH'
#!/usr/bin/env bash
[ "${AWS_EXIT:-0}" = 0 ] || exit "$AWS_EXIT"
echo "${CA_TOKEN-tok.en-value}"
SH
cat >"$stubs/sleep" <<'SH'
#!/usr/bin/env bash
case "$1" in 20) exit 0 ;; esac
exec /bin/sleep "$@"
SH
mkdir -p "$work/nixok" "$work/nixdead" "$work/devboxbin" "$work/awsbin"
printf '#!/bin/sh\nexit 0\n' >"$work/nixok/nix"; printf '#!/bin/sh\nexit 1\n' >"$work/nixdead/nix"
printf '#!/bin/sh\n' >"$work/devboxbin/devbox"; cp "$stubs/aws" "$work/awsbin/aws"
chmod +x "$stubs"/* "$work"/nixok/nix "$work"/nixdead/nix "$work"/devboxbin/devbox "$work"/awsbin/aws
printf 'Name:\tx\nNoNewPrivs:\t1\n' >"$work/nnp1"; printf 'Name:\tx\nNoNewPrivs:\t0\n' >"$work/nnp0"
ln -s "$(command -v bash)" "$work/sh-is-bash"; ln -s "$(command -v ls)" "$work/sh-is-other"

# a local release server: /download/<v>/<asset>, /latest redirects to /tag/<v>
rel="$work/rel"; mkdir -p "$rel/download/1.2.3" "$work/pack"
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; esac
printf '#!/bin/sh\necho "devbox 1.2.3 (stand-in)"\n' >"$work/pack/devbox"; chmod +x "$work/pack/devbox"
asset="devbox_1.2.3_${os}_${arch}.tar.gz"
tar -czf "$rel/download/1.2.3/$asset" -C "$work/pack" devbox
(cd "$rel/download/1.2.3" && sha256sum "$asset" >checksums.txt)
mkdir -p "$rel/download/1.2.4"; cp "$rel/download/1.2.3/$asset" "$rel/download/1.2.4/devbox_1.2.4_${os}_${arch}.tar.gz"
printf 'tampered' >>"$rel/download/1.2.4/devbox_1.2.4_${os}_${arch}.tar.gz"; (cd "$rel/download/1.2.3" && sha256sum "$asset" | sed "s/$asset/devbox_1.2.4_${os}_${arch}.tar.gz/") >"$rel/download/1.2.4/checksums.txt"
mkdir -p "$rel/download/1.2.5"; cp "$rel/download/1.2.3/$asset" "$rel/download/1.2.5/devbox_1.2.5_${os}_${arch}.tar.gz"; echo "ffff  other.tar.gz" >"$rel/download/1.2.5/checksums.txt"
cat >"$work/rel.py" <<'PY'
import http.server, os, sys
ROOT = sys.argv[2]
class H(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **k): super().__init__(*a, directory=ROOT, **k)
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path == "/latest":
            self.send_response(302); self.send_header("Location", "/tag/1.2.3"); self.end_headers(); return
        if self.path.startswith("/tag/") or self.path == "/nolatest/latest": self.send_response(200); self.end_headers(); self.wfile.write(b"ok"); return
        return super().do_GET()
s = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(s.server_address[1])); s.serve_forever()
PY
python3 "$work/rel.py" "$work/port" "$rel" & srvpid=$!
for _ in $(seq 1 100); do [ -s "$work/port" ] && break; sleep 0.1; done
relurl="http://127.0.0.1:$(cat "$work/port")"

fail=0; n=0
# scenario <step> <name> [VAR=value ...]   (new step name = old script name)
scenario() {
  local step=$1 name=$2
  shift 2
  n=$((n + 1))
  local which
  for which in old new; do
    local d="$work/s$n-$which"
    mkdir -p "$d/ws" "$d/home" "$d/temp"
    : >"$d/github_env"; : >"$d/github_output"; : >"$d/summary"; : >"$d/github_path"
    (
      cd "$d/ws" || exit 1
      git init -q -b master . && echo a >tracked.txt && git add -A && git -c user.name=p -c user.email=p@example.invalid commit -q -m init
      export HOME="$d/home" RUNNER_TEMP="$d/temp" GITHUB_WORKSPACE="$d/ws" GITHUB_ENV="$d/github_env" GITHUB_OUTPUT="$d/github_output" \
        GITHUB_STEP_SUMMARY="$d/summary" GITHUB_PATH="$d/github_path" DEVBOX_COUNT="$d/devbox-count" PATH="$stubs:/usr/bin:/bin:$PATH" \
        GIT_CONFIG_GLOBAL="$d/home/.gitconfig" GIT_CONFIG_SYSTEM=/dev/null
      for kv in "$@"; do export "${kv//@D@/$d}"; done
      [ -z "${PROTOTOOLS:-}" ] || printf '%b' "$PROTOTOOLS" >.prototools
      [ -z "${DEVBOX_JSON:-}" ] || printf '%s' "$DEVBOX_JSON" >devbox.json
      [ -z "${TRACK_PROTOTOOLS:-}" ] || { git add .prototools && git -c user.name=p -c user.email=p@example.invalid commit -q -m tools; }
      [ -z "${EXTRA_PATH:-}" ] || export PATH="${EXTRA_PATH//@W@/$work}:$PATH"
      script="$work/$step.sh"; newstep=$step
      [ "$step" = preflight ] && script="$work/preflight-old.sh"
      [ "$step" = install-devbox ] && script="$work/install-devbox-old.sh"
      if [ $which = old ]; then
        bash --noprofile --norc -eo pipefail "$script" >"$d/stdout" 2>"$d/stderr"
      else
        ACTION_PATH=x "$work/ci-actions" setup-devbox "$newstep" >"$d/stdout" 2>"$d/stderr"
      fi
      echo "rc=$([ $? = 0 ] && echo zero || echo nonzero)" >"$d/rc"
    )
    norm() { sed -E -e "s#$d#D#g" -e 's#EOF_[0-9a-f]{32}#DELIM#g' -e 's#devbox closure: [0-9]+s#devbox closure: Ns#' -e 's#proto use: [0-9]+s#proto use: Ns#' -e 's#sha256 [0-9a-f]{64}#sha256 H#' -e 's#127\.0\.0\.1:[0-9]+#HOST#g'; }
    norm <"$d/stdout" >"$d/stdout.norm"
    { for f in github_env github_output summary github_path; do echo "== $f"; norm <"$d/$f"; done
      echo "== prototools"; cat "$d/ws/.prototools" 2>/dev/null; echo "== git flags"; (cd "$d/ws" && git ls-files -v | grep -c '^h' || true)
      echo "== gitconfig"; sed -E "s#$d#D#g" "$d/home/.gitconfig" 2>/dev/null
      echo "== devbox bin"; ls "$d/temp/bin" 2>/dev/null; "$d/temp/bin/devbox" 2>/dev/null
      echo "== temp leftovers"; ls -A "$d/temp" | grep -v '^bin$\|bootstrap-proto.log' ; } >"$d/state.norm"
  done
  local o="$work/s$n-old" w="$work/s$n-new" same=1 part
  for part in stdout.norm state.norm rc; do cmp -s "$o/$part" "$w/$part" || same=0; done
  if [ $same = 1 ]; then printf 'same  %s: %s\n' "$step" "$name"; else
    printf 'DIFF  %s: %s\n' "$step" "$name"
    for part in stdout.norm state.norm rc; do diff "$o/$part" "$w/$part" | head -12; done
    fail=1
  fi
}

pf="PREFLIGHT_PROC_STATUS=$work/nnp1 PREFLIGHT_SH_PATH=$work/sh-is-bash PREFLIGHT_EXPECT_NIX=false"
# --- preflight: one branch at a time ---
scenario preflight "a restricted-shaped environment passes" $pf EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "sh not bash, no_new_privs 0" $pf PREFLIGHT_PROC_STATUS="$work/nnp0" PREFLIGHT_SH_PATH="$work/sh-is-other" EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "no_new_privs unreadable" $pf PREFLIGHT_PROC_STATUS=@D@/nope EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "an unwritable HOME" $pf HOME=@D@/nope EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "an unwritable RUNNER_TEMP" $pf RUNNER_TEMP=@D@/nope EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "an unwritable workdir" $pf GITHUB_WORKSPACE=@D@/nope EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "several problems, one error" $pf HOME=@D@/n1 RUNNER_TEMP=@D@/n2 EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "nix reachable" $pf PREFLIGHT_EXPECT_NIX=true EXTRA_PATH="@W@/nixok:@W@/devboxbin"
scenario preflight "nix daemon dead" $pf PREFLIGHT_EXPECT_NIX=true EXTRA_PATH="@W@/nixdead:@W@/devboxbin"
scenario preflight "auto expects nix when on PATH" $pf PREFLIGHT_EXPECT_NIX=auto EXTRA_PATH="@W@/nixdead:@W@/devboxbin"
scenario preflight "no nix, restricted" $pf PREFLIGHT_NIX_BIN= EXTRA_PATH="@W@/devboxbin"
scenario preflight "no nix, can escalate" $pf PREFLIGHT_NIX_BIN= PREFLIGHT_PROC_STATUS="$work/nnp0" EXTRA_PATH="@W@/devboxbin"
scenario preflight "no devbox" $pf PREFLIGHT_DEVBOX_BIN= EXTRA_PATH="@W@/nixok"
# --- install-devbox ---
scenario install-devbox "the published binary" DEVBOX_RELEASE_BASE="$relurl" DEVBOX_VERSION=1.2.3
scenario install-devbox "a tampered archive" DEVBOX_RELEASE_BASE="$relurl" DEVBOX_VERSION=1.2.4
scenario install-devbox "checksums that do not list the asset" DEVBOX_RELEASE_BASE="$relurl" DEVBOX_VERSION=1.2.5
scenario install-devbox "a release that does not exist" DEVBOX_RELEASE_BASE="$relurl" DEVBOX_VERSION=9.9.9
scenario install-devbox "latest" DEVBOX_RELEASE_BASE="$relurl"
scenario install-devbox "a redirect that names no tag" DEVBOX_RELEASE_BASE="$relurl/nolatest" DEVBOX_VERSION=latest
# --- the small steps ---
scenario aws-config "the CI face" AWS_CONFIG_REL=aws-ci.ini
scenario aws-config "a newline stays inside its block" AWS_CONFIG_REL=$'x\nPATH=/evil'
scenario detect-baked "nothing baked, no prototools" PATH="$stubs:/usr/bin:/bin"
scenario detect-baked "devbox and nix baked, prototools present" EXTRA_PATH="@W@/nixok:@W@/devboxbin" PROTOTOOLS='node = "22"\n'
scenario strip-tools "no .prototools"
scenario strip-tools "a tracked .prototools" PROTOTOOLS='node = "22"\n"npm:@cubic-dev-ai/cli" = "1"\ngo = "1.25"\n' TRACK_PROTOTOOLS=1
scenario materialize "devbox answers" 
scenario materialize "flaky once" DEVBOX_FAILS=1
scenario materialize "down" DEVBOX_FAILS=9
scenario proto "installs" RUNNER_ARCH=X64
scenario proto "down" RUNNER_ARCH=ARM64 DEVBOX_FAILS=9
scenario expose-token "a token" MODULE_TOKEN=ghs_abc.def_123
scenario expose-token "empty" MODULE_TOKEN=
scenario expose-token "a newline" MODULE_TOKEN=$'ghs_a\nPATH=/evil'
scenario go-private "valid" MODULE_TOKEN=ghs_tok GO_PRIVATE='github.com/example,example.com/*'
scenario go-private "a newline injects nothing" MODULE_TOKEN=ghs_tok GO_PRIVATE=$'github.com/example\nPATH=/evil'
scenario go-private "a space" MODULE_TOKEN=ghs_tok GO_PRIVATE='a b'
scenario go-private "no token" MODULE_TOKEN= GO_PRIVATE=github.com/example
scenario codeartifact "aws on PATH" CA_DOMAIN=dom CA_OWNER=owner-acct CA_REGION=eu-west-1 EXTRA_PATH="@W@/awsbin"
scenario codeartifact "through devbox" CA_DOMAIN=dom CA_OWNER=owner-acct CA_REGION=eu-west-1
scenario codeartifact "a failing login" CA_DOMAIN=dom CA_OWNER=1 CA_REGION=r AWS_EXIT=3 EXTRA_PATH="@W@/awsbin"
scenario codeartifact "an empty token" CA_DOMAIN=dom CA_OWNER=1 CA_REGION=r CA_TOKEN= EXTRA_PATH="@W@/awsbin"
scenario codeartifact "None" CA_DOMAIN=dom CA_OWNER=1 CA_REGION=r CA_TOKEN=None EXTRA_PATH="@W@/awsbin"
scenario retired-cache-server "the warning"
scenario guard-goproxy "no devbox.json"
scenario guard-goproxy "pinned" DEVBOX_JSON='{"env":{"GOPROXY":"https://p.example"}}'
scenario guard-goproxy "clean" DEVBOX_JSON='{"env":{"GOFLAGS":"-x"}}'
scenario guard-goproxy "not plain JSON" DEVBOX_JSON='{ // c
}'
scenario guard-aws "profile pinned" DEVBOX_JSON='{"env":{"AWS_PROFILE":"dev"}}'
scenario guard-aws "config file pinned" DEVBOX_JSON='{"env":{"AWS_CONFIG_FILE":"x/aws.ini"}}'
scenario guard-aws "clean" DEVBOX_JSON='{"packages":[]}'
scenario guard-aws "not plain JSON" DEVBOX_JSON='nope'
echo "$n scenarios"
exit $fail
