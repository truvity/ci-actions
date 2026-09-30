#!/usr/bin/env bash
# Hold a repository's checkout against the component contract's checkable
# rules, C1 to C13, and print one line per rule:
#
#   C5 FAIL: CHANGELOG.md has no heading for v1.2.3
#
# The contract itself lives in truvity/policy (docs/contracts/component.md);
# this file is the mechanical half of it and carries no doctrine of its
# own. Where a rule needs judgement the script cannot make, it says what it
# looked at rather than pretending to certainty.
#
# Runs from the root of the repository being judged. Needs bash, git, jq,
# grep, sed, awk and sort -- nothing a hosted runner lacks, and no token:
# the only network call is `git fetch --tags` against the caller's own
# origin, and a failure there downgrades one sub-check, never the run.
#
#   STRICT          "true" exits 1 when any rule fails; anything else
#                   reports failures as warnings and exits 0.
#   SKIP            comma- or space-separated rule ids not to evaluate.
#   REASON          why; required whenever SKIP is not empty.
#   RENOVATE_PRESET the preset C7 expects `extends` to name.
#   DEFAULT_BRANCH  the branch C5 finds the latest tag from.
set -uo pipefail

STRICT=${STRICT:-false}
SKIP=${SKIP:-}
REASON=${REASON:-}
RENOVATE_PRESET=${RENOVATE_PRESET:-github>truvity/ci-workflows}
DEFAULT_BRANCH=${DEFAULT_BRANCH:-}

RULES=(C1 C2 C3 C4 C5 C6 C7 C8 C9 C10 C11 C12 C13)

# The README headings C8 asks for, in this order. Other headings may sit
# between them; these must all be present and must not be reordered.
README_HEADINGS=(
  "Who it is for"
  "The model"
  "Install and a worked example"
  "Consumers"
  "Neighbours"
  "Documentation"
  "The rule that makes this repository public"
  "Status"
  "Development"
  "Releasing"
  "Licence"
)

shopt -s nullglob

results=()
failed=0

emit() {
  local id=$1 verdict=$2 msg=$3
  local line="$id $verdict: $msg"
  echo "$line"
  results+=("$line")
  if [ "$verdict" = FAIL ]; then
    failed=$((failed + 1))
    if [ "$STRICT" = true ]; then
      echo "::error title=policy-conformance ${id}::${msg}"
    else
      echo "::warning title=policy-conformance ${id}::${msg}"
    fi
  fi
}

# Several problems under one rule still make one line.
verdict() {
  local id=$1 ok_msg=$2
  shift 2
  if [ $# -eq 0 ]; then
    emit "$id" PASS "$ok_msg"
  else
    local joined
    joined=$(printf '%s; ' "$@")
    emit "$id" FAIL "${joined%; }"
  fi
}

# A top-level scalar from a YAML file, quotes stripped. Chart.yaml keeps
# `version:` and `appVersion:` at column 0, which is all this reads.
top_key() {
  sed -n -E "s/^$2:[[:space:]]*[\"']?([^\"'#[:space:]]*)[\"']?.*$/\1/p" "$1" | head -1
}

# ── exemptions · .github/policy-conformance.yaml ─────────────────────────
#
# A named, reviewed exception to one rule, for a repository kind the
# contract's plain text does not fit (a library chart has no values to
# schema; a CRD chart republished from upstream carries the upstream's own
# version; a fork of a non-MIT upstream cannot relicense; a CHANGELOG whose
# own preamble documents that a dependency-only patch carries no heading).
# This is not the caller-side `skip:` input: that silences a whole rule for
# one run and demands a reason every time. An exemption here is committed,
# reviewed, and — where the rule is chart-scoped — can name just the charts
# it covers rather than the whole repository.
#
# Shape (a restricted, line-oriented subset of YAML; no flow collections
# other than `charts: [a, b, c]`):
#
#   exempt:
#     C2:
#       reason: library chart takes no values
#       charts: [gateway-routes]
#     C9:
#       reason: fork of an Apache-2.0 upstream; cannot relicense
#
# `reason` is required. `charts`, where present, scopes the exemption to
# those chart directories under charts/*; a rule with no `charts:` line is
# exempted for the whole repository.
EXEMPT_FILE=.github/policy-conformance.yaml

# The reason string for a rule, or empty if it carries no exemption.
exempt_reason() {
  local id=$1
  [ -f "$EXEMPT_FILE" ] || return 0
  awk -v id="$id" '
    /^exempt:/ { in_exempt=1; next }
    in_exempt && /^[^[:space:]]/ { in_exempt=0 }
    in_exempt && $0 ~ "^  " id ":[[:space:]]*$" { in_rule=1; next }
    in_exempt && in_rule && /^  [A-Za-z]/ { in_rule=0 }
    in_exempt && in_rule && /^    reason:[[:space:]]*/ {
      sub(/^    reason:[[:space:]]*/, "")
      print
      exit
    }
  ' "$EXEMPT_FILE"
}

# Comma-separated members of a flow list (`key: [a, b]`) under a rule's
# exemption, or empty when the rule has no such line.
exempt_list() {
  local id=$1 key=$2
  [ -f "$EXEMPT_FILE" ] || return 0
  awk -v id="$id" -v key="$key" '
    /^exempt:/ { in_exempt=1; next }
    in_exempt && /^[^[:space:]]/ { in_exempt=0 }
    in_exempt && $0 ~ "^  " id ":[[:space:]]*$" { in_rule=1; next }
    in_exempt && in_rule && /^  [A-Za-z]/ { in_rule=0 }
    in_exempt && in_rule && $0 ~ "^    " key ":[[:space:]]*\\[" {
      sub("^    " key ":[[:space:]]*\\[", "")
      sub(/\].*$/, "")
      print
      exit
    }
  ' "$EXEMPT_FILE" | tr -d ' '
}

# Comma-separated chart names a rule's exemption is scoped to, or empty
# (meaning: the whole repository) when the rule has no `charts:` line.
exempt_charts() { exempt_list "$1" charts; }

