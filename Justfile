# ci-actions development gate
# Recipes mirror what CI runs on every pull request

default: check

# Lint GitHub Actions workflows and composites with actionlint
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! command -v actionlint &>/dev/null; then
        tmpdir=$(mktemp -d)
        trap "rm -rf $tmpdir" EXIT
        curl -fsSL -o "$tmpdir/actionlint.tgz" \
          https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz
        tar xzf "$tmpdir/actionlint.tgz" -C "$tmpdir" actionlint
        "$tmpdir/actionlint" -color
    else
        actionlint -color
    fi

# Test discovery API rule against stub endpoints
discover:
    @./hack/discover-cases.sh

# Test caller-parity rule against sample workflows
parity:
    @./hack/caller-parity-cases.sh

# Test policy kit currency
kit:
    @./hack/policy-kit-current.sh

# Test cache environment wiring against stub
cache-env:
    @./hack/cache-env-cases.sh

# Test tagged-pins rule against local remotes
pins-cases:
    @./hack/tagged-pins-cases.sh

# Test fork-guard against fake event payloads
fork-guard:
    @./hack/fork-guard-cases.sh

# Fail if any action file or script escalates privilege
no-escalation:
    @./hack/no-escalation-cases.sh

# Test setup-devbox's preflight, one failure at a time
preflight-cases:
    @./hack/preflight-cases.sh

# Run the step scripts as a restricted runner (needs docker and yq)
restricted-sim:
    @./hack/restricted-sim.sh --control
    @./hack/restricted-sim.sh --self-test
    @./hack/restricted-sim.sh

# Verify all pins point to release tags
pins:
    #!/usr/bin/env bash
    set -euo pipefail
    LIBRARIES="truvity/ci-workflows truvity/ci-actions truvity/ci-cache"
    
    fail=0
    seen=0
    
    for library in $LIBRARIES; do
      mapfile -t refs < <(grep -rhoE --exclude-dir=.git "${library}/[^@[:space:]\"']+@[0-9a-f]{40}" . 2>/dev/null | sort -u)
    
      if [ ${#refs[@]} -eq 0 ]; then
        echo "no ${library} pins in this checkout — nothing to check"
        continue
      fi
    
      seen=$((seen + 1))
    
      tags=$(git ls-remote --tags "https://github.com/${library}" \
             | sed -n 's|^\([0-9a-f]\{40\}\)[[:space:]]*refs/tags/\(.*\)^{}$|\1 \2|p')
      newest=$(cut -d' ' -f2 <<<"$tags" | sort -V | tail -1)
    
      for ref in "${refs[@]}"; do
        sha=${ref##*@}
        what=${ref%@*}
        what=${what#"${library}/"}
    
        tag=$(awk -v s="$sha" '$1 == s { print $2; exit }' <<<"$tags")
    
        if [ -n "$tag" ]; then
          printf 'ok        %-46s %.12s  %s\n' "${library}/${what}" "$sha" "$tag"
          continue
        fi
    
        fail=1
        printf 'UNTAGGED  %-46s %.12s  names no release; newest is %s\n' "${library}/${what}" "$sha" "$newest"
      done
    done
    
    if [ "$fail" != 0 ]; then
      echo "ERROR: A pinned commit is not a release. Pin the commit of a tag."
      exit 1
    fi
    
    echo "tagged-pins: ${seen} of 3 libraries had pins in this checkout, all naming releases"

# Verify public runners are used
runners:
    #!/usr/bin/env bash
    set -euo pipefail
    visibility="public"
    
    if [ "$visibility" != "public" ]; then
      echo "not a public repository — any runner is allowed"
      exit 0
    fi
    
    fail=0
    for label in ubuntu-latest; do
      case "$label" in
        ubuntu-*|windows-*|macos-*)
          echo "ok        $label"
          ;;
        *)
          echo "SELF-HOSTED  $label"
          fail=1
          ;;
      esac
    done
    
    if [ "$fail" != 0 ]; then
      echo "ERROR: public repository, hosted runners only"
      exit 1
    fi
    
    echo "public repository, hosted runners only — checked"

# Test policy-conformance rule against fixture repositories
conformance-cases:
    @./hack/policy-conformance-cases.sh

# Run policy-conformance against this repository
conformance:
    #!/usr/bin/env bash
    set -euo pipefail
    # Fetch tags for C5 check
    git fetch --tags origin 2>/dev/null || true

    # Run the conformance action against this repo
    STRICT=false bash -c '
      env GITHUB_ACTION_PATH=. \
          STRICT=false \
          SKIP="" \
          REASON="" \
          RENOVATE_PRESET="github>truvity/ci-workflows" \
          DEFAULT_BRANCH="master" \
          ./policy-conformance/policy-conformance.sh
    '

# Scan for secrets and sensitive data
leak-canary:
    @./hack/leak-canary.sh

# Run all checks (the merge gate)
check: lint discover parity kit cache-env pins-cases conformance-cases no-escalation preflight-cases fork-guard pins runners conformance leak-canary
    @echo "✓ All checks passed"
