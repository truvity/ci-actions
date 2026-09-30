# policy-conformance

Checks the calling repository's checkout against the component
contract's checkable rules, C1 to C13, and prints one line per rule:

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
| C5 | `CHANGELOG.md` has `## vX.Y.Z` headings, newest first, no duplicates, at most one `## Unreleased` and only on top, and the latest tag is covered: it has a heading, or it is an automatic patch | an automatic patch is `vX.Y.Z` with Z > 0 whose `X.Y` is the `X.Y` of the newest heading; every `vX.Y.0`, every `vX.0.0`, every pre-release and any patch of a line no heading carries needs its own heading. The latest tag is `git describe --tags --abbrev=0` on the default branch, after fetching tags (and unshallowing a depth-1 checkout); with no tag, that part is not checked and the line says so. Text after the version on a heading (a date) is tolerated. |
| C6 | every `devbox.json` package names a version other than `latest` | flake references are counted, not judged |
| C7 | `renovate.json` has `$schema`, `extends` is exactly the preset, every `packageRules`/`customManagers` entry has a `description`, and any other override key comes with a top-level `description` | |
| C8 | `README.md` has the eleven `##` headings, exact text, in order | other headings may sit between them |
| C9 | `LICENSE` is the MIT licence | |
| C10 | with a root `go.mod`: `.github/workflows/security.yaml` calls ci-workflows; no other workflow lists `vuln` in its `recipes:`; and the Justfile's `check` recipe does not reach `vuln` | reaching is a `vuln` dependency of `check`, of any recipe `check` depends on (followed through this Justfile), or a body line running `just vuln`; each is reported as `Justfile:<line>`. Skips without `go.mod` |
| C11 | no image name ends in the repository name one level below it (`<registry>/<owner>/<repo>/<repo>`) | names are built from `ko-docker-repo`/`image-repo` in `.github/workflows/release.yaml` and `repositories:` in `.goreleaser.yaml`, joined to the base of each `main:`; plus every `ghcr.io/...` written out in those files and in `charts/*/values.yaml` |
| C12 | `README.md` never says `@latest` | |
| C13 | no estate fact written as a default, and no internal ticket key in any tracked file | see below |

### C13

Narrow on purpose: it names five shapes it can tell from neutral text, and
a reviewer still reads the rest. Every finding is printed on its own line
as `C13 <file>:<line>: <why>`; the rule line summarises.

| check | flags | reads |
| -- | -- | -- |
| `domain` | an organisation domain: the organisation's name followed by `.com`, `.xyz`, `.co` or `.private` | chart `values.yaml` and `values.schema.json` defaults, Go and TypeScript non-comment lines |
| `tenancy` | the organisation's tenancy API group | the same |
| `env` | `kernel`, `devel`, `stage` or `prod` as the default of a key or identifier named `env`, `environment`, `cluster`, `stage` or `tier` (`env: prod`, `flag.String("cluster", "kernel", ...)`) | the same; a `==`, `!=` or `case` line, and a list of names, are tests of a name and are not flagged |
| `region` | a real cloud region (`eu-west-1`) as any chart default, or as the default of an identifier named `region` | the same; `eu-example-1` is neutral |
| `ticket` | an internal ticket key (`INF-` and digits) | every tracked text file, comments and the CHANGELOG included: a key is an internal name and the public history keeps it |

A chart is a directory with a `Chart.yaml`. Not read: comments, files
under `tests/`, `test/`, `testdata/`, `fixtures/`, `golden/`,
`node_modules/` and `vendor/`, generated code, `*_test.go`,
`*.test.ts` and `*.spec.ts` (all but `ticket`).

An exemption for C13 may narrow it. Write one entry per pair of check and
path; an entry covers only its own `checks` on its own `paths`, so two
entries never cross over:

```yaml
exempt:
  C13:
    - checks: [region]                  # omit to cover every check
      paths: [internal/s3test/**]       # shell globs; omit to cover every path
      reason: the fixture names one region as a worked example   # required
    - checks: [domain]
      paths: [catalogue/schemaid.go]
      reason: a schema identifier is a published name
```

An entry with no `reason` fails the rule and exempts nothing. Quotes around
a `checks` or `paths` item (`paths: ["a/b"]`) are stripped.

The older single block, with `reason:`, `checks:` and `paths:` directly
under `C13:`, still works. Its checks and paths combine as a cross product
(`checks: [region, domain]` with two paths exempts both checks on both
paths), which is why the list form is preferred.

Findings the exemption covers are dropped and the line says how many.

## Tests

`hack/policy-conformance-cases.sh` builds a repository that meets every
rule, proves each says `PASS`, then breaks one rule at a time and proves
that rule, and only that rule, says `FAIL`. It also covers `strict`,
`skip` with and without a reason, the job summary, and a repository with
nothing in it. No network: `origin` is a local bare repository.
`self-check.yaml` runs it inside `check`, and runs the action itself
against this repository with `strict: false`.
