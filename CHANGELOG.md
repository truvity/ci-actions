# Changelog

Every release of truvity/ci-actions, newest first. Pin the commit of a
tag (`git rev-parse vX.Y.Z^{commit}`), never the tag object and never
an untagged commit; `tagged-pins` refuses anything else.

## Unreleased

## v1.12.0

- **New action `fleet-step`** (and `ci-actions fleet-step <step>`): five small
  shell steps of the fleet workflows (about 150 lines, curl and jq): printing
  the job's OIDC claims (two copies), resolving the commit author (two
  copies), reading a repository's parity settings, approving the non-major
  renovate pull requests whose review is required, and the table of majors
  available across an estate. They read the API themselves. Compared with the
  shell on a local API double: the same stdout, `GITHUB_OUTPUT`, summary, API
  calls and headers in 84 scenarios, 60 of them randomised renovate reports for
  the majors table (against jq). Nothing in ci-workflows changes until it adopts
  the action after a release. One difference: the shell split the approve loop's
  rows on whitespace, so a label containing a space broke it; the Go step does
  not.

- **New action `pkl-fleet`** (and `ci-actions pkl-fleet rewrite|resolve|publish`):
  the three per-repository shell blocks of ci-workflows' `pkl-fleet.yaml`
  (about 235 lines: rewriting the dependency URIs, resolving and regenerating,
  and opening or updating the pull request), as Go. The URI rewrite refuses
  before it writes on anything it cannot classify and reports every refusal; a
  bump is breaking for a new major, a new minor below 1.0 or a prerelease. The
  pull request step reads and writes the API itself (the shell used curl and
  jq) and pushes only when the branch's content differs. The cases of
  `hack/pkl-fleet-cases.sh` for these blocks run unchanged against the binary
  and are now Go tests, with the API calls and request bodies compared with
  what the shell sent. Nothing in ci-workflows changes until it adopts the
  action after a release.

- **New action `release-pkl`** (and `ci-actions release-pkl <step>`): the five
  shell steps of ci-workflows' `release-pkl.yaml` (about 190 lines: the
  declared version, the tag and changelog refusals, the asset and checksum
  check, the re-runnable publish, and the smoke test with its retry backoff),
  as Go. The original `hack/release-pkl-cases.sh` passes unchanged against the
  binary (every assertion: refusal texts, the notes section, the manifest,
  create, resume and refuse-on-different-bytes behaviour, the PklProject and
  smoke module written, retries); those cases are now Go tests too. Nothing in
  ci-workflows changes until it adopts the action after a release.

## v1.11.0

- **New action `require-green-checks`** (and `ci-actions release-gate`): the
  102-line "Require green checks on the tagged commit" step of ci-workflows'
  `release-private.yaml` (the gate that refuses a release built from a commit
  whose CI has not finished), as Go. It reads the commit's check runs from the
  API itself (the shell used curl and jq). The filter (newest run of each
  check by id, never by start time; this run's own checks by run id; sibling
  release jobs and dependency-bot checks by name) gives the same lines as the
  original jq program on 300 randomised inputs, and the same log lines, error
  text and exit status; the step's embedded self-test is now the Go tests.
  Nothing in ci-workflows changes until it adopts the action after a release.

- **New action `publish-charts`** (and `ci-actions publish-charts`): the 109-line
  "Package and push charts" step of ci-workflows' `release-public.yaml` (an
  embedded Python resolver and the helmctl download, package and push loop),
  as Go. The `charts` input resolves to the same directories and names, and
  every refusal prints the same `::error::charts: ...` text (Python's `repr`
  quoting included) and exits 1 before anything is built or pushed; the
  `hack/chart-paths-cases.sh` cases are Go tests. helmctl is invoked with the
  same arguments in the same order, in the same three modes (release
  appVersion, chart appVersion, goreleaser manifest). `python3` is no longer
  needed on the runner for it. Nothing in ci-workflows changes until it adopts
  the action after a release.

- **New action `publish-nix-flakes`** (and `ci-actions publish-nix-flakes`): the
  149-line "Publish Nix flakes" step of ci-workflows' `release-public.yaml`
  (a Python generator and the nix, tar and gh loop), as Go. The generated
  `flake.nix` is byte-identical to the Python generator's on the same
  `dist/artifacts.json` and `devbox.lock` (the tests compare against output
  that generator produced), the archive is the same deterministic
  `tar | gzip -n` pipeline, and the same refusals print the same `::error::`
  lines. `python3` is no longer needed on the runner for it. Nothing in
  ci-workflows changes until it adopts the action after a release.

- **New action `auto-release`** (and `ci-actions auto-release gate|tag`): the two
  shell blocks of ci-workflows' `auto-release.yaml`, each written twice (once
  per token source), as Go. `gate` decides whether a push to the default branch
  releases now (a security-labelled merge, a `fix:` PR title, a `fix:` commit
  in an unconventionally titled merge or pushed directly) or waits for the
  weekly batch (`skip=true`); `tag` cuts the next patch tag, writing the
  CHANGELOG heading through a pull request first when the release needs one
  (renaming `## Unreleased`, or adding a dependency-updates heading), running
  the caller's `version-bump-command` in that commit, and waiting for the merge
  so the tag names the commit that carries its own heading. The same cases
  `hack/auto-release-cases.sh` ran against the shell run against the binary and
  pass unchanged; they are now Go tests too (a real git repository and a bare
  origin, with a fake `gh` and `devbox`). Nothing in ci-workflows changes until
  it adopts the action after a release.