# True (rc 0) when $2 is named in the comma-separated chart list $1, or
# when $1 is empty (an unscoped, whole-repository exemption).
chart_in() {
  local list=$1 chart=$2
  [ -n "$list" ] || return 0
  case ",$list," in *",$chart,"*) return 0 ;; esac
  return 1
}

# ── C1 · charts commit 0.0.0 ─────────────────────────────────────────────

# Best-effort: does this repository build and publish an image at all? C1
# only asks appVersion to be the tag placeholder "when the repo ships an
# image" — a chart-only repository (goreleaser's builds all `skip: true`,
# no ko section, no Dockerfile) has no image, so an appVersion it carries
# names something else (an upstream compatibility pin) and is out of scope.
repo_ships_image() {
  local gf=""
  for c in .goreleaser.yaml .goreleaser.yml; do
    [ -f "$c" ] && { gf=$c; break; }
  done
  [ -n "$gf" ] || { [ -f Dockerfile ] || [ -f .ko.yaml ]; return; }
  grep -qE '^kos:' "$gf" && return 0
  [ -f Dockerfile ] && return 0
  local block
  block=$(awk '/^builds:/{f=1; next} /^[A-Za-z_]+:/{f=0} f' "$gf")
  [ -n "$block" ] || return 0
  # Every build entry in this file is an explicit no-op.
  if grep -qE '^\s*-\s*id:' <<<"$block"; then
    return 0
  fi
  if grep -qE '^\s*-?\s*skip:\s*true\s*$' <<<"$block" \
      && ! grep -vE '^\s*-?\s*skip:\s*true\s*$|^\s*$' <<<"$block" >/dev/null; then
    return 1
  fi
  return 0
}

