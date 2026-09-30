#!/usr/bin/env bash
# shellcheck disable=SC2015,SC2016 # ok() cannot fail; the $ in fixture JSON is literal
# policy-conformance, run against fixture repositories this script builds.
#
# A conformance check that reports PASS for everything is indistinguishable
# from one that looks at nothing. So the harness starts from a repository
# that meets every rule, proves each rule says PASS there, then breaks one
# rule at a time and proves THAT rule, and only that rule, says FAIL.
#
# No network and no token: `origin` is a bare repository beside the
# fixture, and the tag C5 looks for is pushed there.
set -uo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/policy-conformance/policy-conformance.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail=0
ok()  { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=1; }

export GIT_AUTHOR_NAME=case GIT_AUTHOR_EMAIL=case@example.invalid
export GIT_COMMITTER_NAME=case GIT_COMMITTER_EMAIL=case@example.invalid
export GITHUB_REPOSITORY=example/widget

# Estate facts the C13 cases plant. This file is itself tracked by a public
# repository whose canary and ticket rule would match them written out, so
# each is assembled from pieces at run time.
org=truv; org="${org}ity"          # the organisation's own name
key=IN; key="${key}F-4242"         # an internal ticket key
unset GITHUB_STEP_SUMMARY

# A repository that meets C1-C13.
conformant() {
  local d=$1
  rm -rf "$d" "$d.git"
  mkdir -p "$d"/charts/widget "$d"/tests/golden/widget "$d"/tests/invalid/widget \
    "$d"/hack "$d"/.github/workflows
  (
    cd "$d" || exit 1
    printf 'apiVersion: v2\nname: widget\nversion: 0.0.0\nappVersion: "0.0.0"\n' >charts/widget/Chart.yaml
    echo '{}' >charts/widget/values.schema.json
    echo 'image:' >charts/widget/values.yaml
    echo '  repository: ghcr.io/example/widget/server' >>charts/widget/values.yaml
    echo 'kind: Deployment' >tests/golden/widget/default.yaml
    echo 'replicas: -1' >tests/invalid/widget/negative.yaml
    printf '#!/usr/bin/env bash\n' >hack/leak-canary.sh
    printf 'check:\n    ./hack/leak-canary.sh\n' >Justfile
    printf '# Changelog\n\n## Unreleased\n\n## v1.1.0\n\n## v1.0.0\n' >CHANGELOG.md
    echo '{"packages": ["go@1.25.1", "just@1.40.0"]}' >devbox.json
    echo '{"$schema": "https://docs.renovatebot.com/renovate-schema.json", "extends": ["github>truvity/ci-workflows"]}' >renovate.json
    {
      echo '# widget'
      for h in "Who it is for" "The model" "Install and a worked example" "Consumers" \
               "Neighbours" "Documentation" "The rule that makes this repository public" \
               "Status" "Development" "Releasing" "Licence"; do
        printf '\n## %s\n\ntext\n' "$h"
      done
      printf '\n```sh\ngo install example.invalid/widget@v1.1.0\n```\n'
    } >README.md
    printf 'MIT License\n\nPermission is hereby granted, free of charge, ...\n' >LICENSE
    echo 'module example.invalid/widget' >go.mod
    printf 'jobs:\n  govulncheck:\n    uses: truvity/ci-workflows/.github/workflows/check.yaml@x\n    with:\n      recipes: %s\n' "'[\"vuln\"]'" >.github/workflows/security.yaml
    printf 'jobs:\n  check:\n    with:\n      recipes: %s\n' "'[\"build\", \"test\"]'" >.github/workflows/ci.yaml
    printf 'jobs:\n  release:\n    with:\n      ko-docker-repo: ghcr.io/example/widget\n' >.github/workflows/release.yaml
    printf 'builds:\n  - id: server\n    main: ./cmd/server\n' >.goreleaser.yaml

    git init -q -b master .
    git add -A
    git commit -q -m init
    git tag -a v1.1.0 -m v1.1.0
    git clone -q --bare . "$d.git"
    git remote add origin "$d.git"
    git fetch -q origin
  ) || return 1
}

# The rules read tracked files, so what an edit created is staged first,
# as it would be by the commit a real change ends in.
run() { (cd "$1" && git add -A 2>/dev/null; bash "$script" 2>&1); }

# The one line for a rule, or nothing.
line() { grep -E "^$2 " <<<"$1"; }

