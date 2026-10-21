# ci-actions

The composite actions [truvity/ci-workflows](https://github.com/truvity/ci-workflows)
is built from: bootstrapping a runner, running a recipe, guarding pins
and runners, discovering and comparing the fleet, standing up an
end-to-end cluster, and checking a repository against the component
contract.

## What ships

| action | what it does | key inputs | used by (ci-workflows) |
| -- | -- | -- | -- |
| [`setup-devbox`](setup-devbox/action.yaml) | Installs devbox, proto and the toolchain, logs into CodeArtifact, mints a GitHub App token for private-module reads, and wires the caches by delegating to [`setup-cache`](setup-cache/action.yaml) | `go-cache-bucket`, `go-cache-region`, `goproxy`, `npm-registry`, `bazel-remote`, `go-private`, `module-app-client-id`, `aws-config-file`, `codeartifact-domain`, `devbox-config` | `check`, `integration`, `release-public`, `release-private`, `parity-fleet` |
| [`setup-cache`](setup-cache/action.yaml) | Detects the build systems a repository uses and wires each to the estate's cache (Go build cache through go-cache-plugin to the bucket; checks the Yarn and moon settings). Nested: only `setup-devbox` calls it, moved here from `truvity/ci-cache` | `bucket`, `region`, `endpoint`, `path-style`, `goproxy`, `npm-registry`, `bazel-remote`, `languages`, `client-version` | none directly (through `setup-devbox`) |
| [`recipe`](recipe/action.yaml) | Runs one task-runner recipe inside devbox, then fails if the working tree changed | `recipe` (required), `command` (`just`), `devbox-config` | `check` |
| [`s3-cache`](s3-cache/README.md) | `actions/cache` against the CI cache bucket (zstd tar, keys and restore-keys), falling back to GitHub's cache without a bucket or without credentials (decided by configuration, not by runner type) | `path`, `key`, `restore-keys`, `bucket`, `region` (`restore` and `save` sub-actions) | `check` (golangci-lint cache), `integration` (yarn cache), `setup-devbox` (proto) |
| [`stable-mtimes`](stable-mtimes/README.md) | Gives every tracked file a content-derived modification time right after checkout, so Go's test cache hits when the content is unchanged | `working-directory` | `check` (default on, opt out with `stable-mtimes: false`) |
| [`public-runners`](public-runners/action.yaml) | Refuses a public repository that asks for self-hosted runners | `runners` (required), `visibility` | `check`, `integration`, `release-public` |
| [`tagged-pins`](tagged-pins/action.yaml) | Refuses a pin into the shared CI libraries that is not the commit of a tag | `libraries` (ci-workflows, ci-actions) | `check` |
| [`policy-conformance`](policy-conformance/README.md) | Checks a repository against the component contract, rules C1 to C13 and C18, one line per rule | `strict` (`false`), `skip`, `reason`, `runner-code` (`error`) | `check`, opt-in |
| [`cluster`](cluster/README.md) | Stands up the end-to-end cluster (a disposable kind box, or a shared cluster) behind one set of outputs | `mode` (required), `policy-version`, `namespace`, `background` | `integration` (kind tier) |
| [`setup-remote-builders`](setup-remote-builders/action.yaml) | Attaches the remote BuildKit builders a cross-architecture image build needs, over mutual TLS when the runner provides a client certificate | `remote-builders` (required) | `integration` |
| [`setup-buildkit-tls`](setup-buildkit-tls/action.yaml) | Settles where the runner's BuildKit client certificate comes from: `CI_BUILDKIT_TLS_DIR` as it is, or three PEM environment variables written to a fresh directory and exported as `CI_BUILDKIT_TLS_DIR` | none (reads the runner's environment) | before the "Plan the remote builders" steps (once adopted) |
| [`openbao-secrets`](openbao-secrets/action.yaml) | Reads a job's third-party secrets from OpenBao at run time | `issuer`, `address`, `path` (required), `keys` | `release-private` |
| [`token-exchange`](token-exchange/action.yaml) | Exchanges the job's GitHub identity token at a roster issuer for the clusters, cloud roles and GitHub App tokens its rules grant (curl and jq only, nothing downloaded) | `issuer` (required), `audiences`, `kubeconfig`, `default-profile`, `region`, `github-app`, `repositories`, `permissions` | `auto-release`, `renovate-fleet`, `parity-fleet`, `pkl-fleet` |
| [`fleet-discover`](fleet-discover/action.yaml) | Decides which repositories a fleet job touches, and whether each default branch is gated | `token` (required), `estate`, `require-check`, `repositories` | `renovate-fleet`, `parity-fleet` |
| [`caller-parity`](caller-parity/action.yaml) | Compares each repository's shared caller workflows (and the depguard block) against the kits in [`caller-parity/kits/`](caller-parity/kits/), and reports | `token`, `repositories` (required), `fail-on-diff` (`false`) | `parity-fleet` |
| [`devbox-parity`](devbox-parity/action.yaml) | Refreshes devbox packages and aligns a Go repository's toolchain triple, opening a pull request | `token` (required), `mode`, `module-dirs` | `parity-fleet` |
| [`auto-release`](auto-release/action.yaml) | The steps of the shared auto-release workflow: `gate` (a security-labelled merge or a hand-written fix releases now, the rest waits for the batch) and `tag` (cuts the next patch tag, behind a CHANGELOG heading pull request when the release needs one) | `step`, `token`, `repository` (required), `sha`, `tag-prefix`, `bot`, `changelog-heading`, `version-bump-command` | `auto-release` (once released and adopted) |
| [`publish-nix-flakes`](publish-nix-flakes/action.yaml) | Generates, checks and uploads a Nix flake per GoReleaser archive id, deterministically packed | `flakes`, `flake-dir`, `token` (required) | `release-public` (once released and adopted) |
| [`publish-charts`](publish-charts/action.yaml) | Refuses a wrong chart name before anything is pushed, then packages and pushes each chart with helmctl | `charts`, `chart-root`, `registry`, `app-version-mode`, `helmctl-version`, `token` (required), `images`, `require-image-digests` | `release-public` (once released and adopted) |
| [`require-green-checks`](require-green-checks/action.yaml) | Refuses a release unless the tagged commit has a successful `check` and nothing else on it is red or pending | `token` (required) | `release-private` (once released and adopted) |
| [`release-pkl`](release-pkl/action.yaml) | The steps of the shared Pkl-package release: read the declared version, refuse a tag that is not it or has no changelog heading, check the assets and their checksums, publish (safe to re-run), smoke-test the published packages | `step` (required), then per step: `tag`, `version`, `manifest`, `repository`, `token`, `pkl-command`, `smoke-import` | `release-pkl` (once released and adopted) |
| [`pkl-fleet`](pkl-fleet/action.yaml) | The per-repository steps of the Pkl contracts fleet bump: rewrite the dependency URIs (refusing what it cannot classify), resolve and regenerate, open or update the pull request | `step` (required), then per step: `target`, `source`, `dirs-file`, `token`, `repository`, `base`, `version`, `from`, `breaking` | `pkl-fleet` (once released and adopted) |
| [`fleet-step`](fleet-step/action.yaml) | Small fleet steps: print the job's OIDC claims, resolve the commit author, read a repository's parity settings, approve non-major renovate pull requests, summarise the majors available across an estate | `step` (required), then per step: `token`, `repository`, `user-login`, `run-mode` | `parity-fleet`, `renovate-fleet`, `pkl-fleet` (once released and adopted) |
| [`token-inputs`](token-inputs/action.yaml) | Checks a fleet caller's token inputs against its `token-source`, reporting every fault at once | `source`, `secret-name` (required), `has-key`, `id`, `extra-keys` | `parity-fleet`, `renovate-fleet`, `pkl-fleet`, `auto-release` (once released and adopted) |
| [`enrolment-list`](enrolment-list/action.yaml) | Reads a dotted-path list of repository names from the caller's repositories file, as JSON for `fleet-discover` | `file`, `list` (required) | `parity-fleet`, `renovate-fleet`, `pkl-fleet` (once released and adopted) |

