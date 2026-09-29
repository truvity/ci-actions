# policy-conformance

Checks the calling repository's checkout against the component
contract's checkable rules, C1 to C12, and prints one line per rule:

```
C1 PASS: 1 chart(s) commit version 0.0.0
C5 FAIL: CHANGELOG.md has no heading for v1.2.3
C6 SKIP: no devbox.json
```

The contract lives in truvity/policy,
[`docs/contracts/component.md`](https://github.com/truvity/policy/blob/master/docs/contracts/component.md);
the rule IDs here are its IDs. This action carries no doctrine of its
own: when a rule and this README disagree, the contract wins and this
action has a bug.

## Use

```yaml
steps:
  - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
  - uses: truvity/ci-actions/policy-conformance@<sha> # vX.Y.Z
    with:
      strict: "false"
      skip: C11
      reason: "images are renamed in the next major"
```

Most repositories get it from ci-workflows' `check.yaml` by setting its
`policy-conformance: true` input rather than calling it directly.

| input | default | meaning |
| -- | -- | -- |
| `strict` | `false` | `true` fails the job on any failed rule. Otherwise each failure is a warning annotation and a job-summary row, and the step succeeds. |
| `skip` | empty | Comma-separated rule IDs not to evaluate. Each prints as `SKIP` with `reason`. An ID outside C1 to C12 fails the step. |
| `reason` | empty | Why. Required whenever `skip` is set. |
| `renovate-preset` | `github>truvity/ci-workflows` | The one preset C7 expects `extends` to name. |
| `working-directory` | `.` | The repository root to judge. |

It needs bash, git and jq, which every hosted runner has, and no
token. Run it locally from a repository root with
`bash <path>/policy-conformance.sh`.

## What each rule reads

| rule | passes when | notes |
| -- | -- | -- |
| C1 | every `charts/*/Chart.yaml` has `version: 0.0.0`, and `appVersion: 0.0.0` when it has an `appVersion` | `0.0.0-dev` fails |
| C2 | every chart directory has `values.schema.json` | |
| C3 | every chart has files under `tests/golden/<chart>/` and `tests/invalid/<chart>/`, or a `*_test.go` under `charts/` mentions an invalid fixture | the Go alternative is textual: it proves the test names a fixture, not that it asserts on it |
| C4 | `hack/leak-canary.sh` exists and the Justfile mentions it | textual: the Justfile naming the canary is taken as `just check` running it |
| C5 | `CHANGELOG.md` has `## vX.Y.Z` headings, newest first, no duplicates, at most one `## Unreleased` and only on top, and one for the latest tag | the latest tag is `git describe --tags --abbrev=0` on the default branch, after fetching tags (and unshallowing a depth-1 checkout). With no tag, that part is not checked and the line says so. Text after the version on a heading (a date) is tolerated. |
| C6 | every `devbox.json` package names a version other than `latest` | flake references are counted, not judged |
| C7 | `renovate.json` has `$schema`, `extends` is exactly the preset, every `packageRules`/`customManagers` entry has a `description`, and any other override key comes with a top-level `description` | |
| C8 | `README.md` has the eleven `##` headings, exact text, in order | other headings may sit between them |
| C9 | `LICENSE` is the MIT licence | |
| C10 | with a root `go.mod`: `.github/workflows/security.yaml` calls ci-workflows, and no other workflow lists `vuln` in its `recipes:` | skips without `go.mod` |
| C11 | no image name ends in the repository name one level below it (`<registry>/<owner>/<repo>/<repo>`) | names are built from `ko-docker-repo`/`image-repo` in `.github/workflows/release.yaml` and `repositories:` in `.goreleaser.yaml`, joined to the base of each `main:`; plus every `ghcr.io/...` written out in those files and in `charts/*/values.yaml` |
| C12 | `README.md` never says `@latest` | |

C13 (estate facts are inputs, never defaults) is not checked: whether a
value in `values.yaml` is an estate fact needs a reader, not a grep. The
action prints a line saying so.

## Tests

`hack/policy-conformance-cases.sh` builds a repository that meets every
rule, proves each says `PASS`, then breaks one rule at a time and proves
that rule, and only that rule, says `FAIL`. It also covers `strict`,
`skip` with and without a reason, the job summary, and a repository with
nothing in it. No network: `origin` is a local bare repository.
`self-check.yaml` runs it inside `check`, and runs the action itself
against this repository with `strict: false`.
