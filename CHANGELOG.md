# Changelog

Every release of truvity/ci-actions, newest first. Pin the commit of a
tag (`git rev-parse vX.Y.Z^{commit}`), never the tag object and never
an untagged commit; `tagged-pins` refuses anything else.

## Unreleased

`policy-conformance`:

- **Now a thin wrapper around `ci-actions policy-conformance`.** Inputs, the
  line per rule (`C1 PASS: ...`), the `::warning::`/`::error::` annotations,
  the C13 finding lines, the job-summary table and the exit status are
  unchanged: the shell version's own cases (every rule broken one at a time,
  C1's mirror-chart rule, the exemption file in each shape, the C5 tag cases,
  the C10 `check`-reaches-`vuln` cases, the C11 image-name cases, the C13
  estate-fact cases and their paired exemptions, `strict`, `skip`, the job
  summary, an empty repository) plus three mirror charts (a CRD chart of each
  of two repositories, and drifted, malformed and mixed variants) ran through
  the old script and the binary in one CI run, 93 invocations, and stdout,
  stderr, the summary file and the exit status were identical. Every case is a
  Go table test now; the 1,065-line script and `hack/policy-conformance-cases.sh`
  are gone. `git` is still the command that reads tags and tracked files; the
  step needs no `jq`, `awk` or `sed`.

`setup-devbox`:

- **devbox is installed without privilege.** When the runner does not bake
  devbox, the step fetches the release binary into `$RUNNER_TEMP/bin`, checks
  it against the release's own `checksums.txt` (a mismatch is an
  `::error::` and nothing is installed), and adds that directory to `PATH`.
  It used to run `curl https://get.jetify.com/devbox | bash`, which ran an
  unverified script as the job and installed into `/usr/local/bin` through
  root. New input `devbox-version` (a release tag, or `latest`, the default:
  the same release the script chose, now resolved from the releases page's
  redirect and verified); pin a tag to make the release a reviewed one.
  **A job that ran `/usr/local/bin/devbox` by absolute path must use `devbox`
  from `PATH`.** `hack/install-devbox-cases.sh` covers the install against a
  local release, and `restricted-sim` runs it under the restricted profile.
- **The nix installer is the one remaining root step**, and it is documented
  and said out loud: it runs only when nix is not baked into the runner (a
  GitHub-hosted runner; the self-hosted pool bakes nix and never runs it).
  `preflight` now prints whether devbox and nix are baked or will be
  installed, names the nix installer as the one step that needs root, and
  fails early, in one `::error::`, on a runner that has no nix and cannot
  escalate (`no_new_privs`), where the installer could only fail a minute
  later. See "Running without privilege" in the README.

`fleet pins`:

- **A repository with no commit is no error.** GitHub answers its tree with
  `409 Git Repository is empty.`; that made the whole run exit 3. It now
  counts as a repository that pins nothing. Any other 409 is still an error.

`public-runners`:

- **Now a thin wrapper around the `ci-actions` Go binary**
  (`ci-actions public-runners`). Inputs, log lines and exit status are
  unchanged (checked byte for byte against the shell version on twelve
  fixtures in one CI run). Like `tagged-pins` it uses a `ci-actions` on
  `PATH`, else the release archive of the pinned tag, else builds with Go, so
  it now needs curl and tar or Go on the runner (a hosted runner has both).
  The visibility lookup uses the job's token against the API directly rather
  than the `gh` CLI. The binary-finding logic is shared by every wrapper in
  `lib/ci-actions-bin.sh`. The shell cases became Go table tests, and
  `restricted-sim` now also runs this wrapper with the built binary.

`fleet-discover`:

- **Now a thin wrapper around `ci-actions fleet discover`.** Inputs, outputs,
  log, summary and `GITHUB_OUTPUT` lines are unchanged (checked against the
  shell version on a stub API in one CI run). It no longer needs `curl` and
  `jq` for the rules; the wrapper needs curl and tar or Go to fetch the
  binary. The required-check rule (rulesets and classic protection, with an
  unreadable source undecided, never a skip) is the same, now table-tested in
  Go; `hack/discover-cases.sh` is gone. One fix: an invalid `filter`
  expression used to skip every repository without a word, and now also
  prints one `::warning::` saying so. `fleet pins` and `fleet-discover` share
  one GitHub client (`internal/ghapi`).

`devbox-parity`:

- **Now a thin wrapper around `ci-actions devbox-parity`.** Inputs, outputs
  (`changed`, `pull-request`) and log lines are unchanged: twelve scenarios
  (full and auto modes, a raised toolchain, a language above the cap, a
  playwright follower, several modules, an open PR, each auto-merge gate)
  run through the old inline shell and the binary in one CI run, and the log,
  the `gh` calls, the outputs and the resulting git branch were identical.
  `devbox`, `git` and `gh` are still the commands that run; the logic around
  them (mode, version comparison, `go.mod` and `package.json` reading, the
  movement gate and the auto-merge gate) is Go, table-tested. The five steps
  became one, with `::group::` markers, and the token is passed only to the
  processes of the pull-request phase, not to `devbox update` or the
  lockfile regeneration.
