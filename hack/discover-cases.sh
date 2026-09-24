#!/usr/bin/env bash
# fleet-discover's required-check rule, exercised against a stub API.
#
# The rule decides whether a repository is processed at all, and a wrong
# answer is invisible: a skipped repository looks exactly like a quiet
# one. It reads two independent sources — a repository ruleset and
# classic branch protection, the latter through two endpoints — so the
# shapes it has to survive are pinned down here rather than in a fleet
# run: either source alone, both, neither, a ruleset with rules but no
# status-check rule, a protection object with no required checks, a 404
# that means "not protected", a 404 that means "not yours to read", a
# 403.
#
# The action's `api-url` input exists for exactly this: the stub answers
# on localhost and discover.sh cannot tell the difference.
#
#   hack/discover-cases.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
server=""
cleanup() {
  [ -n "$server" ] && kill "$server" 2>/dev/null
  rm -rf "$work"
  return 0
}
trap cleanup EXIT

cat >"$work/stub.py" <<'PY'
import http.server, json, sys, urllib.parse


def rsc(contexts):
    return {
        "type": "required_status_checks",
        "parameters": {"required_status_checks": [{"context": c} for c in contexts]},
    }


NOT_PROTECTED = (404, {"message": "Branch not protected"})
HIDDEN = (404, {"message": "Not Found"})
FORBIDDEN = (403, {"message": "Resource not accessible by integration"})

# One case per repository in the stub installation: what the effective-
# rules endpoint answers, what the classic protection endpoint answers,
# and what GraphQL's refUpdateRule answers when the reader falls back to
# it. `classic` is None for no rule, a list of contexts, or "403".
CASES = {
    # Either source alone is a gate, and so is both.
    "classic-only":        {"rules": (200, []),               "protection": (200, {"required_status_checks": {"contexts": ["check"]}}),               "classic": None},
    "ruleset-only":        {"rules": (200, [rsc(["check"])]), "protection": NOT_PROTECTED,                                                            "classic": None},
    "both":                {"rules": (200, [rsc(["check"])]), "protection": (200, {"required_status_checks": {"contexts": ["check", "integration"]}}), "classic": ["check"]},
    # Nothing requires a check: a skip, with both sources named.
    "neither":             {"rules": (200, []),               "protection": NOT_PROTECTED,                                                            "classic": None},
    "ruleset-no-checks":   {"rules": (200, [{"type": "pull_request", "parameters": {}}]), "protection": NOT_PROTECTED,                                "classic": None},
    "protection-no-checks": {"rules": (200, []),              "protection": (200, {"enforce_admins": {"enabled": True}}),                             "classic": None},
    # Classic protection this token may not read: the fallback answers.
    "protection-hidden":   {"rules": (200, []),               "protection": HIDDEN,                                                                   "classic": ["check"]},
    "protection-forbidden": {"rules": (200, []),              "protection": FORBIDDEN,                                                                "classic": ["check"]},
    # A read that failed, on either side: undecided, never a skip.
    "rules-404":           {"rules": HIDDEN,                  "protection": NOT_PROTECTED,                                                            "classic": None},
    "rules-403":           {"rules": FORBIDDEN,               "protection": NOT_PROTECTED,                                                            "classic": None},
    "classic-unreadable":  {"rules": (200, []),               "protection": FORBIDDEN,                                                                "classic": "403"},
}


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        parts = path.strip("/").split("/")
        if path == "/installation/repositories":
            return self.send(200, {"repositories": [
                {"full_name": "stub/" + name, "visibility": "private",
                 "archived": False, "default_branch": "master"}
                for name in CASES
            ]})
        if parts[0] == "repos" and len(parts) == 6:
            # /repos/stub/<name>/rules/branches/master
            if parts[3] == "rules":
                return self.send(*CASES[parts[2]]["rules"])
            # /repos/stub/<name>/branches/master/protection
            if parts[3] == "branches" and parts[5] == "protection":
                return self.send(*CASES[parts[2]]["protection"])
        # /repos/stub/<name>/contents/devbox.json — every case opted in
        if parts[0] == "repos" and len(parts) >= 5 and parts[3] == "contents":
            return self.send(200, {"name": parts[4]})
        self.send(404, {"message": "Not Found"})

    def do_POST(self):
        if urllib.parse.urlsplit(self.path).path != "/graphql":
            return self.send(404, {"message": "Not Found"})
        length = int(self.headers.get("Content-Length", 0))
        name = json.loads(self.rfile.read(length))["variables"]["r"]
        classic = CASES[name]["classic"]
        if classic == "403":
            return self.send(*FORBIDDEN)
        rule = None if classic is None else {"requiredStatusCheckContexts": classic}
        self.send(200, {"data": {"repository": {"defaultBranchRef": {"refUpdateRule": rule}}}})