expect() {
  local log=$1 id=$2 want=$3 what=$4
  local l
  l=$(line "$log" "$id")
  case "$l" in
    "$id $want:"*) ok "$what" ;;
    *) bad "$what: got '${l:-no line}'" ;;
  esac
}

repo="$work/widget"

# ── the baseline: every rule passes ──────────────────────────────────────

conformant "$repo" || { bad "fixture builds"; exit 1; }
log=$(run "$repo")
for id in C1 C2 C3 C4 C5 C6 C7 C8 C9 C10 C11 C12 C13; do
  expect "$log" "$id" PASS "$id passes on a conformant repository"
done
case "$log" in
  *"heading for the latest tag v1.1.0 present"*) ok "C5 found the latest tag through origin" ;;
  *) bad "C5 found the latest tag through origin: $(line "$log" C5)" ;;
esac

# ── one rule broken at a time ────────────────────────────────────────────

# breaks <rule> <description> <shell run inside the fixture> [<text the FAIL line must carry>]
breaks() {
  local id=$1 what=$2 edit=$3 needle=${4:-}
  conformant "$repo" || { bad "fixture builds"; return; }
  (cd "$repo" && eval "$edit")
  local log l
  log=$(run "$repo")
  expect "$log" "$id" FAIL "$id: $what"
  if [ -n "$needle" ]; then
    l=$(line "$log" "$id")
    case "$l" in
      *"$needle"*) ok "$id names it: $needle" ;;
      *) bad "$id names it: $needle; got '$l'" ;;
    esac
  fi
  # Only the broken rule moved.
  local others
  others=$(grep -E '^C[0-9]+ FAIL' <<<"$log" | grep -vE "^$id " || true)
  [ -z "$others" ] || bad "$id: breaking it failed other rules too: $others"
}

breaks C1 "a chart committing a real version" \
  "sed -i 's/^version: 0.0.0/version: 0.1.0/' charts/widget/Chart.yaml" "version is 0.1.0"
breaks C1 "the retired 0.0.0-dev placeholder" \
  "sed -i 's/^version: 0.0.0/version: 0.0.0-dev/' charts/widget/Chart.yaml" "0.0.0-dev"
breaks C1 "an appVersion that is not the placeholder" \
  "sed -i 's/^appVersion: .*/appVersion: 1.2.3/' charts/widget/Chart.yaml" "appVersion is 1.2.3"
breaks C2 "a chart with no values schema" \
  "rm charts/widget/values.schema.json" "values.schema.json"
breaks C3 "a chart with no negative fixture" \
  "rm -r tests/invalid" "tests/invalid/widget"
breaks C4 "no Justfile" \
  "rm Justfile" "no Justfile"
breaks C4 "a Justfile that never runs the canary" \
  "echo 'check:' >Justfile" "never mentions"
breaks C5 "no heading for the latest tag" \
  "git commit -q --allow-empty -m next && git push -q origin master && git tag -a v1.2.0 -m v1.2.0 && git push -q origin v1.2.0 && git fetch -q origin" "no heading for v1.2.0"
breaks C5 "no heading for the higher of two tags on one commit" \
  "git tag -a v1.2.0 -m v1.2.0 && git push -q origin v1.2.0" "no heading for v1.2.0"
breaks C5 "headings oldest first" \
  "printf '## v1.0.0\n\n## v1.1.0\n' >CHANGELOG.md" "newest first"
breaks C5 "a Keep-a-Changelog style heading" \
  "printf '## [1.1.0] - 2026-09-29\n' >CHANGELOG.md" "not in the form"
breaks C5 "two Unreleased headings" \
  "printf '## Unreleased\n\n## Unreleased\n\n## v1.1.0\n' >CHANGELOG.md" "at most one"
breaks C5 "a patch of a line no heading carries is hand-cut and needs its heading" \
  "git commit -q --allow-empty -m next && git push -q origin master && git tag -a v1.2.1 -m v1.2.1 && git push -q origin v1.2.1 && git fetch -q origin" \
  "not an automatic patch"
breaks C6 "a package on latest" \
  "echo '{\"packages\": {\"go\": \"latest\", \"just\": \"1.40.0\"}}' >devbox.json" "pinned to latest: go"
breaks C6 "a package with no version" \
  "echo '{\"packages\": [\"go\"]}' >devbox.json" "no version: go"
