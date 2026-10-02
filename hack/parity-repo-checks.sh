#!/usr/bin/env bash
# TEMPORARY, removed before merge: hack/no-escalation-cases.sh's scan and
# hack/policy-kit-current.sh (at the base the pull request branched from)
# against `ci-actions repo-check no-escalation` and `repo-check policy-kit`, on
# the same fixtures. no-escalation compares the sorted hit lines and the exit
# status; policy-kit compares stdout and the exit status, the old script reading
# the "upstream" through a curl stand-in and the port through its base URL.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
srv=""
trap '[ -n "$srv" ] && kill "$srv" 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$work"' EXIT
command -v yq >/dev/null || { echo "::error::yq is required"; exit 2; }
git -C "$here" show "$base:hack/no-escalation-cases.sh" >"$work/ne.sh" || exit 2
git -C "$here" show "$base:hack/policy-kit-current.sh" >"$work/pk.sh" || exit 2
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2
word='su''do'
fail=0; n=0

# ---- no-escalation ----
mk() { # dir
  rm -rf "$1"; mkdir -p "$1/act" "$1/hack/restricted-sim" "$1/node_modules/p" "$1/.git/hooks" "$1/deep/restricted-sim-not" "$1/bin"
  printf 'runs:\n  steps:\n    - shell: bash\n      run: %s ln -sf /bin/bash /bin/sh\n' "$word" >"$1/act/action.yaml"
  printf '#!/usr/bin/env bash\n# %s apt-get update\n' "$word" >"$1/act/x.sh"
  printf 'recipe:\n    %s true\n' "$word" >"$1/Justfile"
  printf 'FROM x\nRUN %s true\n' "$word" >"$1/hack/restricted-sim/Dockerfile"
  printf 'we removed %s\n' "$word" >"$1/CHANGELOG.md"
  printf '%s x\n' "$word" >"$1/node_modules/p/i.js"; printf '%s x\n' "$word" >"$1/.git/hooks/pre-commit"
  printf 'pseudo-%sx and pseudocode, %s_ and _%s\n' "$word" "$word" "$word" >"$1/act/ok.sh"
  printf '\000\001 %s \002' "$word" >"$1/bin/blob"
  printf '%s is flagged\n' "$word" >"$1/deep/restricted-sim-not/f.sh"
}
ne_case() { # name, fixture-dir
  n=$((n + 1))
  local orc wrc
  (cd "$work" && bash "$work/ne.sh" --scan "$2" >"$work/ne.old" 2>&1); orc=$?
  "$work/ci-actions" repo-check no-escalation "$2" >"$work/ne.new" 2>&1; wrc=$?
  local o w
  o=$(sed "s#$2/##" "$work/ne.old" | sort)
  w=$(grep -v '^::error::the no-privilege\|^ok    ' "$work/ne.new" | sed "s#$2/##" | sort)
  if [ "$o" = "$w" ] && [ "$orc" = "$wrc" ]; then echo "same  no-escalation: $1"; else
    echo "DIFF  no-escalation: $1 (old rc=$orc new rc=$wrc)"; diff <(echo "$o") <(echo "$w") | head; fail=1; fi
}
mk "$work/ne1"; ne_case "every shape the scan must catch or leave" "$work/ne1"
mkdir -p "$work/ne2" && printf 'echo hi\n' >"$work/ne2/a.sh" && printf '%s\n' "$word" >"$work/ne2/README.md"; ne_case "a clean tree" "$work/ne2"
mkdir -p "$work/ne3/sub" && printf 'x\n%s here\n\n%s there\n' "$word" "$word" >"$work/ne3/sub/multi.yaml"; ne_case "several lines in one file" "$work/ne3"

# ---- policy-kit ----
cat >"$work/curl" <<'SH'
#!/usr/bin/env bash
out=""; url=""
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done
case "$url" in
  *"/acme/policy/v1.2.3/lint/golangci-depguard.yaml") cat "$UPSTREAM" >"$out" ;;
  *) exit 22 ;;
esac
SH
chmod +x "$work/curl"
cat >"$work/srv.py" <<'PY'
import http.server, os, sys
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path == "/acme/policy/v1.2.3/lint/golangci-depguard.yaml":
            b = open(os.environ["UPSTREAM"], "rb").read(); self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
        else: self.send_response(404); self.end_headers()
s = http.server.HTTPServer(("127.0.0.1", 0), H); open(sys.argv[1], "w").write(str(s.server_address[1])); s.serve_forever()
PY
printf 'depguard:\n  rules:\n    main:\n      deny: []\n' >"$work/upstream.yaml"
export UPSTREAM="$work/upstream.yaml"
python3 "$work/srv.py" "$work/port" & srv=$!
for _ in $(seq 1 100); do [ -s "$work/port" ] && break; sleep 0.1; done
rawurl="http://127.0.0.1:$(cat "$work/port")"
pk_case() { # name source kitbody
  n=$((n + 1))
  local d="$work/pk$n"; mkdir -p "$d/caller-parity/kits" "$d/hack"
  { echo 'golangci-depguard.yaml:'; echo '  path: .golangci.yaml'; [ -z "$2" ] || echo "  source: $2"; } >"$d/caller-parity/kits/kits.yaml"
  printf '%s' "$3" >"$d/caller-parity/kits/golangci-depguard.yaml"
  cp "$work/pk.sh" "$d/hack/policy-kit-current.sh"
  local o w orc wrc
  o=$(PATH="$work:$PATH" bash "$d/hack/policy-kit-current.sh" 2>&1); orc=$?
  w=$(CI_ACTIONS_POLICY_RAW_URL="$rawurl" "$work/ci-actions" repo-check policy-kit "$d" 2>&1); wrc=$?
  o=${o//$d\//}; w=${w//$d\//}; w=${w//$rawurl/https://raw.githubusercontent.com}
  if [ "$o" = "$w" ] && [ "$orc" = "$wrc" ]; then echo "same  policy-kit: $1"; else
    echo "DIFF  policy-kit: $1 (old rc=$orc new rc=$wrc)"; diff <(echo "$o") <(echo "$w") | head -15; fail=1; fi
}
up=$(cat "$work/upstream.yaml")$'\n'
pk_case "a verbatim copy" "acme/policy@v1.2.3" "$up"
pk_case "a drifted copy" "acme/policy@v1.2.3" $'depguard:\n  rules:\n    main:\n      deny: [x]\n'
pk_case "no source" "" "$up"
pk_case "a source with no tag" "acme/policy" "$up"
pk_case "a tag that cannot be read" "acme/policy@v9" "$up"
echo "$n cases"
exit $fail
