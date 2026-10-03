#!/usr/bin/env bash
# token-exchange, run as written against a stub curl and a stub kubectl:
# the profiles, the kubeconfig and the GitHub App token it writes, that
# every token is masked before it is written, and each refusal.
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/token-exchange/run.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }
has() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }

mkdir -p "$work/bin"
# The OIDC request carries a Bearer header; anything else is the issuer's
# /token. The audience is echoed into the token so a case can tell them apart.
cat >"$work/bin/curl" <<'STUB'
#!/usr/bin/env bash
args="$*"
printf '%s\n' "$args" >> "$STUB_LOG"
case "$args" in
  *"Authorization: Bearer"*) echo '{"value":"subject-jwt"}'; exit 0 ;;
esac
if [ -n "${STUB_REFUSE:-}" ]; then
  echo '{"error":"access_denied","error_description":"no rule grants this job"}'
  exit 22
fi
aud=$(printf '%s\n' "$args" | sed -n 's/.*audience=\([^ ]*\).*/\1/p' | head -1)
printf '{"access_token":"tok-%s"}\n' "$aud"
STUB
cat >"$work/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >> "$STUB_LOG"
STUB
chmod +x "$work/bin/"*
for t in bash jq tr xargs printf mkdir chmod cat dirname env sed head; do
  command -v "$t" >/dev/null && [ ! -e "$work/bin/$t" ] && ln -s "$(command -v "$t")" "$work/bin/$t"
done

# run <case> VAR=value...: a fresh job each time. Prints the log; the
# outputs, GITHUB_ENV and GITHUB_OUTPUT land under $work/<case>.
run() {
  local c=$1; shift
  mkdir -p "$work/$c/tmp"
  : >"$work/$c/env"; : >"$work/$c/out"; : >"$work/$c/log"
  env -i PATH="$work/bin" HOME="$work" RUNNER_TEMP="$work/$c/tmp" \
    GITHUB_ENV="$work/$c/env" GITHUB_OUTPUT="$work/$c/out" STUB_LOG="$work/$c/log" \
    ACTIONS_ID_TOKEN_REQUEST_URL="https://token.example/req?x=1" ACTIONS_ID_TOKEN_REQUEST_TOKEN=req-token \
    ISSUER=https://issuer.example AUDIENCES="" WANT_KUBECONFIG=false DEFAULT_PROFILE="" REGION="" \
    GITHUB_APP="" REPOSITORIES="" PERMISSIONS="" "$@" bash "$script" 2>&1
}

# aws audiences: both of a comma/newline list are written, the last one too.
log=$(run aws AUDIENCES=$'aws:111122223333:deployer,\naws:444455556666:reader' REGION=eu-west-1 DEFAULT_PROFILE=reader@444455556666)
rc=$?
cfg="$work/aws/tmp/access-roster/aws-config"
out=$(cat "$work/aws/out"); env_=$(cat "$work/aws/env")
if [ $rc = 0 ] && has "$out" "profiles=deployer@111122223333 reader@444455556666" \
  && grep -q 'role_arn = arn:aws:iam::444455556666:role/reader' "$cfg" \
  && grep -q 'region = eu-west-1' "$cfg" \
  && has "$env_" "AWS_CONFIG_FILE=$cfg" && has "$env_" "AWS_PROFILE=reader@444455556666" \
  && has "$log" "::add-mask::tok-aws:111122223333:deployer"; then
  ok "aws audiences: every profile written (the last one too), default profile chosen, token masked"
else
  bad "aws audiences (rc=$rc): $log / $out / $env_"
fi

# The client is form-encoded in Basic, and the subject is the job's token.
if grep -q -- '-u aws%3A111122223333%3Adeployer:' "$work/aws/log" && grep -q 'subject_token=subject-jwt' "$work/aws/log" \
  && grep -q 'audience=https%3A%2F%2Fissuer.example' "$work/aws/log"; then
  ok "the client is form-encoded in Basic; the OIDC token is minted for the issuer's own url"
else
  bad "client encoding / OIDC audience: $(cat "$work/aws/log")"
fi

# k8s: a context per audience, only when asked for.
log=$(run k8s AUDIENCES=k8s:dev WANT_KUBECONFIG=true)
rc=$?
if [ $rc = 0 ] && has "$(cat "$work/k8s/out")" "kubeconfig=$work/k8s/tmp/access-roster/kubeconfig" \
  && grep -q 'set-credentials dev --token=tok-k8s:dev' "$work/k8s/log" \
  && grep -q 'set-context dev --user=dev --cluster=dev' "$work/k8s/log" \
  && has "$(cat "$work/k8s/env")" "KUBECONFIG="; then
  ok "k8s audience writes a credential and a context, and exports KUBECONFIG"
else
  bad "k8s audience (rc=$rc): $log"
fi
run k8s-off AUDIENCES=k8s:dev >/dev/null
if ! grep -q kubectl "$work/k8s-off/log" && has "$(cat "$work/k8s-off/out")" "kubeconfig="; then
  ok "no kubeconfig unless asked for"
else
  bad "kubeconfig written without being asked for"
fi

# github-app: token masked, then output; the request carries the narrowing.
log=$(run app GITHUB_APP=github-app:some-app REPOSITORIES=$'one, two' PERMISSIONS='contents=write,pull_requests:read')
rc=$?
if [ $rc = 0 ] && has "$(cat "$work/app/out")" "github-token=tok-github-app:some-app" \
  && has "$log" "::add-mask::tok-github-app:some-app" \
  && grep -q 'repositories=one two' "$work/app/log" \
  && grep -q 'scope=contents:write pull_requests:read' "$work/app/log" \
  && grep -q 'requested_token_type=urn:access-roster:params:oauth:token-type:github-installation-token' "$work/app/log"; then
  ok "github-app: masked token output, repositories and permissions normalised"
else
  bad "github-app (rc=$rc): $log"
fi

expect_fail() { # name want-message env...
  local name=$1 want=$2; shift 2
  local l; l=$(run "$name" "$@"); local r=$?
  if [ $r != 0 ] && has "$l" "$want"; then ok "refuses: $name"; else bad "refuses: $name (rc=$r): $l"; fi
}
expect_fail nothing "nothing to exchange for"
expect_fail no-oidc "no id-token permission" AUDIENCES=k8s:dev ACTIONS_ID_TOKEN_REQUEST_URL=
expect_fail issuer-refuses "the issuer refused aws:111122223333:deployer: no rule grants this job" AUDIENCES=aws:111122223333:deployer STUB_REFUSE=1
expect_fail bad-audience "neither a k8s: nor an aws: audience" AUDIENCES=gcp:x
expect_fail bad-app "is not a catalogue id" GITHUB_APP='Bad_App'
expect_fail unknown-default "default-profile is not one this run wrote" AUDIENCES=aws:111122223333:deployer DEFAULT_PROFILE=other@1

if [ "$fail" = 0 ]; then echo "token-exchange cases passed"; fi
exit "$fail"