breaks C7 "a renovate.json that restates the preset" \
  "echo '{\"\$schema\": \"x\", \"extends\": [\"config:recommended\"]}' >renovate.json" "extends is"
breaks C7 "an undocumented override" \
  "echo '{\"\$schema\": \"x\", \"extends\": [\"github>truvity/ci-workflows\"], \"automerge\": false}' >renovate.json" "no top-level description"
breaks C8 "a missing heading" \
  "sed -i '/^## Neighbours$/d' README.md" "'Neighbours'"
breaks C8 "two headings swapped" \
  "sed -i 's/^## Status$/## TMP/; s/^## Development$/## Status/; s/^## TMP$/## Development/' README.md" "comes before"
breaks C9 "a licence that is not MIT" \
  "echo 'Apache License' >LICENSE" "not the MIT"
breaks C10 "a Go repository with no security.yaml" \
  "rm .github/workflows/security.yaml" "security.yaml is missing"
breaks C10 "the check recipe depending on vuln" \
  "printf 'vuln:\n    true\ncheck: vuln\n    ./hack/leak-canary.sh\n' >Justfile" "the check recipe depends on vuln"
breaks C10 "check reaching vuln through another recipe" \
  "printf 'vuln:\n    true\nci: vuln\n    true\ncheck: ci\n    ./hack/leak-canary.sh\n' >Justfile" "recipe ci depends on vuln"
breaks C10 "check calling just vuln from its body" \
  "printf 'vuln:\n    true\ncheck:\n    ./hack/leak-canary.sh\n    just vuln\n' >Justfile" "runs \`just vuln\`"
breaks C10 "vuln in the merge gate" \
  "sed -i 's/\"test\"/\"test\", \"vuln\"/' .github/workflows/ci.yaml" "ci.yaml runs vuln"
breaks C11 "an image named after the repository twice" \
  "sed -i 's|cmd/server|cmd/widget|' .goreleaser.yaml && sed -i 's|widget/server|widget/widget|' charts/widget/values.yaml" "ghcr.io/example/widget/widget"
breaks C12 "an @latest install" \
  "printf '\n\`\`\`sh\ngo install example.invalid/widget@latest\n\`\`\`\n' >>README.md" "@latest"

# holds <id> <what> <edit> [<want>] — apply <edit> to a conformant repo and
# expect <id> to still say <want> (PASS unless given).
holds() {
  local id=$1 what=$2 edit=$3 want=${4:-PASS}
  conformant "$repo" || { bad "fixture builds"; return; }
  (cd "$repo" && eval "$edit")
  local log
  log=$(run "$repo")
  expect "$log" "$id" "$want" "$what"
}

# ── C12 · prose mentioning @latest is not an install command ────────────

holds C12 "@latest named only in prose, outside a fenced block, is not an install" \
  "printf '\nPin a real version -- an OCI reference has no \`@latest\` tag to fall back to.\n' >>README.md"

# ── C1 · a chart-only repository judges no appVersion ────────────────────

holds C1 "appVersion is not judged when every goreleaser build is skipped" \
  "printf 'builds:\n  - skip: true\n' >.goreleaser.yaml
   sed -i 's/^appVersion: .*/appVersion: \"9.9.9\"/' charts/widget/Chart.yaml"

# ── exemptions: .github/policy-conformance.yaml ──────────────────────────

holds C1 "an exempted chart's version is not judged" \
  "mkdir -p .github
   sed -i 's/^version: 0.0.0/version: 1.2.3-upstream/' charts/widget/Chart.yaml
   printf 'exempt:\n  C1:\n    reason: CRD chart, stamped from upstream'\''s pin\n    charts: [widget]\n' >.github/policy-conformance.yaml"

breaks C1 "an exemption named for one chart does not cover a second" \
  "mkdir -p charts/other tests/golden/other tests/invalid/other .github
   cp charts/widget/Chart.yaml charts/other/Chart.yaml
   cp charts/widget/values.schema.json charts/other/values.schema.json
   echo 'kind: Deployment' >tests/golden/other/default.yaml
   echo 'replicas: -1' >tests/invalid/other/negative.yaml
   sed -i 's/^version: 0.0.0/version: 1.2.3/' charts/other/Chart.yaml
   printf 'exempt:\n  C1:\n    reason: only widget is exempt\n    charts: [widget]\n' >.github/policy-conformance.yaml" \
  "charts/other version is 1.2.3"

