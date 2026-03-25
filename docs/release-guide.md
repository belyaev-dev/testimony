# Release guide

This repository does **not** have tag-driven release automation yet.

Until a dedicated release workflow exists, releases should stay explicit and manual so the tag, notes, and distribution story do not drift away from the repository reality.

## Current release policy

- CI is required on pull requests and on `main`.
- Release automation is intentionally deferred.
- A release should only be cut from `main` after the verification surface below passes.
- GitHub Releases, tags, and any future container publication should follow one documented version for the repository.

## What counts as release-ready

Before creating a version tag, run the repository-owned verification commands from a clean checkout:

```bash
go test ./... -p 1
bash scripts/verify-docs-and-ci.sh
bash scripts/verify-helm-chart.sh
bash scripts/verify-compose-e2e.sh
```

A release is not ready if any of these fail.

## Versioning and tagging

Until the project adopts a different rule, use semantic version tags:

- `v0.1.0`
- `v0.2.0`
- `v1.0.0`

Guidance:

- bump **patch** for fixes and non-breaking packaging/doc corrections
- bump **minor** for backward-compatible features
- bump **major** for breaking API, config, or deployment changes

Create annotated tags, not lightweight tags.

## Manual release flow

### 1. Confirm `main` is the exact release commit

Make sure the release commit is already merged to `main` and that local `main` matches the remote.

```bash
git checkout main
git pull --ff-only origin main
```

### 2. Run the release verification suite

```bash
go test ./... -p 1
bash scripts/verify-docs-and-ci.sh
bash scripts/verify-helm-chart.sh
bash scripts/verify-compose-e2e.sh
```

### 3. Create the annotated tag

Replace `vX.Y.Z` with the version you are releasing.

```bash
git tag -a vX.Y.Z -m "vX.Y.Z"
```

### 4. Push the tag

```bash
git push origin vX.Y.Z
```

### 5. Create the GitHub Release entry

Create a GitHub Release from the pushed tag and include:

- a short summary of what changed
- notable operator-facing changes
- breaking changes or migration notes, if any
- verification commands used for the release

Until automation exists, this step is manual.

## Release notes template

Use this structure for the GitHub Release body:

```markdown
## Summary
- <one-line release summary>

## What changed
- <change 1>
- <change 2>

## Operator impact
- <deployment/config/runtime note>

## Verification
- `go test ./... -p 1`
- `bash scripts/verify-docs-and-ci.sh`
- `bash scripts/verify-helm-chart.sh`
- `bash scripts/verify-compose-e2e.sh`

## Breaking changes
- None.
```

## Future automation target

When repository metadata, versioning expectations, and distribution targets are stable, replace the manual path above with a tag-triggered release workflow that:

1. re-runs the release verification gates
2. builds the release binary
3. publishes the container image to GHCR, if that remains the chosen distribution path
4. creates the GitHub Release with generated notes plus checksums/assets

Until then, keep the release path simple, observable, and manual.