Every input is described in full in its `action.yaml`.

## Who it is for

Mostly for truvity/ci-workflows, which is the only thing a repository in
either organisation pins: its reusable workflows call these actions, so
a repository gets them without naming this repository at all. Pin an
action here directly when you are writing a workflow that
ci-workflows does not provide, or when you maintain ci-workflows itself.

## The model

The actions lived in ci-workflows until 2026-09-24. A workflow cannot
pin the commit that introduces an action in its **own** repository: that
commit does not exist until the merge, and rebase-merge rewrites it. So
every action change was a three-step dance (land the action, tag,
land a second pull request moving the pin) with `master` red in between.
Across repositories that problem does not exist: ci-workflows pins
`truvity/ci-actions/<action>@<sha>` the way it pins any third party.

**Pin the commit of a tag, never a tag or a branch.** Every repository in
both organisations executes this code, so a compromise here reaches the
whole estate. Tags move; commit SHAs do not. Take the commit, not the
annotated tag object (`git rev-parse vX.Y.Z^{commit}`); a `uses:` at a
tag object resolves to nothing. `tagged-pins` refuses anything that is
not the commit of a tag.

## Install and a worked example

Nothing to install: GitHub fetches an action from its `uses:` line. The
smallest useful job, on a public repository:

```yaml
jobs:
  guard:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          lfs: true # fetch Git LFS objects; see "Git LFS" below
      - uses: truvity/ci-actions/public-runners@f9130c63db2899c18884c027a7eade20d0ab796a # v1.18.0
        with:
          runners: ubuntu-latest
          visibility: ${{ github.event.repository.visibility }}
      - uses: truvity/ci-actions/tagged-pins@73a8e54d28025275f2b2750ae4bfc56f008b36cb # v1.3.0
```