## v1.10.0

Two checks the fleet workflows of ci-workflows carry as inline shell, as Go:

- **`token-inputs`** (action and `ci-actions token-inputs`): the check that a
  caller's token inputs agree with its `token-source`, written four times in
  `parity-fleet`, `renovate-fleet`, `pkl-fleet` and `auto-release`. Same
  `::error::` and `::warning::` lines, same order, every fault reported, exit 1
  on any. The id input's name, the secret's name, the extra App key pairs and
  the two warnings are inputs, so one command serves all four shapes.
- **`enrolment-list`** (action and `ci-actions enrolment-list`): reads a
  dotted-path list from the repositories file and writes `names=<compact JSON>`
  to `GITHUB_OUTPUT`, as the `yq`/`jq` block did, but without needing either
  on the runner. Absent file, absent or empty list: the same errors.
  Difference: only a plain dotted path is read (no yq expression syntax).

Nothing in ci-workflows changes until it adopts them after a release.

## v1.9.0

- **New action `token-exchange`**, moved from the root `action.yml` of
  truvity/access-roster so that nothing needs to name that repository. The
  inputs, outputs and behaviour are unchanged (a caller changes only its
  `uses:` line): the job's GitHub identity token is exchanged at the issuer
  with `curl` and `jq`, and the profiles, kubeconfig and GitHub App token are
  written as before. The step body is `token-exchange/run.sh`, run as written
  by `hack/token-exchange-cases.sh` in `just check`. The action downloads no
  binary, so there is no release URL or checksum to configure.

## v1.8.0

This repository's own gate:

- **`hack/no-escalation-cases.sh` and `hack/policy-kit-current.sh` are now
  `ci-actions repo-check no-escalation` and `ci-actions repo-check
  policy-kit`**, next to `repo-check cache-seam`. The scan for the
  privilege-escalation command (whole word, comments included, prose and the
  simulation's control image exempt, binary files unread) reports the same hits
  and exit status as the script on the same fixtures, and the scan's own
  cases (the call planted in each shape it must catch, and the shapes it must
  leave) are Go tests. The depguard-kit check reads the tagged blob of the
  policy repository over HTTPS exactly as before and prints the same lines and
  the same `diff -u` on drift. With these, no shell script of this
  repository's gate is left but the leak canary, the wrapper cases and the
  restricted-runner simulation, which are about shell by nature.

`setup-devbox`, the rest of its logic:

- **Every step that was inline shell or a script is now a step of `ci-actions
  setup-devbox <step>`** (`preflight`, `aws-config`, `detect-baked`,
  `strip-tools`, `install-devbox`, `materialize`, `proto`, `expose-token`,
  `go-private`, `codeartifact`, `retired-cache-server`, `guard-goproxy`,
  `guard-aws`); the composite keeps its structure (the third-party actions,
  the conditions, the caches) and each of its own steps is a thin wrapper.
  Inputs, outputs, every log, `::warning::` and `::error::` line, what is
  written to `GITHUB_ENV`, `GITHUB_OUTPUT` and the step summary (a
  random-delimiter heredoc, as before), and the exit statuses are unchanged:
  fifty-one scenarios (each preflight branch, the devbox install against a
  local release with a tampered archive, unlisted asset, missing release and
  `latest`, the AWS config, baked-tool detection, the `.prototools` strip, the
  closure and proto retries, the token validation including a newline, the
  `GOPRIVATE` validation and git rewrite, CodeArtifact through `aws` and
  through devbox, the retired-input warning and both devbox.json guards) ran
  through the old shell and the binary in one CI run, and stdout, the exit
  status, the files written and the git config were identical.
- **`preflight.sh`, `install-devbox.sh`, `hack/preflight-cases.sh`,
  `hack/install-devbox-cases.sh` and `hack/cache-env-cases.sh` are gone.** The
  cases are Go table tests; the cache seam (that the composite delegates to a
  SHA-pinned `truvity/ci-cache/setup`, passes every input it declares, wires
  the retired input to its warning and writes no cache itself) is
  `ci-actions repo-check cache-seam`, read live at the pinned sha and run in
  `check`.
- **Needs more of the runner than before: `curl`, `tar` and `sha256sum` (or
  Go) to fetch the binary, in the first step of every job.** The wrappers cache the resolved
  binary per pinned commit, so it is fetched once per job. The `devbox.json`
  guards no longer need `jq` on the runner.

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

`recipe` and `setup-remote-builders`:

- **Now thin wrappers around `ci-actions recipe` and `ci-actions
  remote-builders`.** Inputs, every log and `::error::` line and the exit
  status are unchanged: sixteen scenarios (a clean recipe, another task
  runner, a failing recipe, a recipe that dirties the tree or leaves an
  untracked file, a gitignored output; two nodes with and without devbox, one
  node with several platforms, the plugin link, an absent builder, empty and
  space-only nodes, a platform the builder does not offer, a failing
  registration, newline- and tab-separated entries) ran through the old
  inline shell and the binary with stand-ins for devbox, the task runners and
  docker in one CI run, and stdout, the exit status, the docker calls,
  `GITHUB_ENV`, the plugin link and the saved inspect output were identical.
  devbox, git, docker and buildx are still the commands that run. Each step
  needs bash plus curl and tar or Go.

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
  from `PATH`.** The Go tests of `internal/setupdevbox` cover the install against a
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