holds C2 "an exempted chart's missing schema is not judged" \
  "mkdir -p .github
   rm charts/widget/values.schema.json
   printf 'exempt:\n  C2:\n    reason: library chart takes no values\n    charts: [widget]\n' >.github/policy-conformance.yaml"

holds C5 "an exemption suppresses only the missing-latest-heading failure" \
  "mkdir -p .github
   printf 'exempt:\n  C5:\n    reason: dependency-only patches carry no heading\n' >.github/policy-conformance.yaml
   git commit -q --allow-empty -m next && git push -q origin master && git tag -a v1.2.0 -m v1.2.0 && git push -q origin v1.2.0 && git fetch -q origin"

breaks C5 "an exemption does not excuse a genuinely unordered CHANGELOG" \
  "mkdir -p .github
   printf 'exempt:\n  C5:\n    reason: dependency-only patches carry no heading\n' >.github/policy-conformance.yaml
   printf '## v1.0.0\n\n## v1.1.0\n' >CHANGELOG.md" "newest first"

holds C9 "an exempted fork's non-MIT licence is EXEMPT, not FAIL" \
  "mkdir -p .github
   echo 'Apache License' >LICENSE
   printf 'exempt:\n  C9:\n    reason: fork of an Apache-2.0 upstream\n' >.github/policy-conformance.yaml" \
  EXEMPT

# ── C5 · an automatic patch needs no heading of its own ─────────────────

# A patch tag on a new commit, cut after v1.1.0's heading.
patch_tag='git commit -q --allow-empty -m next && git push -q origin master && git tag -a %s -m %s && git push -q origin %s && git fetch -q origin'
# shellcheck disable=SC2059
holds C5 "vX.Y.Z with Z > 0 and the newest heading's X.Y is an automatic patch" \
  "$(printf "$patch_tag" v1.1.1 v1.1.1 v1.1.1)"
# shellcheck disable=SC2059
holds C5 "a patch whose own heading exists passes as a hand-cut tag" \
  "printf '## Unreleased\n\n## v1.1.1\n\n## v1.1.0\n\n## v1.0.0\n' >CHANGELOG.md
   $(printf "$patch_tag" v1.1.1 v1.1.1 v1.1.1)"
# shellcheck disable=SC2059
breaks C5 "vX.Y.0 is always hand-cut, even one minor past the newest heading" \
  "$(printf "$patch_tag" v1.2.0 v1.2.0 v1.2.0)" "no heading for v1.2.0"
# shellcheck disable=SC2059
breaks C5 "a major bump patch (v2.0.1, newest heading v1.1.0) is not automatic" \
  "$(printf "$patch_tag" v2.0.1 v2.0.1 v2.0.1)" "not an automatic patch"

# ── C10 · look-alikes that are not check reaching vuln ──────────────────

holds C10 "a vuln recipe that check does not reach, and a comment naming it" \
  "printf '# check: vuln would be wrong\nvuln:\n    govulncheck ./...\ncheck: build\n    ./hack/leak-canary.sh\nbuild:\n    just build-all\n' >Justfile"
holds C10 "just vuln-report in a body is another recipe, not vuln" \
  "printf 'check:\n    ./hack/leak-canary.sh\n    just vuln-report\n' >Justfile"

# ── C13 · estate facts are inputs, never defaults ───────────────────────

breaks C13 "an organisation domain as a chart default" \
  "printf 'host: internal.%s.com\n' \"$org\" >>charts/widget/values.yaml" "values.yaml:3 (domain)"
breaks C13 "the tenancy API group as a chart default" \
  "printf 'group: tenancy.%s.io\n' \"$org\" >>charts/widget/values.yaml" "values.yaml:3 (tenancy)"
breaks C13 "a real environment name as the default of an env key" \
  "printf 'env: prod\n' >>charts/widget/values.yaml" "values.yaml:3 (env)"
breaks C13 "a real cluster name, quoted, as the default of a cluster key" \
  "printf 'cluster: \"kernel\"\n' >>charts/widget/values.yaml" "values.yaml:3 (env)"
breaks C13 "a real region as a chart default" \
  "printf 'bucketRegion: eu-west-1\n' >>charts/widget/values.yaml" "values.yaml:3 (region)"