The first action prints `ok ubuntu-latest` and `public repository,
hosted runners only — checked`; the second prints one `ok` or `UNTAGGED`
line per pin into the shared CI libraries and fails on an `UNTAGGED`
one. A repository that calls ci-workflows' `check.yaml` already runs
both.

## Runner-provided overrides

The cache endpoints are org-wide variables (`CI_GOPROXY`, `CI_NPM_REGISTRY`,
`CI_BAZEL_REMOTE_CACHE`, `CI_REMOTE_BUILDERS`), one value for every runner a
workflow may land on. A runner pool that reaches the same services by other
names (a container-task pool going through a private gateway, where a Kubernetes
pod uses in-cluster service names) would need a variable of its own. Instead the
**runner exports its own value**, in the process environment of the runner (a pod
spec, a task definition), and that wins:

| the runner exports | overrides | read by |
| -- | -- | -- |
| `CI_RUNNER_GOPROXY` | the `goproxy` input | `setup-devbox` (and so `setup-cache`) |
| `CI_RUNNER_NPM_REGISTRY` | the `npm-registry` input | the "Point npm and yarn" steps of ci-workflows (`setup-devbox` logs it) |
| `CI_RUNNER_BAZEL_REMOTE_CACHE` | the `bazel-remote` input | `setup-devbox`, which exports the winner to the job as `CI_BAZEL_REMOTE_CACHE` |
| `CI_RUNNER_REMOTE_BUILDERS` | the `remote-builders` input | `setup-remote-builders`; the "Plan the remote builders" steps of ci-workflows |

Precedence, per value: **runner environment, then the input (the caller's
organisation variable), then the action's default.** With no `CI_RUNNER_*` set
nothing changes. The names are distinct from the variables and inputs they
override, so they never collide with them. Each step logs which source won and the
address with any `user:password@` removed. A runner value holding whitespace or a
control character (a list of builders may hold whitespace between entries) fails
the step; a malformed value never falls back silently to the caller's.

Example, for a pool whose gateway serves all four: `CI_RUNNER_GOPROXY=https://gateway.example/goproxy,direct`,
`CI_RUNNER_NPM_REGISTRY=https://gateway.example/npm`,
`CI_RUNNER_BAZEL_REMOTE_CACHE=grpcs://gateway.example:443`,
`CI_RUNNER_REMOTE_BUILDERS="linux/amd64=tcp://builder.example:1234"`.

## BuildKit client certificate

The remote builders are dialled with mutual TLS when the runner provides a client
certificate. The runner provides it in one of two forms, in its own process
environment:

| the runner exports | form | typical runner |
| -- | -- | -- |
| `CI_BUILDKIT_TLS_DIR` | a directory holding `ca.pem`, `cert.pem` and `key.pem` | a Kubernetes pod with a mounted Secret |
| `CI_BUILDKIT_TLS_CA_PEM`, `CI_BUILDKIT_TLS_CERT_PEM`, `CI_BUILDKIT_TLS_KEY_PEM` | the three PEM documents as values | a container task with secrets injected as environment variables |

`CI_BUILDKIT_TLS_SERVERNAME` (the certificate name the builders are verified
against, when it is not the endpoint host) is separate and unchanged.

