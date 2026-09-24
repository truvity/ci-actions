#!/usr/bin/env bash
# caller-parity's comparison, exercised against a stub API.
#
# The check compares SUBSTANCE, and both ways of being wrong are quiet:
# too strict and every repository differs over its own prose, until the
# table is noise nobody reads; too loose and it reports parity over a
# repository that has lost a trigger — which is the defect the check
# exists to catch. So the exemptions are pinned here, each as a case
# built by transforming the real canonical copy: a repository that
# rewrote the comments, one that staggered its cron, one whose library
# pin renovate has not moved yet, one that dropped a trigger, two that
# ADDED something the kit does not have, one that does not carry the
# file at all, and reads that fail.
#
# The two "added" cases were measured, not imagined. Every case here
# until 2026-09-21 removed or rewrote something, because the public
# estate's callers are the kit. Enrolling private repositories put the
# other direction on the table: a caller that carries the kit plus its
# own `with:` inputs (a runner pool, a module proxy) and one that
# carries the kit plus a top-level block and a renamed job. Both must
# read `differs` — a comparison that only notices deletions would call
# either one parity.
#
# The action's `api-url` input exists for exactly this: the stub answers
# on localhost and caller-parity.sh cannot tell the difference.
#
#   hack/caller-parity-cases.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
kits="$root/caller-parity/kits"
work="$(mktemp -d)"
server=""
cleanup() {
  [ -n "$server" ] && kill "$server" 2>/dev/null
  rm -rf "$work"
  return 0
}
trap cleanup EXIT

# One directory per stub repository, holding what it carries under
# .github/workflows. Every case starts from the canonical copy, so a
# change to a kit cannot leave these fixtures behind.
mk() { # $1 case, $2 kit file name
  mkdir -p "$work/repos/$1"
  cat >"$work/repos/$1/$2"
}

for case in identical comments-differ cron-differs pin-differs trigger-missing extra-inputs extra-block absent forbidden repo-gone; do
  mkdir -p "$work/repos/$case"
done

# Carries both files, verbatim.
mk identical security.yaml <"$kits/security.yaml"
mk identical auto-release.yaml <"$kits/auto-release.yaml"

# Explains itself in its own words: every comment replaced, blank lines
# moved. The same workflow.
strip_comments() { sed -e 's/[[:space:]]*#.*$//' -e '/^[[:space:]]*$/d'; }
{
  echo "# Our own words about what this does, and why."
  echo
  strip_comments <"$kits/security.yaml"
} | mk comments-differ security.yaml
{
  echo "# Ditto, at length."
  echo "#"
  echo "# Several lines of it."
  strip_comments <"$kits/auto-release.yaml"
} | mk comments-differ auto-release.yaml

# The staggered minute, which is the point of the exemption.
sed -E 's/cron: "[0-9]+ [0-9]+ /cron: "37 2 /' "$kits/security.yaml" | mk cron-differs security.yaml
sed -E 's/cron: "[0-9]+ [0-9]+ /cron: "59 23 /' "$kits/auto-release.yaml" | mk cron-differs auto-release.yaml

# A library pin renovate has not moved here yet.
old_pin=0123456789abcdef0123456789abcdef01234567
sed -E "s/@[0-9a-f]{40}/@$old_pin/" "$kits/security.yaml" | mk pin-differs security.yaml
sed -E "s/@[0-9a-f]{40}/@$old_pin/" "$kits/auto-release.yaml" | mk pin-differs auto-release.yaml

# THE DEFECT THIS CHECK EXISTS FOR: the security lane's `push:` trigger,
# gone. Found by eye across thirteen repositories; nothing else would
# have said so.
cp "$kits/security.yaml" "$work/repos/trigger-missing/security.yaml"
sed -e '/^  push:$/,+1d' "$kits/auto-release.yaml" | mk trigger-missing auto-release.yaml

# ADDS inputs the kit does not pass: an estate whose repositories build
# on their own runner pool and through their own module proxy says so in
# the caller, and one of them needs a secret as well. Nothing was taken
# away, so a comparison that only looks for missing lines reads this as
# parity.
mk extra-inputs auto-release.yaml <"$kits/auto-release.yaml"
{
  cat "$kits/security.yaml"
  cat <<'EOF'
      runner: ${{ vars.CI_RUNNER_LABEL_LARGE }}
      goproxy: ${{ vars.CI_GOPROXY }}
    secrets:
      module-app-private-key: ${{ secrets.MODULE_APP_PRIVATE_KEY }}
EOF
} | mk extra-inputs security.yaml