breaks C13 "a real region inside a longer chart default" \
  "printf 'endpoint: s3.us-east-2.amazonaws.example\n' >>charts/widget/values.yaml" "values.yaml:3 (region)"
breaks C13 "an environment name as a schema default" \
  "printf '{\n  \"properties\": {\n    \"env\": {\n      \"type\": \"string\",\n      \"default\": \"devel\"\n    }\n  }\n}\n' >charts/widget/values.schema.json" \
  "values.schema.json:5 (env)"
breaks C13 "a Go constant naming a real cluster" \
  "mkdir -p cmd/server && printf 'package main\n\nconst defaultCluster = \"kernel\"\n' >cmd/server/main.go" "main.go:3 (env)"
breaks C13 "a Go flag defaulting to a real region" \
  "mkdir -p cmd/server && printf 'package main\n\nvar region = fs.String(\"region\", \"us-east-1\", \"\")\n' >cmd/server/main.go" "main.go:3 (region)"
breaks C13 "a TypeScript constant holding an organisation domain" \
  "mkdir -p src && printf 'export const HOST = \"api.%s.xyz\";\n' \"$org\" >src/config.ts" "config.ts:1 (domain)"
breaks C13 "a ticket key in the README" \
  "printf '\nSee %s.\n' \"$key\" >>README.md" "README.md:"
breaks C13 "a ticket key in the CHANGELOG is no exception" \
  "printf -- '- fixes %s\n' \"$key\" >>CHANGELOG.md" "CHANGELOG.md:"
breaks C13 "a ticket key in code" \
  "mkdir -p cmd/server && printf 'package main\n\n// see %s\n' \"$key\" >cmd/server/main.go" "main.go:3 (ticket)"

# What is not an estate fact must stay quiet.
holds C13 "neutral placeholders in chart defaults" \
  "printf 'host: app.example.com\nregion: eu-example-1\nenv: \"\"\ncluster: \"\"\n' >>charts/widget/values.yaml"
holds C13 "a real region or environment named only in a comment" \
  "printf '# e.g. eu-west-1, or prod for %s.com\nregion: \"\" # eu-west-1 in our case\n' \"$org\" >>charts/widget/values.yaml"
holds C13 "a Go comparison, case label and list of names are tests of a name, not defaults" \
  "mkdir -p cmd/server && printf 'package main\n\nfunc f(env string) bool {\n\tswitch env {\n\tcase \"prod\":\n\t\treturn true\n\t}\n\tenvs := []string{\"devel\", \"prod\"}\n\t_ = envs\n\treturn env == \"kernel\"\n}\n' >cmd/server/main.go"
holds C13 "Go test files may name anything" \
  "mkdir -p cmd/server && printf 'package main\n\nconst defaultCluster = \"kernel\"\n' >cmd/server/main_test.go"
holds C13 "a values.yaml that is not a chart's is not read" \
  "mkdir -p docs && printf 'env: prod\n' >docs/values.yaml"
holds C13 "a URL that merely contains the organisation's name as a path is not a domain" \
  "mkdir -p cmd/server && printf 'package main\n\nconst repo = \"https://github.com/%s/widget\"\n' \"$org\" >cmd/server/main.go"

# ── C13 · exemptions ─────────────────────────────────────────────────────

holds C13 "an exemption for one check suppresses that check" \
  "mkdir -p .github
   printf 'bucketRegion: eu-west-1\n' >>charts/widget/values.yaml
   printf 'exempt:\n  C13:\n    reason: this chart documents one region as a worked example\n    checks: [region]\n' >.github/policy-conformance.yaml"
breaks C13 "an exemption for one check does not cover another" \
  "mkdir -p .github
   printf 'bucketRegion: eu-west-1\nhost: internal.%s.com\n' \"$org\" >>charts/widget/values.yaml
   printf 'exempt:\n  C13:\n    reason: this chart documents one region as a worked example\n    checks: [region]\n' >.github/policy-conformance.yaml" \
  "values.yaml:4 (domain)"
holds C13 "an exemption scoped to a path covers that path" \
  "mkdir -p .github
   printf 'See %s.\n' \"$key\" >HISTORY.md
   printf 'exempt:\n  C13:\n    reason: a history file that quotes tickets\n    checks: [ticket]\n    paths: [HISTORY.md]\n' >.github/policy-conformance.yaml"