The [`setup-buildkit-tls`](setup-buildkit-tls/action.yaml) action, run before the
step that reads `CI_BUILDKIT_TLS_DIR`, makes the second form look like the first:

1. `CI_BUILDKIT_TLS_DIR` is set: nothing changes, the PEM variables are ignored.
2. It is unset and all three PEM variables are set: each value is checked to be
   PEM of the right type (certificates for the CA and the client certificate, one
   unencrypted private key), then written to a fresh directory under
   `$RUNNER_TEMP` (mode 0700) as `ca.pem`, `cert.pem` and `key.pem` (key mode 0600),
   and that directory is exported as `CI_BUILDKIT_TLS_DIR` to the later steps.
3. Only some of the three are set: the step fails and names the missing ones. A
   value of the wrong type, an encrypted key, or text that is not PEM fails it too,
   naming the variable and never the value.
4. None is set: nothing happens.

No value is printed. The key is registered with `::add-mask::` line by line before
anything else is written, the action takes no inputs (so the key never passes
through an `env:` block that step debug logging would expand), and the log says
only which source won: `TLS material from runner env` or `TLS material from
CI_BUILDKIT_TLS_DIR`. The PEM variables stay in the runner's environment for the
rest of the job; the files stay on the runner's disk for as long as the runner
lives.

```yaml
      - uses: truvity/ci-actions/setup-buildkit-tls@<commit of a release tag>
```

The logic is `ci-actions buildkit-tls` (`internal/buildkittls`, table-tested).

[`setup-remote-builders`](setup-remote-builders/action.yaml) reads the result: with
`CI_BUILDKIT_TLS_DIR` set, every node is registered with
`--driver-opt cacert=…,cert=…,key=…` (plus `servername=…` from
`CI_BUILDKIT_TLS_SERVERNAME` when that is set); with it unset the nodes are plain
TCP, as before. It takes no input for this, so a caller changes nothing but the
order: `setup-buildkit-tls` first when the runner passes the PEM variables.

## Git LFS

No action here checks out the repository: the caller does, with
`actions/checkout`. The shared convention is to fetch Git LFS objects at
checkout (`lfs: true`), because a repository that tracks images or other
binaries in LFS otherwise builds from pointer files. The reusable workflows
in truvity/ci-workflows do this by default through an `lfs` input (default
`true`; `false` opts out). It needs `git-lfs` on the runner: the ARC image
has it since ci-plane v6.7.0, and GitHub-hosted runners ship it. This
repository's own workflows follow the convention; it holds no LFS objects.

## Running without privilege

Every action here runs with **no privilege, by contract**: no root, no
`sudo`, no changes outside `$HOME`, `$RUNNER_TEMP` and the work
directory. The reference environment is a runner under the Pod Security
`restricted` profile: uid 1001, `runAsNonRoot`, `allowPrivilegeEscalation:
false` (so `no_new_privs`, and `sudo` can never work), all capabilities
dropped. GitHub-hosted runners have passwordless `sudo`; we do not use it,
with one exception named below.

- **devbox is installed without privilege.** When the runner does not bake
  devbox, `setup-devbox` fetches the release binary (`devbox-version`, a tag
  or `latest`), checks it against that release's own `checksums.txt` and puts
  it in `$RUNNER_TEMP/bin`, which it adds to `PATH`. It no longer pipes
  `get.jetify.com` into `bash`. Pin a tag to make the release a reviewed one.
- **A second, smaller devbox environment is opt-in.** `devbox-config` (on
  `setup-devbox` and on `recipe`, the same value on both) names a
  workspace-relative directory holding its own `devbox.json` and
  `devbox.lock`; every devbox call runs as `devbox run -c <dir>`, so the job
  downloads that closure instead of the repository's full one. devbox makes
  that directory the project root, and `just` finds the Justfile by searching
  parent directories, so recipes still run from the repository root. The value
  must be a relative path inside the workspace that contains a `devbox.json`;
  anything else fails with a message. Empty (the default) changes nothing. A
  second environment that ships no proto needs `proto: "false"`.
- **The one remaining root step is the nix installer.** It runs only when nix
  is not on the runner (`Install nix` in `setup-devbox`), which means a
  GitHub-hosted runner, the one place root exists. A runner that cannot
  escalate (`no_new_privs`) must bake nix into its image: the preflight says
  so in one `::error::` instead of letting the installer fail a minute later.
  Where nix is baked (the self-hosted pool, any restricted runner) no step
  here needs root at all.