- Three differences, all fixes: `gh` is downloaded only when missing, and now
  verified against the release's checksums; the playwright pin is rewritten
  in place in `package.json`, so a file without `dependencies` or
  `devDependencies` no longer gains a `"dependencies": null` key and the rest
  of the file is never reformatted; the auto-merge gate reads the branch
  rules over the API with the token rather than through `gh api` (same
  answer, shared with `fleet-discover` in `internal/gates`).

`caller-parity`:

- **Now a thin wrapper around `ci-actions caller-parity`.** Inputs, outputs
  (`differences`, `absent`), log lines, summary table and diffs are unchanged:
  the shell version's own cases (the dropped trigger, added inputs and
  blocks, a staggered cron, a library pin renovate has not moved, 403 and
  gone repositories, a kit turned off, and the whole depguard-block set) ran
  through the old script and the binary in one CI run, and stdout, summary,
  outputs and exit status were identical. The cases became Go table tests.
  The wrapper needs curl and tar or Go, and no longer needs `yq` or `jq` on
  the runner; the report's diffs still come from `diff -u`.
- **First dependency of the Go module: `go.yaml.in/yaml/v3`** (the
  maintained continuation of `gopkg.in/yaml.v3`, which the depguard kit
  itself bans for Go repositories). It replaces `yq` for reading
  `kits/kits.yaml` and comparing a block of a lint configuration as sorted
  data, which a hand-written parser would get wrong on anchors, flow style
  and multi-line scalars. Nothing else uses it.
- One difference: a `kits.yaml` that is not valid YAML is now an error; the
  shell version read every setting as unset and compared whole files.

`openbao-secrets`:

- **Now a thin wrapper around `ci-actions openbao-secrets`.** Inputs, outputs
  (`value`, `env-file`), the env file, every log and `::error::` line and
  every `::add-mask::` (the same lines, in the same order, before anything is
  written) are unchanged: sixteen scenarios (a single value, several keys
  with quotes and a multi-line secret, `keys` picking, a missing key, values
  that are not strings, an empty path, a bad variable name, a refused login,
  a login with no token, a refused read, accessctl failing or returning
  nothing, no id-token, a missing input, a private CA, custom mounts) ran
  through the old inline shell and the binary against one stub OpenBAO in
  one CI run, and stdout, the env file and its mode, the step outputs, what
  the server saw and the exit status were identical. The Go tests assert
  that no secret value is printed anywhere but as an `::add-mask::`
  argument, that the login is revoked even when the read fails, and that the
  token goes in a request body, never in a URL.
- `accessctl` is still the executed command that does the exchange (through
  devbox when the repository has a `devbox.json`, else from `PATH`). The
  HTTP calls are Go's, so the step no longer needs `curl` or `jq`; the
  wrapper needs curl and tar or Go. A `ca-cert` is trusted alone, as
  `curl --cacert` did, and one that holds no certificate is now named in an
  `::error::` instead of failing inside TLS. The `value` output's heredoc
  delimiter is random rather than time-based.

`cluster`:

