# ci/

The scripts the GitHub Actions workflows run, kept here so that you can
run the same gate locally.

- `rust/run.sh` is the Rust gate. `.github/workflows/rust.yml` runs it,
  and so can you.

The workflows themselves live in `.github/workflows/`. To change CI, edit
them there in a reviewed pull request: session credentials can push
workflow changes (admin `DECISIONS.md` ADR-8, as amended on 2026-09-24),
so staging a workflow here for a maintainer to promote is optional.
Sessions still cannot push tags. Releases therefore go through
`workflow_dispatch`, and a workflow that runs only on a tag push needs a
maintainer to push that tag.

Some of these workflows are maintained in admin as well, and an edit made
only here does not last:

- A workflow with a template in admin `rollout/workflows/`, named
  `bnf__<file>`, changes in that template too, in a pull request to admin.
  Today that is `ci.yml`, `release.yml` and `crates-release.yml`. Admin
  `scripts/verify.sh` reports a deployed copy that differs from its
  template, and the next `rollout/apply-workflows.sh --apply` writes the
  template back over it.
- `clib.yml` and `clib-release.yml` are stamped from admin
  `tasks/clib-template/`, together with `go/clib/`. Change the template
  and restamp with admin `tasks/adopt-clib.sh`, which writes the two
  workflows to `ci/`, then move them over the copies in
  `.github/workflows/` in the same pull request. Admin `scripts/verify.sh`
  reports a `ci/*.yml` left behind as a promotion still owed.

`docs.yml` and `rust.yml`, the last workflows staged here, were promoted
to `.github/workflows/` on 2026-09-22.