- **`/bin/sh` is left alone.** `setup-devbox` used to relink it to bash;
  it no longer does. Every step in these actions says `shell: bash`
  explicitly and every script has a bash shebang.
- **Inside `devbox run`, `sh` is already bash.** devbox puts its own bash,
  with a `sh` link, on `PATH`, so `just`'s default `sh -cu` line recipes
  and devbox scripts run under bash whatever `/bin/sh` is. Verified with
  devbox 0.18 and just 1.58.
- **Outside devbox, say so.** A caller step that relies on bash
  semantics (`[[ ]]`, arrays, `pipefail`) must not assume `sh` is bash
  on a hosted runner. Either set `defaults.run.shell: bash` in the
  workflow (it applies to `run:` steps only, never to `just`), or give
  a Justfile whose recipes need bash `set shell := ["bash", "-euo",
  "pipefail", "-c"]`.
- `setup-devbox`'s first step runs the `ci-actions` binary, so the runner
  needs `curl`, `tar` and `sha256sum` (or Go): hosted runners and the actions
  runner base image have them. It then starts with a `preflight` that prints the uid, gid,
  `no_new_privs`, what `/bin/sh` is, whether `HOME`, `RUNNER_TEMP` and the
  work directory are writable, whether devbox and nix are baked or will be
  installed (and that the nix installer is the one step needing root), and,
  when nix is present, whether its store answers. It fails with one
  `::error::` naming what is missing.

`ci-actions repo-check no-escalation` fails the gate if any action file or script says
`sudo`; the Go tests of `internal/setupdevbox` check the devbox install, including that
a tampered archive is refused, and the preflight, one failure at a time. `hack/restricted-sim.sh` runs the actions' step scripts in a
container with `--user 1001:1001 --security-opt no-new-privileges
--cap-drop ALL --read-only` (see the `restricted-sim` job).

## Consumers

