# Changelog

Every release of truvity/ci-actions, newest first. Pin the commit of a
tag (`git rev-parse vX.Y.Z^{commit}`), never the tag object and never
an untagged commit; `tagged-pins` refuses anything else.

## Unreleased

`policy-conformance` exemption file:

- **C13 exemptions are a list of paired entries.** Each
  `- checks: [...]`, `paths: [...]`, `reason: ...` entry covers only its
  own checks on its own paths, so exempting `region` for one path and
  `domain` for another no longer exempts the other two combinations. A list
  entry with no `reason` fails C13. The single-block form still works
  unchanged, cross product included; the list form is preferred.
- Quotes around an item in `checks:` or `paths:` are stripped, so
  `paths: ["a/b"]` matches `a/b` instead of silently matching nothing.

## v1.4.0

`policy-conformance` tightened before the check becomes required:

- **C10** now enforces the second half of its own rule: the Justfile's
  `check` recipe must not reach `vuln`, as a dependency, through another
  recipe it depends on, or by running `just vuln` in a body. Each hit is
  reported as `Justfile:<line>`.
- **C13** is checked, for the five shapes a script can tell from neutral
  text: an organisation domain, the tenancy API group, a real
  cluster or environment name and a real cloud region as a default, and
  an internal ticket key anywhere in a tracked file. Every finding prints
  as `C13 <file>:<line>`. An exemption in `.github/policy-conformance.yaml`
  may name `checks:` and `paths:`. A caller that lists `skip: C13` no
  longer needs to; one that sets `strict: true` will now fail on a hit.
- **C5** follows the contract's text: an automatic patch (`vX.Y.Z`, Z > 0,
  whose `X.Y` is the `X.Y` of the newest heading) needs no heading of its
  own; every other tag, a hand-cut patch of an unheaded line included,
  does.

## v1.3.0

`policy-conformance` checker fixes, found triaging the first estate-wide
scorecard against fresh clones of all public repos:

- **C1**: appVersion was judged whenever `Chart.yaml` declared the key,
  not only "when the repo ships an image" as the rule itself says. A
  chart-only repository (every `goreleaser` build `skip: true`) is now
  detected, and its appVersion left alone.
- **C11**: a ko `repositories:` entry with `base_import_paths: false`
  publishes the bare repository — the checker always appended the
  build's main-package basename regardless, inventing an image never
  pushed. A `ghcr.io/...` string inside a YAML comment was also read as
  a live reference. Both fixed; the repeated-name verdict is computed
  after deduplicating images, so the same image named twice no longer
  inflates its own sibling count.
- **C11**: a component sharing a registry prefix with sibling images is
  only flagged as "repeats the repository name" when it is the SOLE
  image under that prefix — the degenerate case the rule means.
- **C12**: `@latest` was flagged anywhere in `README.md`, including
  prose warning against it. Now only matched inside a fenced code block.
- **New: a named-exemption mechanism.** `.github/policy-conformance.yaml`'s
  `exempt:` map names a rule, a reason, and — for a chart-scoped rule
  (C1, C2) — which charts it covers, for the genuine cases where a rule's
  plain text does not fit a repository's kind (a library chart, a fork
  of a non-MIT upstream, a CRD chart whose version tracks an upstream
  pin). Distinct from the existing `skip:` input: an exemption is
  committed and reviewed, not silenced per run. C9 reports a new
  `EXEMPT` verdict rather than `PASS`; C5's exemption suppresses only
  the missing-latest-tag-heading sub-check. See
  [truvity/policy `docs/contracts/component.md`'s Exemptions
  section](https://github.com/truvity/policy/blob/master/docs/contracts/component.md#exemptions).
- `hack/policy-conformance-cases.sh` gets one case per fix above, plus
  the exemption mechanism.

## v1.2.0

- **`policy-conformance`**, a new action: checks the calling repository
  against the component contract's rules C1 to C12 (truvity/policy
  `docs/contracts/component.md`) and prints one line per rule.
  Report-only by default; `strict: true` fails the job. Self-check runs
  it against this repository, and `hack/policy-conformance-cases.sh`
  proves each rule fails on its own defect.
- `tagged-pins` judges `truvity/ci-cache` too, and searches the whole
  checkout rather than `.github` only, so a pin inside a composite action
  is seen.
- `setup-devbox` pins `truvity/ci-cache/setup` at the v0.2.0 release.
  v1.0.0 and v1.1.0 pinned an untagged commit.
- `caller-parity`: the `golangci-depguard` kit is on, arrays compare as
  sets, and `hack/policy-kit-current.sh` fails self-check when the kit
  drifts from its source in truvity/policy.
- This repository's `auto-release.yaml` is standalone instead of calling
  truvity/ci-workflows, which pins these actions; the two libraries no
  longer pin each other.
- `renovate.json` extends the estate preset, `github>truvity/ci-workflows`.
- README reworked around the per-action table; `cluster` has its own
  [README](cluster/README.md).

## v1.1.0

2026-09-26.

- **`cluster`**, a new action: one composite for both end-to-end tiers.
  `mode: kind` stands up truvity/policy's `hack/kind` box at a pinned
  release (policy v0.8.1 in self-check), with `background: true` and
  `mode: wait`; `mode: shared` refuses a fork pull request, then resolves
  the caller's kubeconfig, AWS config and registry login. Both export the
  same five variables.
- `caller-parity`: a kit can be a block inside a file (`kits/kits.yaml`)
  and can ship disabled. The `golangci-depguard` kit ships disabled.
- The `setup-devbox` / `truvity/ci-cache/setup` seam case reads the
  required inputs from ci-cache's `action.yaml` at the pinned SHA, and
  fails when it cannot fetch it.
- Licensed MIT.

## v1.0.0

2026-09-24. The composite actions truvity/ci-workflows is built from,
moved here from ci-workflows with their history, so a workflow change
and an action change stop needing three pull requests.

- `setup-devbox`: devbox, proto and the toolchain; CodeArtifact login;
  a GitHub App token for private-repository reads; the fleet caches,
  delegated to `truvity/ci-cache/setup`. `go-cache-server` is retired
  and warns when passed.
- `recipe`: one task-runner recipe, then a clean-tree check.
- `fleet-discover`: which repositories a fleet job touches, from an
  enrolment list, and whether each default branch is gated (a ruleset
  counts as a required check).
- `caller-parity`: compares a repository's shared caller workflows
  against the canonical kits, and reports.
- `devbox-parity`: refreshes devbox packages and aligns a Go
  repository's toolchain triple.
- `openbao-secrets`: a job reads its third-party secrets from OpenBao at
  run time.
- `setup-remote-builders`: the remote builders a cross-architecture
  image build needs.
- `public-runners`: refuses a public repository pointed at self-hosted
  runners.
- `tagged-pins`: refuses a pin into the shared CI library that names no
  release.
- `self-check.yaml` runs the `hack/` case harnesses under the `check`
  context, which is the whole merge gate.