server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
with open(sys.argv[1], "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
PY

python3 "$work/stub.py" "$work/port" &
server=$!
for _ in $(seq 1 100); do [ -s "$work/port" ] && break; sleep 0.1; done
[ -s "$work/port" ] || { echo "stub API did not start"; exit 1; }

export TOKEN=stub-token ESTATE=all REQUIRE_CHECK=true REQUIRE_FILE=devbox.json \
  FILTER="" ENROLLED="" API="http://127.0.0.1:$(cat "$work/port")" \
  GITHUB_OUTPUT="$work/output" GITHUB_STEP_SUMMARY="$work/summary"
: >"$GITHUB_OUTPUT"
: >"$GITHUB_STEP_SUMMARY"

if ! bash "$root/fleet-discover/discover.sh" >"$work/log" 2>&1; then
  cat "$work/log"
  echo "::error::discover.sh failed"
  exit 1
fi

kept="$(sed -n 's/^repositories=//p' "$work/output")"
fail=0

check() { # $1 what, $2 expected, $3 actual
  if [ "$2" = "$3" ]; then
    echo "ok    $1"
  else
    echo "FAIL  $1"
    echo "      expected: $2"
    echo "      actual:   $3"
    fail=1
  fi
}

kept_case() { # $1 name — processed, with the rule decided
  check "$1 is processed" true \
    "$(jq --arg r "stub/$1" 'index($r) != null' <<<"$kept")"
  check "$1 needed no undecided note" 0 \
    "$(grep -c -e "^- stub/$1 — " "$work/summary" || true)"
}

skipped_case() { # $1 name, $2 the reason column, verbatim
  check "$1 is skipped" false \
    "$(jq --arg r "stub/$1" 'index($r) != null' <<<"$kept")"
  check "$1 says why" "| stub/$1 | $2 |" \
    "$(grep -F "| stub/$1 |" "$work/summary" || true)"
}

undecided_case() { # $1 name, $2 the detail, verbatim
  check "$1 is processed although the rule is undecided" true \
    "$(jq --arg r "stub/$1" 'index($r) != null' <<<"$kept")"
  check "$1 is reported as undecided" "- stub/$1 — $2" \
    "$(grep -F -e "- stub/$1 — " "$work/summary" || true)"
  check "$1 warns on the run" 1 \
    "$(grep -c -e "^::warning::stub/$1: could not decide" "$work/log" || true)"
}

none="no required status check on master (rulesets: none, classic protection: none)"

kept_case classic-only
kept_case ruleset-only
kept_case both
kept_case protection-hidden
kept_case protection-forbidden

skipped_case neither "$none"
skipped_case ruleset-no-checks "$none"
skipped_case protection-no-checks "$none"

undecided_case rules-404 "rulesets: could not read, classic protection: none"
undecided_case rules-403 "rulesets: could not read, classic protection: none"
undecided_case classic-unreadable "rulesets: none, classic protection: could not read"

check "kept count" 8 "$(jq 'length' <<<"$kept")"

[ "$fail" = 0 ] || { echo "::error::fleet-discover's required-check rule does not behave as documented"; exit 1; }
echo "all cases pass"