- **Now thin wrappers around `ci-actions cluster <step>`** (`fork-guard`,
  `kind`, `wait`, `shared-connect`, `shared-finish`). Inputs, the five
  outputs (`kubeconfig`, `snapshot-registry`, `gemaal-tier`,
  `gemaal-namespace`, `gemaal-release`), their `$GITHUB_ENV` twins, the
  state-dir files a background launch leaves for its `wait`, every log and
  `::error::` line and the exit status (a failed box exits with the box's
  own code) are unchanged: twenty-eight scenarios (the fork guard on each
  payload shape, the kind launch in the foreground and the background with
  its wait, a failing box, a box that publishes no registry on 5001, defaults
  and explicit names, the shared connect with matching and differing
  identities, retries and every missing input, and the finish step) ran
  through the shell scripts and the binary in one CI run, and stdout, the exit
  status, `GITHUB_ENV`, `GITHUB_OUTPUT` and the state files were identical.
  `hack/fork-guard-cases.sh` and the six scripts are gone; the cases are Go
  table tests. `docker`, `kind` (through the policy box's own `up.sh`) and
  `kubectl` are still the commands that run. The `cluster-kind` job is
  unchanged and still proves the real box end to end.
- One difference: a fork guard whose event payload is not JSON still refuses
  (the one guard that must fail closed) but now with exit status 1 rather
  than `jq`'s 5, and says what was wrong on stderr.
- The binary the wrappers resolve is now kept per pinned commit under
  `$RUNNER_TEMP`, so the five steps of one `cluster` call in a job fetch the
  release archive once, not five times.

## v1.7.0

`policy-conformance`:

- **C1 accepts a declared mirror chart.** A chart that republishes a third-party artifact unchanged (vendored CRDs) declares `annotations: {truvity.io/mirror: "<owner>/<repo>@<version>"}` in `Chart.yaml`; C1 then requires its `version` (and `appVersion`, when present) to equal that `<version>` instead of `0.0.0`. An undeclared chart with any other version still fails, as does an annotation not shaped `<owner>/<repo>@<version>` or a version that differs from it. Every other chart in the repository is still judged `0.0.0`. Cases added to `hack/policy-conformance-cases.sh`.

No action needs privilege any more: no root, no `sudo`, no relinking of
`/bin/sh`, by contract (see "Running without privilege" in the README).

`setup-devbox`:

- **BEHAVIOUR CHANGE for GitHub-hosted callers: `/bin/sh` is no longer
  relinked to bash.** The "Use bash as sh" step is removed. It ran
  `sudo ln -sf /bin/bash /bin/sh`, which cannot work under a Pod Security
  `restricted` runner (no_new_privs), and v1.6.1 only guarded it. On a hosted
  runner `/bin/sh` stays dash. Inside `devbox run`, `sh` is bash already
  (devbox puts its own bash on `PATH`), so devbox scripts and `just` recipes
  are unaffected. A caller `run:` step that needs bash semantics must set
  `shell: bash` or the workflow `defaults.run.shell: bash`; a Justfile whose
  recipes need bash should `set shell := ["bash", "-euo", "pipefail", "-c"]`.
  All actions in this repository already say `shell: bash` on every step.
- **New first step, `preflight`.** Prints the uid and gid, whether
  `no_new_privs` is set, whether `/bin/sh` is bash, whether `HOME`,
  `RUNNER_TEMP` and the work directory are writable and, when nix is on
  `PATH`, whether its store answers. Fails early with one `::error::` naming
  what is missing and what to do. `no_new_privs` and a dash `/bin/sh` are
  reported, not failures.
- Scratch files go to `$RUNNER_TEMP` instead of `/tmp` (the proto log here,
  the gh download in `devbox-parity`), so a read-only root filesystem works.

Tests: `hack/no-escalation-cases.sh` fails if any action file or script contains
`sudo`; `hack/preflight-cases.sh` covers the preflight; a new
`restricted-sim` job in `self-check` runs the step scripts in a container
with `--user 1001:1001 --security-opt no-new-privileges --cap-drop ALL
--read-only` and fails when the pre-v1.6.1 `sudo` step is put back.

`tagged-pins`:

- **Now a thin wrapper around the new `ci-actions` Go binary**
  (`ci-actions tagged-pins`). Inputs, outputs, log lines and exit status are
  unchanged (checked byte for byte against the shell version on this
  repository and on ci-workflows). The wrapper uses a `ci-actions` on `PATH`,
  else the release archive of the tag the action is pinned at (verified
  against `checksums.txt`), else builds the binary with Go. A pin to a commit
  that is not a release tag therefore needs Go on `PATH`; consumers pin tag
  commits, so they get the archive. `hack/tagged-pins-cases.sh` became Go
  table tests.

New:

- **`ci-actions fleet pins` follows repo-local composite actions.** It reads
  every `.github/actions/**/action.y*ml` of a repository and every
  `uses: ./path` (a local action calling another, cycles included), and the
  local actions of a pinned reusable workflow at its pinned commit, so a
  setup-devbox reached only through them is no longer a blind spot. The VIA
  column shows `local action <file>` for such a pin. A repository whose tree
  GitHub truncates is reported as an error, never passed.
- **`fleet pins` has a RUNNERS column** (`hosted`, `self-hosted`, `mixed`,
  `dynamic`, `-`) read from each job's `runs-on`, with `runner`,
  `runner_kinds` and `runner_labels` in the JSON, and a `--runner-filter
  any|hosted|self-hosted` that narrows the printed table. Reporting only: it
  gates nothing and changes no exit code.

- **`ci-actions fleet pins`**: per repository of the given organisations,
  which ci-workflows and ci-actions versions its workflows pin, including the
  transitive setup-devbox pin read from the pinned reusable workflow at its
  pinned commit. Prints a table and, with `--json`, a report.
  `--min-setup-devbox vX.Y.Z` exits non-zero when any repository resolves
  below it. Reads the token from `GITHUB_TOKEN` or `GH_TOKEN` and never prints
  it.
- `.goreleaser.yaml` and a tag-triggered `release.yaml` publish the binary.

## v1.6.1

Not yet released.

`setup-devbox`:

- **"Use bash as sh" no longer needs `sudo` when `/bin/sh` is already bash.**
  Runner images that ship the link (the ARC runner image does) skip the step's
  work, so it also passes on a runner that cannot escalate privilege, such as a
  Pod Security `restricted` pod. Hosted runners, where `sh` is dash, still run
  `sudo ln -sf`.

## v1.6.0

`setup-devbox`:

- **Warns when `devbox.json` `env` sets `AWS_CONFIG_FILE` or `AWS_PROFILE`.**
  `devbox run` re-applies the env block over the CI AWS config, so the CI
  identity (OIDC) is lost and build tooling such as the Go cache plugin
  cannot authenticate. Set the variable in `shell.init_hook` with a
  `${VAR:-default}` fallback instead. A warning, not a failure.

## v1.5.0

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