breaks C13 "an exemption scoped to a path does not cover another" \
  "mkdir -p .github
   printf 'See %s.\n' \"$key\" >HISTORY.md
   printf '\nSee %s.\n' \"$key\" >>README.md
   printf 'exempt:\n  C13:\n    reason: a history file that quotes tickets\n    checks: [ticket]\n    paths: [HISTORY.md]\n' >.github/policy-conformance.yaml" \
  "README.md:"

# ── C11 · ko's base_import_paths: false, and sibling components ─────────

holds C11 "base_import_paths: false publishes the bare repository, no comp appended" \
  "cat >>.goreleaser.yaml <<'EOF'
kos:
  - id: server
    build: server
    repositories: [ghcr.io/example/widget/server]
    base_import_paths: false
EOF"

holds C11 "a component named after the repo is not flagged when a sibling exists" \
  "mkdir -p charts/other
   printf 'apiVersion: v2\nname: other\nversion: 0.0.0\n' >charts/other/Chart.yaml
   echo '{}' >charts/other/values.schema.json
   mkdir -p tests/golden/other tests/invalid/other
   echo 'kind: Deployment' >tests/golden/other/default.yaml
   echo 'replicas: -1' >tests/invalid/other/negative.yaml
   sed -i 's|cmd/server|cmd/widget|' .goreleaser.yaml
   sed -i 's|widget/server|widget/widget|' charts/widget/values.yaml
   echo '  repository: ghcr.io/example/widget/second' >>charts/other/values.yaml
   echo 'image:' | cat - charts/other/values.yaml >/tmp/ov && mv /tmp/ov charts/other/values.yaml"

holds C11 "a ghcr.io reference inside a comment is not a reference to check" \
  "sed -i '1i # was ghcr.io/example/widget/widget up to an earlier version.' charts/widget/values.yaml"

# ── inputs ───────────────────────────────────────────────────────────────

conformant "$repo"
(cd "$repo" && rm Justfile)

log=$(cd "$repo" && STRICT=false bash "$script" 2>&1)
rc=$?
[ "$rc" = 0 ] && ok "strict: false exits 0 on a failure" || bad "strict: false exits 0 on a failure (rc $rc)"
case "$log" in
  *"::warning title=policy-conformance C4::"*) ok "strict: false annotates a warning" ;;
  *) bad "strict: false annotates a warning" ;;
esac

(cd "$repo" && STRICT=true bash "$script" >/dev/null 2>&1)
rc=$?
[ "$rc" = 1 ] && ok "strict: true exits 1 on a failure" || bad "strict: true exits 1 on a failure (rc $rc)"

log=$(cd "$repo" && STRICT=true SKIP=c4 REASON="no Justfile here yet" bash "$script" 2>&1)
rc=$?
[ "$rc" = 0 ] && ok "a skipped rule does not fail strict" || bad "a skipped rule does not fail strict (rc $rc)"
expect "$log" C4 SKIP "skip prints SKIP"
case "$(line "$log" C4)" in
  *"no Justfile here yet"*) ok "skip prints the reason" ;;
  *) bad "skip prints the reason: $(line "$log" C4)" ;;
esac

(cd "$repo" && SKIP=C4 bash "$script" >/dev/null 2>&1)
[ $? = 1 ] && ok "skip without a reason is refused" || bad "skip without a reason is refused"

(cd "$repo" && SKIP=C99 REASON=x bash "$script" >/dev/null 2>&1)
[ $? = 1 ] && ok "an unknown rule id is refused" || bad "an unknown rule id is refused"

summary="$work/summary.md"
(cd "$repo" && GITHUB_STEP_SUMMARY=$summary bash "$script" >/dev/null 2>&1)
if grep -q '^| C4 | FAIL |' "$summary" 2>/dev/null; then
  ok "the job summary carries a row per rule"
else
  bad "the job summary carries a row per rule"
fi

# ── rules with nothing to judge ──────────────────────────────────────────

empty="$work/empty"
mkdir -p "$empty"
log=$(run "$empty")
for id in C1 C2 C3 C6 C10 C11 C13; do
  expect "$log" "$id" SKIP "$id skips when there is nothing to judge"
done
for id in C5 C7 C8 C9 C12; do
  expect "$log" "$id" FAIL "$id fails when its file is missing"
done

if [ "$fail" != 0 ]; then
  echo "::error::policy-conformance cases failed"
  exit 1
fi
echo "policy-conformance cases: all passed"
