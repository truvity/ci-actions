#!/usr/bin/env bash
# CI_RUNNER_GITHUB_RELEASES in the shell library: the URL rewrite
# (_ci_actions_mirror_url) and the download that asks the mirror first and
# falls back to GitHub (_ci_actions_download), against a stub curl. The same
# rule is table-tested in Go (internal/ghmirror).
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
ok() { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }

# shellcheck source=../lib/ci-actions-bin.sh
. "$here/lib/ci-actions-bin.sh"

gh=https://github.com/acme/tool/releases/download/v1.2.3/tool_1.2.3_linux_amd64.tar.gz

url_case() { # name, variable value (or UNSET), url, want (or NONE)
  local name=$1 val=$2 url=$3 want=$4 got rc=0
  if [ "$val" = UNSET ]; then
    got=$(unset CI_RUNNER_GITHUB_RELEASES; _ci_actions_mirror_url "$url") || rc=$?
  else
    got=$(CI_RUNNER_GITHUB_RELEASES=$val _ci_actions_mirror_url "$url") || rc=$?
  fi
  if [ "$want" = NONE ]; then
    [ "$rc" = 1 ] && [ -z "$got" ] && ok "$name" || bad "$name (rc=$rc got=$got)"
  else
    [ "$rc" = 0 ] && [ "$got" = "$want" ] && ok "$name" || bad "$name (rc=$rc got=$got want=$want)"
  fi
}

m=https://cache.example/github
rel=acme/tool/releases/download/v1.2.3/tool_1.2.3_linux_amd64.tar.gz
url_case "unset: no mirror" UNSET "$gh" NONE
url_case "empty: no mirror" "" "$gh" NONE
url_case "blank: no mirror" "   " "$gh" NONE
url_case "not a URL: no mirror" "cache.example/github" "$gh" NONE
url_case "set: the leading host is swapped" "$m" "$gh" "$m/$rel"
url_case "trailing slash is dropped" "$m/" "$gh" "$m/$rel"
url_case "whitespace is dropped" " $m
" "$gh" "$m/$rel"
url_case "http base works" "http://cache.internal:8080/github" "$gh" "http://cache.internal:8080/github/$rel"
url_case "a bare version tag" "$m" "https://github.com/acme/tool/releases/download/1.2.3/x.tgz" "$m/acme/tool/releases/download/1.2.3/x.tgz"
url_case "a prerelease tag" "$m" "https://github.com/acme/tool/releases/download/v1.2.3-rc.1/x.tgz" "$m/acme/tool/releases/download/v1.2.3-rc.1/x.tgz"
for tag in latest nightly edge main vnext v; do
  url_case "the moving tag $tag stays on github.com" "$m" "https://github.com/acme/tool/releases/download/$tag/x.tgz" NONE
done
url_case "a release page is not a download" "$m" "https://github.com/acme/tool/releases/tag/v1.2.3" NONE
url_case "latest is not a download" "$m" "https://github.com/acme/tool/releases/latest" NONE
url_case "an archive URL is not a download" "$m" "https://github.com/acme/tool/archive/refs/tags/v1.tar.gz" NONE
url_case "another host is untouched" "$m" "https://example.com/acme/tool/releases/download/v1/x.tgz" NONE
url_case "a download without an asset is untouched" "$m" "https://github.com/acme/tool/releases/download/v1.2.3" NONE
url_case "dot segments are untouched" "$m" "https://github.com/acme/tool/releases/download/../x/y" NONE

# --- the download: a stub curl serving two "hosts" out of directories.
mkdir -p "$work/bin" "$work/mirror" "$work/direct"
cat >"$work/bin/curl" <<'STUB'
#!/usr/bin/env bash
out="" url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    --retry | --retry-delay | --retry-max-time | --max-time) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
echo "$url" >>"$CURL_LOG"
case "$url" in
  https://cache.example/github/*) dir=$MIRROR_DIR; name=${url#https://cache.example/github/} ;;
  https://github.com/*) dir=$DIRECT_DIR; name=${url#https://github.com/} ;;
  *) exit 22 ;;
esac
[ -f "$dir/${name//\//_}" ] || exit 22
cp "$dir/${name//\//_}" "$out"
STUB
chmod +x "$work/bin/curl"

export CURL_LOG="$work/log" MIRROR_DIR="$work/mirror" DIRECT_DIR="$work/direct"
key=${rel//\//_}
dl() { # variable value; leaves the file in $work/out and the requests in $work/log
  : >"$CURL_LOG"; rm -f "$work/out"
  PATH="$work/bin:$PATH" CI_RUNNER_GITHUB_RELEASES=$1 _ci_actions_download "$gh" "$work/out" 2>"$work/err"
}

echo mirror >"$work/mirror/$key"; echo direct >"$work/direct/$key"
dl "$m"; rc=$?
[ $rc = 0 ] && [ "$(cat "$work/out")" = mirror ] && [ "$(wc -l <"$CURL_LOG")" = 1 ] \
  && ok "the mirror serves it: one request, to the mirror" || bad "the mirror serves it (rc=$rc): $(cat "$CURL_LOG")"

dl ""; rc=$?
[ $rc = 0 ] && [ "$(cat "$work/out")" = direct ] && [ "$(cat "$CURL_LOG")" = "$gh" ] \
  && ok "empty variable: GitHub only, exactly the URL given" || bad "empty variable (rc=$rc): $(cat "$CURL_LOG")"

rm "$work/mirror/$key"
dl "$m"; rc=$?
[ $rc = 0 ] && [ "$(cat "$work/out")" = direct ] && grep -q '^::warning::the GitHub releases mirror' "$work/err" \
  && [ "$(sed -n 2p "$CURL_LOG")" = "$gh" ] \
  && ok "the mirror answers an error: falls back to GitHub, with a warning" || bad "mirror error falls back (rc=$rc): $(cat "$CURL_LOG") / $(cat "$work/err")"

rm "$work/direct/$key"
dl "$m"; rc=$?
[ $rc = 22 ] && ok "both fail: curl's own exit code comes back" || bad "both fail (rc=$rc)"

[ $fail = 0 ] && echo "github releases mirror cases: all passed"
exit $fail