| consumer | what it uses |
| -- | -- |
| [truvity/ci-workflows](https://github.com/truvity/ci-workflows) | every action, from the workflows in the table above; and through them, every repository that calls ci-workflows |
| [truvity/policy](https://github.com/truvity/policy) | `cluster` (`mode: kind`), through ci-workflows' `integration.yaml` on its kind tier |

## Neighbours

- **ci-workflows → ci-actions → ci-cache; ci-plane hosts the runners.**
  [ci-workflows](https://github.com/truvity/ci-workflows) is the only
  thing a caller pins; this repository holds the composite steps;
  [ci-cache](https://github.com/truvity/ci-cache) (archived) held the cache
  engine and the go-cache-plugin client releases that `setup-cache` downloads;
  [ci-plane](https://github.com/truvity/ci-plane) is where work executes
  (runner and nix-worker images, `arc-runners`, `ci-builders`).
- **[policy](https://github.com/truvity/policy)**: the component contract
  `policy-conformance` checks, and the `hack/kind/` box `cluster` runs at
  a pinned release.
- **[sluis](https://github.com/truvity/sluis)** (formerly access-roster):
  its issuer mints this repository's tagging token in `auto-release.yaml`,
  its `sluisctl` is the exec plugin `cluster`'s `mode: shared` expects,
  and `openbao-secrets` runs it for the exchange.

## Documentation

- [`cluster/README.md`](cluster/README.md): both tiers, which one is
  neutral, and the five variables.
- [`policy-conformance/README.md`](policy-conformance/README.md): each
  rule, what it reads, and how to skip one.
- Each action's `action.yaml`: every input, with its reason.
- [CHANGELOG.md](CHANGELOG.md): every release.
- When to reach for these, in ci-workflows: the
  [estate lifecycle](https://github.com/truvity/ci-workflows/blob/master/docs/estate-lifecycle.md),
  the [fleet model](https://github.com/truvity/ci-workflows/blob/master/docs/fleet.md),
  the [devbox contract](https://github.com/truvity/ci-workflows/blob/master/docs/devbox.md)
  and [its update job](https://github.com/truvity/ci-workflows/blob/master/docs/devbox-update.md),
  and the [secret plane](https://github.com/truvity/ci-workflows/blob/master/docs/secrets.md).

## The rule that makes this repository public

**Mechanism only.** Account ids, role ARNs, registry hostnames, bucket
names, cluster names and internal DNS are caller inputs or organisation
variables, never content here. Public history cannot be unpublished, so
`hack/leak-canary.sh` enforces the rule in CI.

Public because a private repository's actions cannot be used across an
organisation boundary, and *internal* visibility needs an Enterprise
plan neither organisation has. Public is what lets the second
organisation consume these directly: no mirror, no sync job, no drift
check.

## Status

Four tags: v1.0.0 (2026-09-24), v1.1.0 (2026-09-26), v1.2.0 and v1.3.0
(both 2026-09-29). v1.2.0 and v1.3.0 have a GitHub release each,
v1.3.0 marked "Latest"; v1.0.0 and v1.1.0 do not. ci-workflows pins
v1.3.0 for every action. `policy-conformance` and `setup-devbox`'s move
to a tagged ci-cache pin, both once listed here as unreleased work,
shipped in v1.2.0; see [CHANGELOG.md](CHANGELOG.md) for what shipped
since.

## Development

`just check` runs what CI runs on every pull request. Set up your
environment with `direnv allow`, or run `devbox run just <recipe>` to
call a recipe without direnv.

There is no unit test for a composite action that is not "run its step
bodies and read what they do". That is what `hack/` is, with recipes for
each:

- `just lint` — actionlint over every workflow and composite.
- `just kit` — the golangci-depguard kit against truvity/policy.
- `just cache-env` — setup-devbox's cache delegation holds, read live at the pinned setup-cache sha.
- `just go-test` — `go vet` and the table tests of the Go CLI: the pin guard against local git remotes, `fleet pins` against a fake GitHub.
- `just pins-wrapper` — the thin `tagged-pins` wrapper that finds the binary, with the binary stubbed.
- `just no-escalation` — fail if any action file or script calls `sudo`.
- `just restricted-sim` — the step scripts under uid 1001, no_new_privs, no capabilities, read-only root (needs docker and yq).
- `just pins` — verify all pins point to release tags.
- `just runners` — verify the repository uses public runners.
- `just conformance` — run policy-conformance against this repository.
- `just leak-canary` — scan for secrets and sensitive data.
- `just check` — run all recipes (the merge gate).

All but `kit` need no network and no token. `self-check.yaml` runs every
one of them plus `policy-conformance` against this repository, under the
`check` context. `check` is the whole merge gate: no approving review,
and no bypass. Renovate opens a pull request like anyone else and
GitHub's auto-merge finishes it when `check` goes green.

`restricted-sim` needs docker, so `self-check.yaml` runs it as its own
`restricted-sim` job, outside `check` and not required.

`cluster`'s `mode: kind` needs a real kind cluster and a container
runtime, so `self-check.yaml`'s separate `cluster-kind` job proves it
end to end, outside `check` and not required.

## The `ci-actions` CLI

Logic that outgrows a shell script lives in one Go binary, `cmd/ci-actions`
(standard library only), and the composite actions become thin wrappers that
run it. `tagged-pins`, `public-runners`, `fleet-discover`, `devbox-parity`, `caller-parity`, `openbao-secrets`, `cluster`, `token-inputs`, `enrolment-list` and `auto-release` are in: each
action keeps its inputs, outputs and messages and runs `ci-actions <command>`.
The wrapper (`<action>/run.sh`, sharing `lib/ci-actions-bin.sh`) uses a `ci-actions` already on `PATH`, else the goreleaser archive of the tag
the action is pinned at (the pinned commit resolves back to its tag; the
archive is checked against `checksums.txt`), else builds the binary with Go.

```
ci-actions tagged-pins                 # env LIBRARIES, as the action's `libraries` input
ci-actions public-runners              # env RUNNERS, VISIBILITY, GITHUB_REPOSITORY, GH_TOKEN
ci-actions fleet discover              # env TOKEN, ESTATE, REQUIRE_CHECK, REQUIRE_FILE, FILTER, ENROLLED, API
ci-actions devbox-parity               # env TOKEN, WORKDIR, BASE, LABEL, MODE, FULL_DAY, MODULE_DIRS, GIT_USER, GIT_EMAIL
ci-actions caller-parity               # env TOKEN, REPOSITORIES, KITS, FAIL_ON_DIFF, API
ci-actions openbao-secrets             # env ISSUER, ADDRESS, KV_PATH, ... (no value is ever printed)
ci-actions token-inputs                # env SOURCE, ISSUER, APP, ID, ID_NAME, SECRET, HAS_KEY, EXTRA_KEYS, WARN_*
ci-actions enrolment-list              # env FILE, LIST, GITHUB_OUTPUT
ci-actions fleet-step oidc-claims      # also: commit-author, parity-settings, approve-renovate, available-majors
ci-actions pkl-fleet rewrite           # also: resolve, publish (env: see pkl-fleet/action.yaml)
ci-actions release-pkl declared        # also: checks, assets, publish, smoke (env: see release-pkl/action.yaml)
ci-actions release-gate                # env GH_TOKEN, GITHUB_REPOSITORY, GITHUB_SHA, GITHUB_RUN_ID
ci-actions publish-charts              # env CHARTS, CHART_ROOT, REGISTRY, APP_VERSION_MODE, CHART_IMAGES, REQUIRE_IMAGE_DIGESTS, HELMCTL_VERSION
ci-actions publish-nix-flakes          # env FLAKES, FLAKE_DIR, GITHUB_REPOSITORY, GITHUB_REF_NAME
ci-actions auto-release gate           # env REPO, SHA, GITHUB_OUTPUT, GITHUB_STEP_SUMMARY
ci-actions auto-release tag            # env PREFIX, BOT, REPO, BASE, HEADING_MODE, CHANGELOG, WAIT_MINUTES, VERSION_BUMP_COMMAND
ci-actions buildkit-tls                # env CI_BUILDKIT_TLS_DIR, or CI_BUILDKIT_TLS_CA_PEM, _CERT_PEM and _KEY_PEM; RUNNER_TEMP, GITHUB_ENV
ci-actions cluster kind                # also: fork-guard, wait, shared-connect, shared-finish
ci-actions fleet pins --org example-org --org other-org \
    --min-setup-devbox v1.6.1 --json pins.json
```

`fleet pins` reads every non-archived repository of the organisations with a
token from `GITHUB_TOKEN` (or `GH_TOKEN`; it is never printed), resolves
which ci-workflows and ci-actions versions each repository's workflows pin,
and reads each pinned reusable workflow at its pinned commit to find the
TRANSITIVE setup-devbox pin. The version is the tag the pinned commit IS,
never the `# vX.Y.Z` comment beside it. A setup-devbox vendored inside a
pre-split ci-workflows shows as `in-tree`, and a commit that names no release
as `untagged:<sha>`; both count as below any `--min-setup-devbox`. It also
follows the repository's own composite actions: every
`.github/actions/**/action.y*ml` is read, and so is any `uses: ./path` (a local
action may call another), so a setup-devbox pin hidden inside them is seen; a
pin found there shows as `local action <file>` in the VIA column. The RUNNERS
column says what the repository's own workflows run on (`hosted`,
`self-hosted`, `mixed`, `dynamic` for an expression, `-` when only reusable
workflows are called); `--runner-filter hosted|self-hosted` narrows the
printed table. It is reporting only: the JSON, the gate and the exit codes
ignore it. It prints a
table and, with `--json`, a machine-readable report. Exit codes: 1 when the
gate fails, 3 when a repository could not be read (a gate that cannot see a
repository must not pass it), 2 for usage.

## Releasing

A release is an annotated `vX.Y.Z` tag on `master`; add its heading to
[CHANGELOG.md](CHANGELOG.md) in the same change that is tagged. Pushing the
tag runs [`release.yaml`](.github/workflows/release.yaml), which publishes the
`ci-actions` binary archives with goreleaser.
[`auto-release.yaml`](.github/workflows/auto-release.yaml) cuts the
next patch tag each Monday when `master` has moved, and at once for a
merged pull request labelled `security`, but only while
`vars.AUTO_RELEASE` is `true` and `vars.ACCESS_ROSTER_ISSUER` is set.
Every run so far has been skipped on that guard, so all four tags were
cut by hand; v1.2.0 and v1.3.0 also got a GitHub release by hand, v1.0.0
and v1.1.0 did not. Consumers pin the tag's commit either way.

`auto-release.yaml` is standalone rather than a call into ci-workflows'
shared workflow, because ci-workflows pins these actions and the two
libraries would otherwise pin each other.

## Licence

MIT, as [LICENSE](LICENSE); the same text as ci-cache and ci-plane.