rule_C1() {
  local charts=(charts/*/Chart.yaml) f name v av probs=() exempt="" exempt_list=""
  if [ ${#charts[@]} -eq 0 ]; then
    emit C1 SKIP "no charts/*/Chart.yaml"
    return
  fi
  exempt=$(exempt_reason C1)
  [ -n "$exempt" ] && exempt_list=$(exempt_charts C1)
  local ships_image=1
  repo_ships_image && ships_image=0
  for f in "${charts[@]}"; do
    name=${f#charts/}
    name=${name%/Chart.yaml}
    if [ -n "$exempt" ] && chart_in "$exempt_list" "$name"; then
      continue
    fi
    v=$(top_key "$f" version)
    [ "$v" = 0.0.0 ] || probs+=("charts/$name version is ${v:-absent}, not 0.0.0")
    # appVersion is optional (a chart that ships no image has none), but
    # when it is committed AND the repo ships an image it is the same
    # placeholder the tag replaces.
    if [ "$ships_image" -eq 0 ] && grep -qE '^appVersion:' "$f"; then
      av=$(top_key "$f" appVersion)
      [ "$av" = 0.0.0 ] || probs+=("charts/$name appVersion is ${av:-empty}, not 0.0.0")
    fi
  done
  local ok="${#charts[@]} chart(s) commit version 0.0.0"
  [ -n "$exempt" ] && ok="$ok (exempt: $exempt_list: $exempt)"
  verdict C1 "$ok" "${probs[@]}"
}

# ── C2 · every chart has a values schema ─────────────────────────────────
rule_C2() {
  local dirs=(charts/*/Chart.yaml) f d name probs=() exempt="" exempt_list=""
  if [ ${#dirs[@]} -eq 0 ]; then
    emit C2 SKIP "no charts/*/Chart.yaml"
    return
  fi
  exempt=$(exempt_reason C2)
  [ -n "$exempt" ] && exempt_list=$(exempt_charts C2)
  for f in "${dirs[@]}"; do
    d=${f%/Chart.yaml}
    name=${d#charts/}
    if [ -n "$exempt" ] && chart_in "$exempt_list" "$name"; then
      continue
    fi
    [ -f "$d/values.schema.json" ] || probs+=("$d has no values.schema.json")
  done
  local ok="${#dirs[@]} chart(s) carry values.schema.json"
  [ -n "$exempt" ] && ok="$ok (exempt: $exempt_list: $exempt)"
  verdict C2 "$ok" "${probs[@]}"
}

# ── C3 · golden renders and a rejected-values fixture ────────────────────
has_files() { [ -d "$1" ] && [ -n "$(find "$1" -type f -print -quit)" ]; }

rule_C3() {
  local dirs=(charts/*/Chart.yaml) f name probs=() gotests=""
  if [ ${#dirs[@]} -eq 0 ]; then
    emit C3 SKIP "no charts/*/Chart.yaml"
    return
  fi
  # The Go alternative: a chart test under charts/ that exercises an
  # invalid values file. Textual -- it proves the test names such a
  # fixture, not that the assertion is right.
  if [ -d charts ]; then
    gotests=$(find charts -name '*_test.go' -type f -print0 2>/dev/null \
      | xargs -0 -r grep -liE 'invalid' 2>/dev/null | head -1)
  fi
  for f in "${dirs[@]}"; do
    name=${f#charts/}
    name=${name%/Chart.yaml}
    if has_files "tests/golden/$name" && has_files "tests/invalid/$name"; then
      continue
    fi
    [ -n "$gotests" ] && continue
    has_files "tests/golden/$name" || probs+=("no golden renders under tests/golden/$name/")
    has_files "tests/invalid/$name" || probs+=("no negative fixture under tests/invalid/$name/")
  done
  local how="tests/golden + tests/invalid"
  [ -n "$gotests" ] && how="$how or $gotests"
  verdict C3 "${#dirs[@]} chart(s) covered by $how" "${probs[@]}"
}

# ── C4 · the leak canary, run by `just check` ────────────────────────────
rule_C4() {
  local probs=() jf=""
  [ -f hack/leak-canary.sh ] || probs+=("hack/leak-canary.sh is missing")
  for f in Justfile justfile .justfile; do
    [ -f "$f" ] && { jf=$f; break; }
  done
  if [ -z "$jf" ]; then
    probs+=("no Justfile, so \`just check\` cannot run the canary")
  elif ! grep -q 'leak-canary' "$jf"; then
    probs+=("$jf never mentions hack/leak-canary.sh")
  fi
  verdict C4 "hack/leak-canary.sh exists and $jf runs it" "${probs[@]}"
}

# ── C5 · CHANGELOG.md headings ───────────────────────────────────────────
latest_tag() {
  git rev-parse --git-dir >/dev/null 2>&1 || return 1
  if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = true ]; then
    git fetch --quiet --unshallow --tags origin 2>/dev/null || true
  else
    git fetch --quiet --tags origin 2>/dev/null || true
  fi
  local base="" b
  for b in ${DEFAULT_BRANCH:+"origin/$DEFAULT_BRANCH"} origin/master origin/main HEAD; do
    if git rev-parse --verify -q "$b^{commit}" >/dev/null; then
      base=$b
      break
    fi
  done
  [ -n "$base" ] || return 1
  local tag
  tag=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$base" 2>/dev/null) || return 1
  # Two tags on one commit (a re-tag, or a patch cut on an unchanged
  # tree): describe picks either, so take the highest version there.
  git tag --points-at "$tag^{commit}" --list 'v[0-9]*' | sort -V | tail -1
}

rule_C5() {
  if [ ! -f CHANGELOG.md ]; then
    emit C5 FAIL "CHANGELOG.md is missing"
    return
  fi
  local heads=() versions=() probs=() bad=() h unreleased=0 idx=0 first_unreleased=-1
  mapfile -t heads < <(grep -E '^## ' CHANGELOG.md | sed -E 's/[[:space:]]+$//')
  local re='^## (v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)([[:space:]].*)?$'
  for h in "${heads[@]}"; do
    if [[ $h == "## Unreleased"* ]]; then
      unreleased=$((unreleased + 1))
      [ "$first_unreleased" -ge 0 ] || first_unreleased=$idx
    elif [[ $h =~ $re ]]; then
      versions+=("${BASH_REMATCH[1]}")
    elif [[ $h =~ [0-9]+\.[0-9]+\.[0-9]+ ]]; then
      bad+=("${h#\#\# }")
    fi
    idx=$((idx + 1))
  done
  [ ${#bad[@]} -eq 0 ] || probs+=("${#bad[@]} version heading(s) not in the form ## vX.Y.Z, e.g. '${bad[0]}'")
  [ "$unreleased" -le 1 ] || probs+=("$unreleased Unreleased headings; at most one")
  [ "$first_unreleased" -le 0 ] || probs+=("## Unreleased is not the first heading")
  [ ${#versions[@]} -gt 0 ] || probs+=("no ## vX.Y.Z headings")

  if [ ${#versions[@]} -gt 1 ]; then
    local sorted dupes
    sorted=$(printf '%s\n' "${versions[@]}" | sort -rV)
    [ "$sorted" = "$(printf '%s\n' "${versions[@]}")" ] || probs+=("version headings are not newest first")
    dupes=$(printf '%s\n' "${versions[@]}" | sort | uniq -d | paste -sd, -)
    [ -z "$dupes" ] || probs+=("duplicate headings: $dupes")
  fi

  local note="" tag exempt newest="" xy=""
  exempt=$(exempt_reason C5)
  [ ${#versions[@]} -eq 0 ] || newest=$(printf '%s\n' "${versions[@]}" | sort -rV | head -1)
  if tag=$(latest_tag) && [ -n "$tag" ]; then
    # An automatic patch (vX.Y.Z, Z > 0) needs no heading of its own when
    # its X.Y is the X.Y of the newest heading: the hand-cut vX.Y.0 (or a
    # later hand-cut patch) already announced the line it belongs to. A
    # patch whose X.Y no heading carries is a hand-cut tag in disguise, and
    # so is every vX.Y.0, every vX.0.0 and every pre-release.
    local tre='^v([0-9]+)\.([0-9]+)\.([0-9]+)$' nre='^v([0-9]+)\.([0-9]+)\.[0-9]+'
    if [[ $newest =~ $nre ]]; then xy="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}"; fi
    if printf '%s\n' "${versions[@]}" | grep -qxF "$tag"; then
      note="heading for the latest tag $tag present"
    elif [[ $tag =~ $tre ]] && [ "${BASH_REMATCH[3]}" -gt 0 ] && [ -n "$xy" ] \
        && [ "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}" = "$xy" ]; then
      note="latest tag $tag is an automatic patch of $newest, the newest heading; it needs none"
    elif [ -n "$exempt" ]; then
      note="no heading for the latest tag $tag, exempt: $exempt"
    elif [[ $tag =~ $tre ]] && [ "${BASH_REMATCH[3]}" -gt 0 ] && [ -n "$newest" ]; then
      probs+=("CHANGELOG.md has no heading for $tag, and it is not an automatic patch: the newest heading is $newest, not a v$xy.x")
    else
      probs+=("CHANGELOG.md has no heading for $tag")
    fi
  else
    note="no v* tag reachable from the default branch, so the latest-tag heading was not checked"
  fi
  verdict C5 "${#versions[@]} version heading(s), newest first; $note" "${probs[@]}"
}

# ── C6 · devbox pins every package ───────────────────────────────────────
rule_C6() {
  if [ ! -f devbox.json ]; then
    emit C6 SKIP "no devbox.json"
    return
  fi
  local entries=() e probs=() bare=() latest=() flakes=0
  if ! jq -e . devbox.json >/dev/null 2>&1; then
    emit C6 FAIL "devbox.json does not parse as JSON"
    return
  fi
  mapfile -t entries < <(jq -r '
      .packages // [] |
      if type == "array" then .[] | tostring
      else to_entries[] | .key + "@" + (
        if (.value | type) == "string" then .value
        elif (.value | type) == "object" then (.value.version // "")
        else "" end)
      end' devbox.json)
  for e in "${entries[@]}"; do
    # A flake reference names its own revision (or deliberately does not);
    # "name@version" is not its shape, so it is counted and not judged.
    case "$e" in
      *'#'* | github:* | path:* | ./* | /*)
        flakes=$((flakes + 1))
        continue
        ;;
    esac
    if [[ $e != *@* ]] || [ -z "${e##*@}" ]; then
      bare+=("${e%@}")
    elif [ "${e##*@}" = latest ]; then
      latest+=("${e%@*}")
    fi
  done
  [ ${#bare[@]} -eq 0 ] || probs+=("${#bare[@]} package(s) with no version: $(printf '%s, ' "${bare[@]}" | sed 's/, $//')")
  [ ${#latest[@]} -eq 0 ] || probs+=("${#latest[@]} package(s) pinned to latest: $(printf '%s, ' "${latest[@]}" | sed 's/, $//')")
  local ok="${#entries[@]} package(s) pinned to a version"
  [ "$flakes" -eq 0 ] || ok="$ok ($flakes flake reference(s) not judged)"
  verdict C6 "$ok" "${probs[@]}"
}

# ── C7 · renovate.json extends the preset ────────────────────────────────
rule_C7() {
  local f="" probs=()
  for c in renovate.json .github/renovate.json; do
    [ -f "$c" ] && { f=$c; break; }
  done
  if [ -z "$f" ]; then
    if [ -f renovate.json5 ] || [ -f .github/renovate.json5 ]; then
      emit C7 FAIL "renovate.json5 is not the contract's shape; use renovate.json"
    else
      emit C7 FAIL "renovate.json is missing"
    fi
    return
  fi
  if ! jq -e . "$f" >/dev/null 2>&1; then
    emit C7 FAIL "$f does not parse as JSON"
    return
  fi
  jq -e 'has("$schema")' "$f" >/dev/null || probs+=("$f has no \$schema")
  local extends
  extends=$(jq -c '.extends // []' "$f")
  if ! jq -e --arg p "$RENOVATE_PRESET" \
      '(.extends // []) | length == 1 and (.[0] == $p or (.[0] | startswith($p + "#")))' "$f" >/dev/null; then
    probs+=("extends is $extends, not [\"$RENOVATE_PRESET\"]")
  fi
  # "Documented overrides only": a rule or manager carries its own
  # `description`; any other override key needs the top-level one.
  local undocumented
  undocumented=$(jq -r '
      [(.packageRules // [])[], (.customManagers // [])[]]
      | map(select(has("description") | not)) | length' "$f")
  [ "$undocumented" = 0 ] || probs+=("$undocumented packageRules/customManagers entr(ies) without a description")
  if jq -e 'has("description") | not' "$f" >/dev/null; then
    local others
    others=$(jq -r 'keys - ["$schema", "extends", "description", "packageRules", "customManagers"] | join(",")' "$f")
    [ -z "$others" ] || probs+=("overrides ($others) with no top-level description")
  fi
  verdict C7 "$f extends $RENOVATE_PRESET, overrides documented" "${probs[@]}"
}

# ── C8 · README headings, in order ───────────────────────────────────────
rule_C8() {
  if [ ! -f README.md ]; then
    emit C8 FAIL "README.md is missing"
    return
  fi
  local heads=() probs=() missing=() h i last=-1 last_name="" pos
  mapfile -t heads < <(sed -n -E 's/^## (.*[^[:space:]])[[:space:]]*$/\1/p' README.md)
  for h in "${README_HEADINGS[@]}"; do
    pos=-1
    for i in "${!heads[@]}"; do
      if [ "${heads[$i]}" = "$h" ]; then
        pos=$i
        break
      fi
    done
    if [ "$pos" -lt 0 ]; then
      missing+=("$h")
      continue
    fi
    if [ "$pos" -lt "$last" ]; then
      probs+=("'$h' comes before '$last_name'")
    fi
    last=$pos
    last_name=$h
  done
  if [ ${#missing[@]} -gt 0 ]; then
    local joined
    joined=$(printf "'%s', " "${missing[@]}")
    probs=("missing ${joined%, }" "${probs[@]}")
  fi
  verdict C8 "all ${#README_HEADINGS[@]} headings present, in order" "${probs[@]}"
}

# ── C9 · MIT licence ─────────────────────────────────────────────────────
rule_C9() {
  local f="" exempt
  exempt=$(exempt_reason C9)
  for c in LICENSE LICENSE.md LICENSE.txt LICENCE; do
    [ -f "$c" ] && { f=$c; break; }
  done
  if [ -z "$f" ]; then
    if [ -n "$exempt" ]; then
      emit C9 EXEMPT "LICENSE is missing; exempt: $exempt"
    else
      emit C9 FAIL "LICENSE is missing"
    fi
  elif grep -qE 'MIT License|Permission is hereby granted, free of charge' "$f"; then
    emit C9 PASS "$f is MIT"
  elif [ -n "$exempt" ]; then
    emit C9 EXEMPT "$f is not the MIT licence; exempt: $exempt"
  else
    emit C9 FAIL "$f is not the MIT licence"
  fi
}

# ── C10 · security.yaml, and vuln outside the gate ───────────────────────

# Prints "<line>: <why>" for each way the `check` recipe reaches `vuln`:
# `vuln` among its dependencies, or among those of any recipe it depends on
# (transitively, as far as this Justfile defines them), or a body line that
# runs `just vuln`. A recipe header is a column-0 line `name [params]: deps`;
# `name := value` and `set ...` are not recipes.
justfile_reaches_vuln() {
  awk '
    /^@?[A-Za-z_][A-Za-z0-9_-]*([ \t][^:]*)?:([ \t]|$)/ {
      line = $0
      sub(/#.*/, "", line)
      name = line
      sub(/^@/, "", name)
      sub(/[ \t:].*$/, "", name)
      deps = line
      sub(/^[^:]*:/, "", deps)
      gsub(/&&|[()]/, " ", deps)
      dl[name] = deps
      ln[name] = NR
      cur = name
      next
    }
    /^[ \t]+[^ \t]/ {
      if (cur != "" && $0 ~ /(^|[^A-Za-z0-9_-])just[ \t]+([^#]*[ \t])?vuln([ \t]|$)/)
        body[cur] = body[cur] " " NR
      next
    }
    /^[ \t]*$/ { next }
    { cur = "" }
    END {
      if (!("check" in dl)) exit
      queue[1] = "check"; seen["check"] = 1; head = 1; tail = 1
      while (head <= tail) {
        r = queue[head++]
        n = split(body[r], bl, " ")
        for (i = 1; i <= n; i++)
          printf "%d: recipe %s runs `just vuln`\n", bl[i], r
        n = split(dl[r], ds, /[ \t]+/)
        for (i = 1; i <= n; i++) {
          d = ds[i]
          if (d == "") continue
          if (d == "vuln") {
            if (r == "check") printf "%d: the check recipe depends on vuln\n", ln[r]
            else printf "%d: recipe %s depends on vuln, and check depends on %s\n", ln[r], r, r
          } else if ((d in dl) && !(d in seen)) {
            seen[d] = 1; queue[++tail] = d
          }
        }
      }
    }
  ' "$1"
}
rule_C10() {
  if [ ! -f go.mod ]; then
    emit C10 SKIP "no go.mod at the root"
    return
  fi
  local probs=() sec="" f
  for c in .github/workflows/security.yaml .github/workflows/security.yml; do
    [ -f "$c" ] && { sec=$c; break; }
  done
  if [ -z "$sec" ]; then
    probs+=(".github/workflows/security.yaml is missing")
  elif ! grep -q 'truvity/ci-workflows/' "$sec"; then
    probs+=("$sec does not call truvity/ci-workflows")
  fi
  # A new CVE must not turn `check` red: vuln belongs to security.yaml's
  # own schedule, never to the recipes a merge gate runs.
  for f in .github/workflows/*.yaml .github/workflows/*.yml; do
    [ "$f" = "$sec" ] && continue
    if grep -E '^[[:space:]]*recipes:' "$f" | grep -qE '"vuln"'; then
      probs+=("$f runs vuln as a gate recipe")
    fi
  done
  # ...and `check` is the recipe every gate runs, so it must not reach
  # `vuln` either: not as a dependency, not through another recipe it
  # depends on, not by calling `just vuln` from a body.
  local jf="" hit
  for c in Justfile justfile .justfile; do
    [ -f "$c" ] && { jf=$c; break; }
  done
  if [ -n "$jf" ]; then
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      probs+=("$jf:$hit")
    done < <(justfile_reaches_vuln "$jf")
  fi
  verdict C10 "${sec:-security.yaml} present; vuln is neither a gate recipe nor reachable from check" "${probs[@]}"
}

# ── C11 · image names do not repeat the repository name ──────────────────
repo_name() {
  if [ -n "${GITHUB_REPOSITORY:-}" ]; then
    echo "${GITHUB_REPOSITORY#*/}"
    return
  fi
  local url
  url=$(git remote get-url origin 2>/dev/null) || url=$PWD
  url=${url%.git}
  echo "${url##*/}"
}

rule_C11() {
  local repo files=() f prefixes=() comps=() images=() probs=()
  repo=$(repo_name)
  for f in .goreleaser.yaml .goreleaser.yml .ko.yaml .github/workflows/release.yaml; do
    [ -f "$f" ] && files+=("$f")
  done
  if [ ${#files[@]} -eq 0 ]; then
    emit C11 SKIP "no .goreleaser.yaml, .ko.yaml or release.yaml"
    return
  fi
  # Where ko publishes: ko-docker-repo / image-repo (the release workflow's
  # inputs, which ko obeys), and goreleaser's kos `repositories:`.
  mapfile -t prefixes < <(
    { grep -hE '^[[:space:]]*(ko-docker-repo|image-repo|KO_DOCKER_REPO):' "${files[@]}" \
        | sed -E 's/^[^:]*:[[:space:]]*//'
      grep -hE '^[[:space:]]*repositories:[[:space:]]*\[' "${files[@]}" \
        | sed -E 's/^[^[]*\[//; s/\].*$//' | tr ',' '\n'
    } | sed -E "s/#.*$//; s/[\"'[:space:]]//g" | grep -vE '^$|\{\{' | sort -u)
  # What ko appends: the base of each build's main package.
  mapfile -t comps < <(
    grep -hE '^[[:space:]]*-?[[:space:]]*main:' "${files[@]}" \
      | sed -E "s/^[^:]*:[[:space:]]*//; s/#.*$//; s/[\"'[:space:]]//g; s|/+$||" \
      | awk -F/ 'NF { print $NF }' | grep -vE '^\.?$' | sort -u)
  # A ko `repositories:` entry with `base_import_paths: false` set in the
  # same kos block publishes the repository AS the image name: ko's namer
  # consults base_import_paths before `bare`, and false wins either way, so
  # nothing from `main:` is appended for that prefix. Scoped by walking the
  # kos: block of each goreleaser file and remembering the repository last
  # seen when base_import_paths: false is hit.
  local bare_prefixes=() gf
  for gf in "${files[@]}"; do
    case "$gf" in *.goreleaser.yaml | *.goreleaser.yml) ;; *) continue ;; esac
    local cur=""
    while IFS= read -r line; do
      if [[ $line =~ repositories:[[:space:]]*\[([^]]+)\] ]]; then
        cur=${BASH_REMATCH[1]%%,*}
        cur=$(sed -E "s/[\"'[:space:]]//g" <<<"$cur")
      fi
      if [ -n "$cur" ] && [[ $line =~ base_import_paths:[[:space:]]*false ]]; then
        bare_prefixes+=("$cur")
      fi
    done < <(awk '/^kos:/{f=1; next} /^[A-Za-z_]+:/{f=0} f' "$gf")
  done
  local p c is_bare
  for p in "${prefixes[@]}"; do
    is_bare=1
    for c in "${bare_prefixes[@]:-}"; do [ "$c" = "$p" ] && is_bare=0; done
    if [ "$is_bare" -eq 0 ] || [ ${#comps[@]} -eq 0 ]; then
      images+=("$p")
    else
      for c in "${comps[@]}"; do images+=("$p/$c"); done
    fi
  done
  # Image references written out in full, including a chart's default.
  # Comment-only lines are dropped first: a line explaining what an image
  # USED to be named (e.g. "# was ghcr.io/x/x up to v0.2.0") is not a
  # reference to check, and grep alone cannot tell the two apart.
  local vals=(charts/*/values.yaml)
  mapfile -t -O "${#images[@]}" images < <(
    grep -vE '^[[:space:]]*#' "${files[@]}" ${vals[@]+"${vals[@]}"} 2>/dev/null \
      | grep -ohE 'ghcr\.io/[A-Za-z0-9._/-]+' \
      | grep -vE '^ghcr\.io/[^/]+/charts(/|$)' | sort -u)
  if [ ${#images[@]} -eq 0 ]; then
    emit C11 SKIP "no image names found in ${files[*]}"
    return
  fi
  # Distinct images first — the same image named twice (once by goreleaser,
  # once again in a chart's values.yaml default) is one image, not a
  # sibling of itself.
  local img seen=" " distinct=()
  for img in "${images[@]}"; do
    case "$seen" in *" $img "*) continue ;; esac
    seen="$seen$img "
    distinct+=("$img")
  done
  # A component sharing a registry prefix with sibling images (a repo that
  # ships several binaries, e.g. ghcr.io/truvity/audit/{audit,audit-writer,
  # audit-query}) may legitimately have one component named after the repo
  # itself; only a SOLE image under a prefix that repeats the repository is
  # the degenerate case the rule means (ghcr.io/truvity/ci-cache/ci-cache
  # with nothing else published alongside it).
  declare -A prefix_count=()
  for img in "${distinct[@]}"; do
    [[ $img == */*/* ]] || continue
    prefix_count["${img%/*}"]=$(( ${prefix_count["${img%/*}"]:-0} + 1 ))
  done
  local rest
  for img in "${distinct[@]}"; do
    rest=${img#*/} # drop the registry
    rest=${rest#*/} # drop the owner
    if [[ $rest == */* ]] && [ "${rest##*/}" = "$repo" ] \
        && [ "${prefix_count["${img%/*}"]:-1}" -le 1 ]; then
      probs+=("$img repeats the repository name")
    fi
  done
  verdict C11 "image names under $repo do not repeat it (${#distinct[@]} checked)" "${probs[@]}"
}

# ── C12 · install pins a version ─────────────────────────────────────────
rule_C12() {
  if [ ! -f README.md ]; then
    emit C12 FAIL "README.md is missing"
    return
  fi
  # An install command is what this rule means: `@latest` inside a fenced
  # code block. Prose that warns against it (e.g. "an OCI chart reference
  # has no `@latest` tag to fall back to anyway") is not a command and is
  # not what a reader copies, so it does not count.
  local lines
  lines=$(awk '
      /^```/ { fence = !fence; next }
      fence && /@latest/ { print NR }
    ' README.md | paste -sd, -)
  if [ -n "$lines" ]; then
    emit C12 FAIL "README.md installs @latest (line $lines)"
  else
    emit C12 PASS "README.md never installs @latest"
  fi
}

# ── C13 · estate facts are inputs, never defaults ────────────────────────
#
# A value only one estate would choose, written as a default: in a chart's
# values.yaml or values.schema.json, or in a Go or TypeScript constant. The
# check is deliberately narrow -- it names five shapes it can tell from
# neutral text with confidence, and a reader still judges the rest:
#
#   domain   an organisation domain (truvity + .com/.xyz/.co/.private), in
#            a chart default or a code string
#   tenancy  the organisation's tenancy API group
#   env      a real cluster or environment name (kernel, devel, stage, prod)
#            as the default of a key or identifier that is named for one
#            (env, environment, cluster, stage, tier): `env: prod`,
#            `flag.String("cluster", "kernel", ...)`. A comparison or a case
#            label is a test of the name, not a default, and is not flagged
#   region   a real cloud region (eu-west-1 ...) as a chart default, or as
#            the default of an identifier named for one
#   ticket   an internal ticket key anywhere in a tracked file: a key is an
#            internal name, and the public history keeps it for ever
#
# Neutral placeholders (example.com, eu-example-1) match none of these.
# Comments are not defaults and are not read, except by `ticket`, which
# reads everything. Test, fixture and golden directories, generated code
# and `*_test.go`/`*.test.ts` are not read: they may name anything.
#
# An exemption (.github/policy-conformance.yaml) may narrow this rule:
#
#   C13:
#     - checks: [region]        # the shapes it suppresses; omitted = all
#       paths: [charts/x/*]     # shell globs of the files it covers; omitted = all
#       reason: why             # required
#     - checks: [domain]
#       paths: [pkg/id.go]
#       reason: why
#
# Each entry is its own pair: `region` for charts/x/* and `domain` for
# pkg/id.go do not cross over. The older single block (`reason:`, `checks:`,
# `paths:` directly under `C13:`) still works, but its checks and paths
# combine as a cross product, so prefer the list. Quotes around an item
# (`paths: ["a/b"]`) are stripped.
C13_AWK='
BEGIN {
  Q = "[\"\047`]"
  DOM = "(^|[^A-Za-z0-9])truvity[.](com|xyz|co|private)([^A-Za-z0-9]|$)"
  TEN = "tenancy[.]truvity[.]io"
  RPL = "(af|ap|ca|eu|il|me|mx|sa|us)-(central|north|northeast|northwest|south|southeast|southwest|east|west)-[0-9]"
  REG = "(^|[^A-Za-z0-9])" RPL "([^0-9A-Za-z]|$)"
  ENVV = "(kernel|devel|stage|prod)"
  ENVK = "^(env|environment|cluster|clustername|cluster_name|clusterid|stage|tier)$"
  NOTQ = "[^\"\047`{]*"
}
function hit(kind, text) {
  gsub(/^[ \t]+|[ \t]+$/, "", text)
  if (length(text) > 90) text = substr(text, 1, 87) "..."
  printf "%s\t%d\t%s\t%s\n", FILENAME, FNR, kind, text
}
function generic(l, orig) {
  if (l ~ DOM) hit("domain", orig)
  if (l ~ TEN) hit("tenancy", orig)
  if (l ~ REG) hit("region", orig)
}
mode == "yaml" {
  l = $0
  sub(/(^|[ \t])#.*$/, "", l)
  if (l ~ /^[ \t]*$/) next
  generic(l, $0)
  if (l ~ /^[ \t-]*[A-Za-z_][A-Za-z0-9_]*:[ \t]*[\"\047]?[A-Za-z]+[\"\047]?[ \t]*$/) {
    k = l; sub(/^[ \t-]*/, "", k); v = k
    sub(/:.*$/, "", k); sub(/^[^:]*:[ \t]*/, "", v); gsub(/[\"\047 \t]/, "", v)
    if (tolower(k) ~ ENVK && tolower(v) ~ ("^" ENVV "$")) hit("env", $0)
  }
}
mode == "json" {
  if ($0 ~ /^[ \t]*"[^"]+"[ \t]*:[ \t]*\{/) {
    k = $0; sub(/^[ \t]*"/, "", k); sub(/".*$/, "", k); lastkey = tolower(k)
  }
  if ($0 ~ /"default"[ \t]*:/) {
    generic($0, $0)
    d = $0; sub(/^.*"default"[ \t]*:[ \t]*/, "", d); gsub(/[\",\047 \t]/, "", d)
    if (lastkey ~ ENVK && tolower(d) ~ ("^" ENVV "$")) hit("env", $0)
  }
}
mode == "code" {
  l = $0
  if (l ~ /^[ \t]*(\/\/|\/\*|\*|#)/) next
  sub(/[ \t]\/\/.*$/, "", l)
  if (l ~ DOM) hit("domain", $0)
  if (l ~ TEN) hit("tenancy", $0)
  lc = tolower(l)
  if (lc ~ /==|!=|(^|[ \t])case[ \t]/) next
  if (lc ~ ("(env|environment|cluster|stage|tier)[a-z0-9_]*" Q "?" NOTQ Q ENVV Q)) hit("env", $0)
  if (lc ~ ("region[a-z0-9_]*" Q "?" NOTQ Q RPL Q)) hit("region", $0)
}
'

# The C13 exemption entries, one per line: form|checks|paths|reason (a non-blank separator, so empty fields survive `read`).
# form is `list` for a `- checks: ...` entry and `block` for the older single
# block of keys directly under `C13:`. checks and paths are comma-joined, one
# pair of matching quotes stripped from each item (`paths: ["a/b"]` and
# `paths: ['a/b']` mean a/b). A list entry may carry no reason; the caller
# refuses it.
c13_entries() {
  [ -f "$EXEMPT_FILE" ] || return 0
  awk '
    function unq(v) {
      gsub(/^[ \t]+|[ \t]+$/, "", v)
      if (length(v) >= 2) {
        f = substr(v, 1, 1)
        if ((f == "\"" || f == "\047") && substr(v, length(v), 1) == f) v = substr(v, 2, length(v) - 2)
      }
      return v
    }
    function flow(v,   n, a, i, out) {
      sub(/^[ \t]*\[/, "", v); sub(/\].*$/, "", v)
      n = split(v, a, ",")
      out = ""
      for (i = 1; i <= n; i++) { a[i] = unq(a[i]); if (a[i] != "") out = out (out == "" ? "" : ",") a[i] }
      return out
    }
    function kv(s,   k, v) {
      k = s; sub(/:.*$/, "", k)
      v = s; sub(/^[^:]*:[ \t]*/, "", v)
      if (k == "reason") reason = unq(v)
      else if (k == "checks") checks = flow(v)
      else if (k == "paths") paths = flow(v)
    }
    function flush() {
      if (have) printf "%s|%s|%s|%s\n", form, checks, paths, reason
      have = 0; form = ""; checks = ""; paths = ""; reason = ""
    }
    /^[ \t]*#/ { next }
    /^exempt:/ { in_exempt = 1; next }
    in_exempt && /^[^[:space:]]/ { in_exempt = 0 }
    in_exempt && /^  C13:[ \t]*$/ { in_rule = 1; next }
    in_exempt && in_rule && /^  [A-Za-z]/ { flush(); in_rule = 0 }
    in_exempt && in_rule && /^    -[ \t]*/ {
      flush(); have = 1; form = "list"
      s = $0; sub(/^    -[ \t]*/, "", s)
      if (s != "") kv(s)
      next
    }
    in_exempt && in_rule && /^    [A-Za-z]/ {
      if (!have) { have = 1; form = "block" }
      s = $0; sub(/^ +/, "", s); kv(s); next
    }
    in_exempt && in_rule && /^      [A-Za-z]/ {
      s = $0; sub(/^ +/, "", s); kv(s); next
    }
    END { flush() }
  ' "$EXEMPT_FILE"
}

# Is $1 (a path) covered by the shell globs in the comma-separated list $2?
# An empty list covers every path.
path_in() {
  local path=$1 list=$2 g
  [ -n "$list" ] || return 0
  local IFS=,
  for g in $list; do
    # shellcheck disable=SC2254 # the glob is the point
    case "$path" in $g) return 0 ;; esac
  done
  return 1
}

rule_C13() {
  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    emit C13 SKIP "not a git checkout, so there is no tracked-file list to read"
    return
  fi
  local tracked=() f d yamls=() jsons=() codes=() probs=() rows=() row
  mapfile -d '' tracked < <(git ls-files -z)
  for f in "${tracked[@]}"; do
    case "/$f" in
      */tests/* | */test/* | */testdata/* | */fixtures/* | */golden/* | */node_modules/* | */vendor/*) continue ;;
    esac
    d=$(dirname "$f")
    case "$f" in
      *.go)
        case "$f" in *_test.go | *.pb.go | *.pb.gw.go | *zz_generated*) continue ;; esac
        codes+=("$f")
        ;;
      *.ts | *.tsx)
        case "$f" in *.d.ts | *.test.ts | *.test.tsx | *.spec.ts | *.spec.tsx | *.gen.ts | *_pb.ts) continue ;; esac
        codes+=("$f")
        ;;
      */values.yaml | values.yaml | */values.yml | values.yml)
        [ -f "$d/Chart.yaml" ] && yamls+=("$f")
        ;;
      */values.schema.json | values.schema.json)
        [ -f "$d/Chart.yaml" ] && jsons+=("$f")
        ;;
    esac
  done
  [ ${#yamls[@]} -eq 0 ] || mapfile -t -O "${#rows[@]}" rows < <(awk -v mode=yaml "$C13_AWK" "${yamls[@]}")
  [ ${#jsons[@]} -eq 0 ] || mapfile -t -O "${#rows[@]}" rows < <(awk -v mode=json "$C13_AWK" "${jsons[@]}")
  [ ${#codes[@]} -eq 0 ] || mapfile -t -O "${#rows[@]}" rows < <(awk -v mode=code "$C13_AWK" "${codes[@]}")
  # A ticket key is an internal name wherever it sits; git grep skips
  # binary files. The pattern is assembled so this file does not match it.
  local key='IN''F-[0-9]+'
  mapfile -t -O "${#rows[@]}" rows < <(
    git grep -nIE "(^|[^A-Za-z0-9])$key" -- . 2>/dev/null \
      | awk -F: '{ f = $1; l = $2; t = $0; sub(/^[^:]*:[^:]*:/, "", t); printf "%s\t%s\tticket\t%s\n", f, l, t }')

  local kind file line text n=0 dropped=0 badent=() e_form e_checks e_paths e_reason hit
  local -a e_c=() e_p=() e_r=()
  while IFS='|' read -r e_form e_checks e_paths e_reason; do
    [ -n "$e_form" ] || continue
    if [ -z "${e_reason//[[:space:]]/}" ]; then
      # A block without a reason is ignored, as it always was; a list entry
      # is the documented form, so a missing reason is an error.
      [ "$e_form" != list ] || badent+=("an exempt: C13 entry (checks: ${e_checks:-all}; paths: ${e_paths:-all}) has no reason")
      continue
    fi
    e_c+=("$e_checks"); e_p+=("$e_paths"); e_r+=("$e_reason")
  done < <(c13_entries)
  local why i
  for row in "${rows[@]}"; do
    [ -n "$row" ] || continue
    IFS=$'\t' read -r file line kind text <<<"$row"
    hit=""
    for i in "${!e_c[@]}"; do
      path_in "$file" "${e_p[$i]}" || continue
      [ -z "${e_c[$i]}" ] || case ",${e_c[$i]}," in *",$kind,"*) ;; *) continue ;; esac
      hit=1; break
    done
    if [ -n "$hit" ]; then
      dropped=$((dropped + 1))
      continue
    fi
    case "$kind" in
      domain) why="an organisation domain is an estate fact, not a default" ;;
      tenancy) why="the organisation's tenancy API group is an estate fact, not a default" ;;
      env) why="a real cluster or environment name as a default" ;;
      region) why="a real cloud region as a default" ;;
      ticket) why="an internal ticket key" ;;
      *) why=$kind ;;
    esac
    # Every finding gets its own line, so the log carries file:line for
    # each; the verdict line summarises.
    echo "  C13 $file:$line: $why: $text"
    n=$((n + 1))
    [ "$n" -gt 3 ] || probs+=("$file:$line ($kind)")
  done
  if [ "$n" -gt 3 ]; then
    probs=("$n estate facts: ${probs[*]:0:3} and $((n - 3)) more, listed above")
  fi
  local ok="no estate fact as a default in ${#yamls[@]} values file(s), ${#jsons[@]} schema(s), ${#codes[@]} Go/TS file(s); no ticket key"
  [ "$dropped" -eq 0 ] || ok="$ok ($dropped finding(s) exempt: ${e_r[*]})"
  verdict C13 "$ok" "${badent[@]}" "${probs[@]}"
}

# ── main ─────────────────────────────────────────────────────────────────
declare -A skipped=()
for id in ${SKIP//,/ }; do
  id=${id^^}
  case " ${RULES[*]} " in
    *" $id "*) skipped[$id]=1 ;;
    *)
      echo "::error::skip names '$id', which is not one of ${RULES[*]}"
      exit 1
      ;;
  esac
done
if [ ${#skipped[@]} -gt 0 ] && [ -z "${REASON//[[:space:]]/}" ]; then
  echo "::error::skip needs a reason: a rule turned off with no reason is a rule nobody remembers turning off"
  exit 1
fi

for id in "${RULES[@]}"; do
  if [ -n "${skipped[$id]:-}" ]; then
    emit "$id" SKIP "skipped by the caller: $REASON"
  else
    "rule_$id"
  fi
done

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### policy-conformance"
    echo
    echo "Rules from truvity/policy \`docs/contracts/component.md\`; strict: \`$STRICT\`."
    echo
    echo "| rule | verdict | detail |"
    echo "| -- | -- | -- |"
    for line in "${results[@]}"; do
      id=${line%% *}
      rest=${line#* }
      v=${rest%%:*}
      msg=${rest#*: }
      printf '| %s | %s | %s |\n' "$id" "$v" "${msg//|/\\|}"
    done
  } >>"$GITHUB_STEP_SUMMARY"
fi

if [ "$failed" -gt 0 ]; then
  if [ "$STRICT" = true ]; then
    echo "::error::policy-conformance: $failed rule(s) failed"
    exit 1
  fi
  echo "policy-conformance: $failed rule(s) failed; reported as warnings (strict: false)"
  exit 0
fi
echo "policy-conformance: every evaluated rule passed"