# ADDS a top-level block and renames the job. Same workflow called with
# the same inputs, so the `uses:` line and the `with:` block match — and
# it still differs, because what runs and when is not the same.
mk extra-block auto-release.yaml <"$kits/auto-release.yaml"
sed -e 's/^  govulncheck:$/  vuln:/' \
  -e 's/^permissions:$/concurrency:\n  group: security-${{ github.ref }}\n  cancel-in-progress: true\npermissions:/' \
  "$kits/security.yaml" | mk extra-block security.yaml

# Carries security.yaml and releases nothing, so has no auto-release.yaml
# — absent, which is a state of its own and not a difference.
mk absent security.yaml <"$kits/security.yaml"

# `forbidden` and `repo-gone` carry nothing: the stub answers 403 for the
# first repository's file reads and 404 for the second's repository read.

cat >"$work/stub.py" <<'PY'
import base64, http.server, json, os, sys, urllib.parse

ROOT = sys.argv[2]


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
        # /repos/stub/<name>
        if parts[0] == "repos" and len(parts) == 3:
            if parts[2] == "repo-gone":
                return self.send(404, {"message": "Not Found"})
            return self.send(200, {"default_branch": "master"})
        # /repos/stub/<name>/contents/.github/workflows/<file>
        if parts[0] == "repos" and len(parts) > 4 and parts[3] == "contents":
            name = parts[2]
            if name == "forbidden":
                return self.send(403, {"message": "Resource not accessible by integration"})
            f = os.path.join(ROOT, name, parts[-1])
            if not os.path.exists(f):
                return self.send(404, {"message": "Not Found"})
            with open(f, "rb") as fh:
                content = base64.b64encode(fh.read()).decode()
            # GitHub wraps the payload at 60 characters; so does this.
            wrapped = "\n".join(content[i:i + 60] for i in range(0, len(content), 60)) + "\n"
            return self.send(200, {"encoding": "base64", "content": wrapped})
        self.send(404, {"message": "Not Found"})


