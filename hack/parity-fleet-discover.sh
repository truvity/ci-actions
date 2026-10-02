#!/usr/bin/env bash
# TEMPORARY, removed before merge: the shell fleet-discover (read from the
# base the pull request branched from) against the Go port, on one stub API
# and several input sets, comparing stdout, the step summary and the
# GITHUB_OUTPUT file byte for byte, and the exit status as zero/non-zero.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
server=""
trap '[ -n "$server" ] && kill "$server" 2>/dev/null; rm -rf "$work"' EXIT
git -C "$here" show "$base:fleet-discover/discover.sh" >"$work/old.sh" || { echo "::error::cannot read the old script at $base"; exit 2; }
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2

cat >"$work/stub.py" <<'PY'
import http.server, json, sys, urllib.parse

def rsc(c): return {"type": "required_status_checks", "parameters": {"required_status_checks": [{"context": x} for x in c]}}
NP = (404, {"message": "Branch not protected"})
HID = (404, {"message": "Not Found"})
FORB = (403, {"message": "Resource not accessible by integration"})
G = {  # name: (visibility, archived, rules, protection, graphql, has devbox.json)
 "classic-only": ("private", False, (200, []), (200, {"required_status_checks": {"contexts": ["check"]}}), None, True),
 "ruleset-only": ("private", False, (200, [rsc(["check"])]), NP, None, True),
 "both": ("private", False, (200, [rsc(["a", "b"])]), (200, {"required_status_checks": {"contexts": ["check"]}}), ["check"], True),
 "neither": ("private", False, (200, []), NP, None, True),
 "ruleset-no-checks": ("private", False, (200, [{"type": "pull_request"}]), NP, None, True),
 "protection-no-checks": ("private", False, (200, []), (200, {"enforce_admins": {}}), None, True),
 "protection-hidden": ("private", False, (200, []), HID, ["check"], True),
 "protection-forbidden": ("private", False, (200, []), FORB, ["check"], True),
 "rules-404": ("private", False, HID, NP, None, True),
 "rules-403": ("private", False, FORB, NP, None, True),
 "classic-unreadable": ("private", False, (200, []), FORB, "403", True),
 "graphql-errors": ("private", False, (200, []), FORB, "errors", True),
 "pub-gated": ("public", False, (200, [rsc(["check"])]), NP, None, True),
 "pub-open": ("public", False, (200, []), NP, None, False),
 "archived-gated": ("private", True, (200, [rsc(["check"])]), NP, None, True),
 "nofile": ("private", False, (200, [rsc(["check"])]), NP, None, False),
}
for i in range(120):  # a second page of the installation
    G["fill-%03d" % i] = ("private", False, (200, [rsc(["check"])]), NP, None, i % 2 == 0)
NAMES = list(G)

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, st, p):
        b = json.dumps(p).encode()
        self.send_response(st); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        u = urllib.parse.urlsplit(self.path); parts = u.path.strip("/").split("/")
        if u.path == "/installation/repositories":
            page = int(urllib.parse.parse_qs(u.query).get("page", ["1"])[0])
            return self.send(200, {"repositories": [
                {"full_name": "stub/" + n, "visibility": G[n][0], "archived": G[n][1], "default_branch": "master", "extra": {"big": "x" * 50}}
                for n in NAMES[(page - 1) * 100: page * 100]]})
        if parts[0] == "repos" and len(parts) == 6 and parts[3] == "rules": return self.send(*G[parts[2]][2])
        if parts[0] == "repos" and len(parts) == 6 and parts[5] == "protection": return self.send(*G[parts[2]][3])
        if parts[0] == "repos" and len(parts) >= 5 and parts[3] == "contents":
            return self.send(200, {"name": "x"}) if parts[4] == "devbox.json" and G[parts[2]][5] else self.send(*HID)
        self.send(404, {"message": "Not Found"})
    def do_POST(self):
        n = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))["variables"]["r"]
        c = G[n][4]
        if c == "403": return self.send(*FORB)
        if c == "errors": return self.send(200, {"errors": [{"message": "x"}], "data": {"repository": None}})
        rule = None if c is None else {"requiredStatusCheckContexts": c}
        self.send(200, {"data": {"repository": {"defaultBranchRef": {"refUpdateRule": rule}}}})

s = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(s.server_address[1]))
s.serve_forever()
PY
python3 "$work/stub.py" "$work/port" &
server=$!
for _ in $(seq 1 100); do [ -s "$work/port" ] && break; sleep 0.1; done
[ -s "$work/port" ] || { echo "stub API did not start"; exit 2; }
api="http://127.0.0.1:$(cat "$work/port")"

fail=0
n=0
# case_ name ESTATE REQUIRE_CHECK REQUIRE_FILE FILTER ENROLLED
case_() {
  n=$((n + 1))
  local name=$1 estate=$2 rc=$3 rf=$4 filter=$5 enrolled=$6 which out
  for which in old new; do
    : >"$work/$which.output"
    : >"$work/$which.summary"
    local envs=(env -i PATH="$PATH" TOKEN=stub-token ESTATE="$estate" REQUIRE_CHECK="$rc" REQUIRE_FILE="$rf" FILTER="$filter" ENROLLED="$enrolled" API="$api" GITHUB_OUTPUT="$work/$which.output" GITHUB_STEP_SUMMARY="$work/$which.summary")
    if [ $which = old ]; then
      out=$("${envs[@]}" bash "$work/old.sh" 2>&1)
    else
      out=$("${envs[@]}" "$work/ci-actions" fleet discover 2>&1)
    fi
    printf '%s\nrc-zero=%s\n' "$out" "$([ $? = 0 ] && echo yes || echo no)" >"$work/$which.log"
  done
  local same=1
  for part in log output summary; do
    cmp -s "$work/old.$part" "$work/new.$part" || same=0
  done
  if [ $same = 1 ]; then
    printf 'same  %s\n' "$name"
  else
    printf 'DIFF  %s\n' "$name"
    for part in log output summary; do diff "$work/old.$part" "$work/new.$part" | head -20; done
    fail=1
  fi
}
case_ "everything, both rules, file" all true devbox.json "" ""
case_ "private only" private true devbox.json "" ""
case_ "public only" public true "" "" ""
case_ "no check rule" all false "" "" ""
case_ "file rule on a missing file" all false renovate.json "" ""
case_ "filter" all true devbox.json '^stub/(both|neither|fill-00[0-3])$' ""
case_ "enrolled, one unknown" all true devbox.json "" '["both","neither","ghost","classic-only"]'
case_ "enrolled with filter" all false "" '^stub/pub' '["pub-gated","pub-open","both"]'
case_ "enrolled all unknown" all true "" "" '["ghost","ghost2"]'
case_ "bad estate" everything true "" "" ""
case_ "bad enrolled (object)" all true "" "" '{"a":1}'
case_ "bad enrolled (empty array)" all true "" "" '[]'
echo "$n cases"
exit $fail
