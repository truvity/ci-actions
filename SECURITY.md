# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/truvity/ci-actions/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

## What is in scope

This repository publishes:

- The composite actions: `setup-devbox`, `recipe`, `public-runners`, `tagged-pins`, `policy-conformance`, `cluster`, `setup-remote-builders`, `openbao-secrets`, `fleet-discover`, `caller-parity` and `devbox-parity`.
- The kits under `caller-parity/kits/` that repositories are compared against.
- Shell and script steps inside the actions, which run with the caller's job credentials.

Reports that matter most:

- An action that leaks a token or secret it is handed into a log, an output, a cache or an artifact.
- Injection: an input or event field interpolated into a shell step or `github-script` so that a fork or a crafted branch name runs code.
- A guard that passes what it should refuse: `public-runners`, `tagged-pins`, the fork guard, or a fleet job touching a repository it should not.
- An action that asks the caller to grant wider permissions than it needs.

A finding that depends on how a particular deployment uses this repository
belongs with that deployment's owner.
