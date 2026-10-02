#!/usr/bin/env bash
# TEMPORARY, removed before merge: the inline-shell openbao-secrets (the run
# body of the action.yaml at the base the pull request branched from) against
# the Go port, on one stub OpenBAO and several scenarios. Compared per
# scenario: stdout (every ::add-mask:: and ::error:: line, in order), the
# env file, the step outputs (the heredoc delimiter normalised), what the
# server saw (method, path, namespace, whether a token was presented, the
# login body as parsed JSON), and the exit status as zero/non-zero.
set -uo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
base="${PARITY_BASE:-origin/master}"
work=$(mktemp -d)
server=""
trap '[ -n "$server" ] && kill "$server" 2>/dev/null; [ -n "${KEEP:-}" ] || rm -rf "$work"; [ -z "${KEEP:-}" ] || echo "kept $work"' EXIT
command -v yq >/dev/null && command -v openssl >/dev/null || { echo "::error::yq and openssl are required"; exit 2; }
git -C "$here" show "$base:openbao-secrets/action.yaml" >"$work/old-action.yaml" || { echo "::error::cannot read the old action at $base"; exit 2; }
yq -r '.runs.steps[0].run' "$work/old-action.yaml" >"$work/old.sh"
(cd "$here" && CGO_ENABLED=0 go build -o "$work/ci-actions" ./cmd/ci-actions) || exit 2

stubs="$work/stubs"; mkdir -p "$stubs"
cat >"$stubs/accessctl" <<'SH'
#!/usr/bin/env bash
[ -z "${ACCESSCTL_FAIL:-}" ] || { echo "accessctl: denied" >&2; exit 1; }
[ -z "${ACCESSCTL_EMPTY:-}" ] || { echo "Info: nothing"; echo; exit 0; }
echo "Info: exchanging for $*"
printf 'eyJ.job-identity.token\r\n'
SH
chmod +x "$stubs/accessctl"

openssl req -x509 -newkey rsa:2048 -nodes -keyout "$work/key.pem" -out "$work/ca.pem" -days 1 \
  -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1 || { echo "::error::openssl failed"; exit 2; }

cat >"$work/vault.py" <<'PY'
import http.server, json, os, ssl, sys
LOG = sys.argv[2]
DATA = json.loads(os.environ["KV_DATA"])
LOGIN_CODE = int(os.environ.get("LOGIN_CODE", "200"))
READ_CODE = int(os.environ.get("READ_CODE", "200"))
CT = "hvs.client-token-value"
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, st, body):
        b = body.encode() if isinstance(body, str) else json.dumps(body).encode()
        self.send_response(st); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def note(self, extra=""):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode() if n else ""
        try: body = json.dumps(json.loads(body), sort_keys=True) if body else ""
        except Exception: pass
        with open(LOG, "a") as f:
            f.write("%s %s ns=%s token=%s %s\n" % (self.command, self.path, self.headers.get("X-Vault-Namespace", ""),
                    "yes" if self.headers.get("X-Vault-Token") == CT else ("other" if self.headers.get("X-Vault-Token") else "no"), body))
    def do_PUT(self):
        self.note()
        if LOGIN_CODE != 200: return self.send(LOGIN_CODE, {"errors": ["permission denied"]})
        if os.environ.get("LOGIN_NOTOKEN"): return self.send(200, {"auth": None})
        self.send(200, {"auth": {"client_token": CT, "policies": ["default", "ci-read"]}})
    def do_POST(self):
        self.note(); self.send(204, "")
    def do_GET(self):
        self.note()
        if READ_CODE != 200: return self.send(READ_CODE, {"errors": ["permission denied"]})
        self.send(200, {"data": {"data": DATA, "metadata": {"version": 1}}})
s = http.server.HTTPServer(("127.0.0.1", 0), H)
if os.environ.get("TLS"):
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); ctx.load_cert_chain(os.environ["CERT"], os.environ["KEY"])
    s.socket = ctx.wrap_socket(s.socket, server_side=True)
open(sys.argv[1], "w").write(str(s.server_address[1]))
s.serve_forever()
PY