server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
with open(sys.argv[1], "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
PY

python3 "$work/stub.py" "$work/port" "$work/repos" &
server=$!
for _ in $(seq 1 100); do [ -s "$work/port" ] && break; sleep 0.1; done
[ -s "$work/port" ] || { echo "stub API did not start"; exit 1; }

repositories='["stub/identical","stub/comments-differ","stub/cron-differs","stub/pin-differs","stub/trigger-missing","stub/extra-inputs","stub/extra-block","stub/absent","stub/forbidden","stub/repo-gone"]'

export TOKEN=stub-token KITS="$kits" FAIL_ON_DIFF=false \
  REPOSITORIES="$repositories" API="http://127.0.0.1:$(cat "$work/port")" \
  GITHUB_OUTPUT="$work/output" GITHUB_STEP_SUMMARY="$work/summary"
: >"$GITHUB_OUTPUT"
: >"$GITHUB_STEP_SUMMARY"

if ! bash "$root/caller-parity/caller-parity.sh" >"$work/log" 2>&1; then
  cat "$work/log"
  echo "::error::caller-parity.sh failed"
  exit 1
fi

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

row() { # $1 case — the summary's row, verbatim
  grep -F "| stub/$1 |" "$work/summary" || true
}

# The table is the check's whole output, so it is what the cases assert:
# `| repository | auto-release.yaml | security.yaml |` (the kits are read
# in sorted order).
check "identical is at parity" "| stub/identical | same | same |" "$(row identical)"
check "its own prose is not a difference" "| stub/comments-differ | same | same |" "$(row comments-differ)"
check "a staggered cron is not a difference" "| stub/cron-differs | same | same |" "$(row cron-differs)"
check "a library pin renovate has not moved is not a difference" "| stub/pin-differs | same | same |" "$(row pin-differs)"
check "a dropped trigger IS a difference" "| stub/trigger-missing | differs | same |" "$(row trigger-missing)"
check "an ADDED input IS a difference" "| stub/extra-inputs | same | differs |" "$(row extra-inputs)"
check "an ADDED block and a renamed job ARE a difference" "| stub/extra-block | same | differs |" "$(row extra-block)"
check "a file the repository does not carry is absent, not differing" "| stub/absent | absent | same |" "$(row absent)"
check "a 403 is unreadable, not absent and not same" "| stub/forbidden | unreadable | unreadable |" "$(row forbidden)"
check "a repository that cannot be read at all is unreadable" "| stub/repo-gone | unreadable | unreadable |" "$(row repo-gone)"

check "three differences" "differences=3" "$(grep '^differences=' "$work/output")"
check "one absent file" "absent=1" "$(grep '^absent=' "$work/output")"

check "the difference warns on the run" 1 \
  "$(grep -c '^::warning::stub/trigger-missing: .github/workflows/auto-release.yaml differs' "$work/log" || true)"
check "an unreadable file warns on the run" 1 \
  "$(grep -c '^::warning::stub/forbidden: could not read .github/workflows/auto-release.yaml' "$work/log" || true)"
check "the absent file is silent on the run" 0 \
  "$(grep -c '^::warning::stub/absent' "$work/log" || true)"

# The diff is what makes the report actionable, and it is the NORMALISED
# diff: the missing trigger, not the prose around it.
check "the normalised diff names the missing trigger" 1 \
  "$(grep -c '^-  push:$' "$work/summary" || true)"
check "the diff is not the whole file" 0 \
  "$(grep -c '^-name: Auto Release$' "$work/summary" || true)"

# The added lines are what makes the two "extra" cases readable: an
# addition has to show up as an addition, on the repository's side.
check "the normalised diff names the added input" 1 \
  "$(grep -c '^+      goproxy: ' "$work/summary" || true)"
check "the normalised diff names the added block" 1 \
  "$(grep -c '^+concurrency:$' "$work/summary" || true)"

# Report by default; a gate when the estate asks for one.
: >"$GITHUB_OUTPUT"
: >"$GITHUB_STEP_SUMMARY"
if FAIL_ON_DIFF=true bash "$root/caller-parity/caller-parity.sh" >"$work/log-fail" 2>&1; then
  check "fail-on-diff fails the job" "exit 1" "exit 0"
else
  check "fail-on-diff fails the job" "exit 1" "exit 1"
fi

# A kit turned off in the manifest is NOT compared, and the report says
# which. That lever is the only thing between "shipped disabled" and
# "shipped and quietly doing nothing".
check "the disabled kit is not a column" 0 \
  "$(grep -c 'golangci-depguard.yaml |' "$work/summary" || true)"
check "the report names what it did not compare" 1 \
  "$(grep -c 'turned off in .*kits.yaml.* and NOT compared: golangci-depguard.yaml' "$work/summary" || true)"

# ---------------------------------------------------------------------
# A kit that is a BLOCK inside a file the repository also fills with its
# own settings.
#
# golangci-lint v2 cannot extend a configuration from a URL, so the
# import bans are a hand copy in every Go repository that has them. What
# a copy has to keep is the DATA: a repository that reindents it, writes
# its own comments around it or orders the keys differently has the same
# bans, and a comparison that called any of those a difference would be
# noise nobody reads. A repository missing one `deny` entry does not have
# the same bans, and neither does one that dropped the block entirely --
# and "dropped the block" is not "absent", because the file it belongs in
# is right there.
#
# Run with the kit ENABLED, which is not how it ships. The shipped
# manifest turns it off until the estate-wide pass; these cases are what
# make turning it on a one-line change rather than a first run in anger.

lintkits="$work/kits-lint"
mkdir -p "$lintkits"
cp "$kits/golangci-depguard.yaml" "$lintkits/golangci-depguard.yaml"
sed -e 's/^  enabled: false$/  enabled: true/' "$kits/kits.yaml" >"$lintkits/kits.yaml"

# The block as a repository carries it: under `linters.settings`, beside
# that repository's own settings.
their_config() { # stdin: the depguard fragment
  {
    echo 'version: "2"'
    echo
    echo 'linters:'
    echo '  enable:'
    echo '    - depguard'
    echo
    echo '  settings:'
    sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' -e 's/^/    /'
  }
}

for case in lint-identical lint-reindented lint-entry-missing lint-block-absent lint-file-absent; do
  mkdir -p "$work/repos/$case"
done

their_config <"$kits/golangci-depguard.yaml" >"$work/repos/lint-identical/.golangci.yaml"

# Its own comments, its own blank lines, and flow style for one entry:
# the same data, written differently.
{
  echo '# Our lint configuration. The import bans below are copied from'
  echo '# the canonical block and kept in step by hand.'
  their_config <"$kits/golangci-depguard.yaml"
} >"$work/repos/lint-reindented/.golangci.yaml"

# One ban gone. THE DEFECT THIS CHECK EXISTS FOR: a repository that fell
# behind the canonical copy, with nothing to say so.
sed -e '/pkg: github.com\/pkg\/errors/,+1d' "$kits/golangci-depguard.yaml" \
  | their_config >"$work/repos/lint-entry-missing/.golangci.yaml"

# A lint configuration with no import bans at all. The file is there, so
# this is a difference and not an absence.
cat >"$work/repos/lint-block-absent/.golangci.yaml" <<'EOF'
version: "2"

linters:
  enable:
    - errcheck
EOF

# `lint-file-absent` carries nothing: no .golangci.yaml, which is correct
# for a repository with no Go in it.

: >"$GITHUB_OUTPUT"
: >"$GITHUB_STEP_SUMMARY"
if ! KITS="$lintkits" FAIL_ON_DIFF=false \
  REPOSITORIES='["stub/lint-identical","stub/lint-reindented","stub/lint-entry-missing","stub/lint-block-absent","stub/lint-file-absent"]' \
  bash "$root/caller-parity/caller-parity.sh" >"$work/log-lint" 2>&1; then
  cat "$work/log-lint"
  echo "::error::caller-parity.sh failed on the block kit"
  exit 1
fi

check "a verbatim block is at parity" "| stub/lint-identical | same |" "$(row lint-identical)"
check "its own comments and indentation are not a difference" "| stub/lint-reindented | same |" "$(row lint-reindented)"
check "a missing ban IS a difference" "| stub/lint-entry-missing | differs |" "$(row lint-entry-missing)"
check "a file with no block at all differs, it is not absent" "| stub/lint-block-absent | differs |" "$(row lint-block-absent)"
check "a repository with no lint configuration is absent" "| stub/lint-file-absent | absent |" "$(row lint-file-absent)"

check "two block differences" "differences=2" "$(grep '^differences=' "$GITHUB_OUTPUT")"
check "one absent configuration" "absent=1" "$(grep '^absent=' "$GITHUB_OUTPUT")"

# The warning names the path the manifest gave, not the default one.
check "the warning names the repository's own path" 1 \
  "$(grep -c '^::warning::stub/lint-entry-missing: .golangci.yaml differs' "$work/log-lint" || true)"

# The diff is over the DATA, so it names the ban rather than a line of
# YAML -- which is what makes it actionable in a repository that writes
# the block differently.
# The diff is over the DATA, so it names the ban rather than a line of
# YAML -- which is what makes it actionable in a repository that writes
# the block differently. Scoped to one repository's diff block, because
# the block-absent case legitimately removes every ban and would make a
# whole-summary count say nothing.
diff_for() { # $1 case — that repository's diff block
  awk -v want="+++ stub/$1 " '
    index($0, want) == 1 { inside = 1; next }
    inside && $0 == "```" { exit }
    inside { print }
  ' "$GITHUB_STEP_SUMMARY"
}

check "the diff shows the missing ban as REMOVED, and only that ban" 1 \
  "$(diff_for lint-entry-missing | grep -c '^-.*\"pkg\"' || true)"
check "the missing ban is named" 1 \
  "$(diff_for lint-entry-missing | grep -c '^-.*github.com/pkg/errors' || true)"
check "nothing is reported as added" 0 \
  "$(diff_for lint-entry-missing | grep -c '^+.*\"pkg\"' || true)"
check "a repository with no block at all loses every ban" 1 \
  "$([ "$(diff_for lint-block-absent | grep -c '^-.*\"pkg\"' || true)" -gt 5 ] && echo 1 || echo 0)"

[ "$fail" = 0 ] || { echo "::error::caller-parity does not compare as documented"; exit 1; }
echo "all cases pass"
