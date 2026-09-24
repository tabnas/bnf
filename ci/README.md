# ci/

Staging area for GitHub Actions workflow changes.

This directory exists because session credentials cannot write
`.github/workflows/*` — see admin `DECISIONS.md` ADR-8. To change CI:

1. Put the intended workflow file in `workflows/`.
2. A maintainer promotes it with the admin `rollout/apply-ci-folders.sh`
   script.

## Pending

- **`workflows/rust.yml`** — the Rust gate: `ci/rust/run.sh` on the MSRV
  toolchain, with the engine cloned as a sibling checkout the way the Go
  CI already resolves `github.com/tabnas/parser/go` from `main`. It runs
  `cargo fmt --check`, a build, the tests, the doctests (which include
  the README's examples), clippy with warnings denied, and a lockfile
  check that exempts only the engine's recorded version. Standalone
  rather than an arm of `ci.yml`, because the org-shared polyglot
  workflow takes no Rust input, so promoting it needs no change in
  `tabnas/.github`.