fail=0
n=0
# scenario <name> <KV_DATA json> [VAR=value ...]  (inputs: ISSUER ADDRESS KV_PATH BAO_NAMESPACE MOUNT AUTH_MOUNT ROLE AUDIENCE WANTED CA_CERT ACCESSCTL_MODE)
scenario() {
  local name=$1 data=$2
  shift 2
  n=$((n + 1))
  local which
  for which in old new; do
    local d="$work/s$n-$which"
    mkdir -p "$d/tmp" "$d/cwd"
    : >"$d/output"; : >"$d/vault.log"
    (
      export KV_DATA="$data" CERT="$work/ca.pem" KEY="$work/key.pem"
      for kv in "$@"; do export "${kv?}"; done
      python3 "$work/vault.py" "$d/port" "$d/vault.log" & srv=$!
      for _ in $(seq 1 100); do [ -s "$d/port" ] && break; sleep 0.1; done
      port=$(cat "$d/port")
      scheme=http; [ -n "${TLS:-}" ] && scheme=https
      export ISSUER="${ISSUER-https://issuer.example}" ADDRESS="${ADDRESS-$scheme://localhost:$port}" KV_PATH="${KV_PATH-ci/goreleaser}"
      export BAO_NAMESPACE="${BAO_NAMESPACE-}" MOUNT="${MOUNT:-kv}" AUTH_MOUNT="${AUTH_MOUNT:-jwt-roster}" ROLE="${ROLE:-roster}" AUDIENCE="${AUDIENCE:-openbao}"
      export WANTED="${WANTED-}" CA_CERT="${CA_CERT-}" ACCESSCTL_MODE="${ACCESSCTL_MODE:-auto}"
      [ -n "${NO_IDTOKEN:-}" ] || export ACTIONS_ID_TOKEN_REQUEST_URL=http://127.0.0.1:1/token
      export RUNNER_TEMP="$d/tmp" GITHUB_OUTPUT="$d/output" PATH="$stubs:$PATH"
      if [ "${CA_CERT_FILE:-}" = 1 ]; then export CA_CERT; CA_CERT=$(cat "$work/ca.pem"); fi
      cd "$d/cwd" || exit 1
      if [ $which = old ]; then
        bash --noprofile --norc -e -o pipefail "$work/old.sh" >"$d/stdout" 2>"$d/stderr"
      else
        ACTION_PATH=unused "$work/ci-actions" openbao-secrets >"$d/stdout" 2>"$d/stderr"
      fi
      echo "rc-zero=$([ $? = 0 ] && echo yes || echo no)" >"$d/rc"
      kill $srv 2>/dev/null
    )
    # normalise: the temp dir, the server port, the heredoc delimiter
    local port; port=$(cat "$d/port")
    sed -E -e "s#$d#D#g" -e "s#$port#PORT#g" "$d/stdout" >"$d/stdout.norm"
    sed -E -e "s#$d#D#g" -e 's#^value<<openbao-[0-9a-z-]+$#value<<DELIM#' -e 's#^openbao-[0-9a-z-]+$#DELIM#' "$d/output" >"$d/output.norm"
    sed -E "s#$port#PORT#g" "$d/vault.log" >"$d/vault.norm"
    { cat "$d"/tmp/*.env 2>/dev/null || echo "(no env file)"; stat -c '%a' "$d"/tmp/*.env 2>/dev/null || true; } >"$d/env.norm"
  done
  local o="$work/s$n-old" w="$work/s$n-new" same=1 part
  for part in stdout.norm output.norm vault.norm env.norm rc; do cmp -s "$o/$part" "$w/$part" || same=0; done
  # a secret is never printed outside ::add-mask::, by either version
  if grep -h -v '^::add-mask::' "$w/stdout" "$w/stderr" | grep -E 'client-token-value|job-identity|s3cret-|hunter2|PRIVATE KEY'; then echo "LEAK in $name"; same=0; fi
  if [ $same = 1 ]; then
    printf 'same  %s\n' "$name"
  else
    printf 'DIFF  %s\n' "$name"
    for part in stdout.norm output.norm vault.norm env.norm rc; do diff "$o/$part" "$w/$part" | head -20; done
    fail=1
  fi
}

scenario "a single value, in a namespace" '{"GORELEASER_KEY":"s3cret-key-1234"}' BAO_NAMESPACE=team/ci
scenario "several keys, quotes and a multi-line secret" '{"maven-username":"deploy-bot","maven-password":"s3cret '"'"'quoted'"'"' \"p\" $(x)","tls":"-----BEGIN PRIVATE KEY-----\nabcdefghij\nxy\n-----END PRIVATE KEY-----"}'
scenario "keys picks, in order" '{"a-key":"s3cret-a-value","b-key":"s3cret-b-value","c-key":"s3cret-c-value"}' 'WANTED=c-key, a-key'
scenario "a key that is not there" '{"a-key":"s3cret-a-value"}' WANTED=nope
scenario "non-string values" '{"n":42,"flag":true,"nothing":null,"obj":{"a":[1,2],"b":"x"},"z":"s3cret-z-value"}'
scenario "an empty path" '{}'
scenario "no environment variable name" '{"9bad":"s3cret-value"}'
scenario "login refused" '{"k":"s3cret-value"}' LOGIN_CODE=403
scenario "login without a token" '{"k":"s3cret-value"}' LOGIN_NOTOKEN=1
scenario "read refused, login revoked" '{"k":"s3cret-value"}' READ_CODE=403
scenario "accessctl refused" '{"k":"s3cret-value"}' ACCESSCTL_FAIL=1
scenario "accessctl returns nothing" '{"k":"s3cret-value"}' ACCESSCTL_EMPTY=1
scenario "no id-token permission" '{"k":"s3cret-value"}' NO_IDTOKEN=1
scenario "an input missing" '{"k":"s3cret-value"}' ISSUER=
scenario "a private CA" '{"k":"s3cret-value-tls"}' TLS=1 CA_CERT_FILE=1
scenario "custom mounts, role and audience" '{"k":"s3cret-value"}' MOUNT=kv AUTH_MOUNT=jwt-roster ROLE=ci AUDIENCE=bao
echo "$n scenarios"
exit $fail
