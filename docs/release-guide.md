# Release guide

This repository now ships a tag-driven GitHub Actions release workflow in [`.github/workflows/release.yml`](../.github/workflows/release.yml).

The release path is automated, but the release contract is still strict: a tag is not considered valid unless the repository verification gates pass on that exact ref.

## Current release policy

- CI is required on pull requests and on `main`.
- Releases are cut from annotated semantic-version tags.
- The `Release` workflow re-runs the repository verification gates before publishing anything.
- A release publishes both GitHub Release assets and GHCR container images.

## What the release workflow publishes

When you push a tag like `v1.2.3`, `.github/workflows/release.yml` will:

1. run:
   - `go test ./... -p 1`
   - `bash scripts/verify-docs-and-ci.sh`
   - `bash scripts/verify-helm-chart.sh`
   - `bash scripts/verify-compose-e2e.sh`
2. build release archives for:
   - `linux/amd64`
   - `linux/arm64`
   - `darwin/amd64`
   - `darwin/arm64`
   - `windows/amd64`
3. generate `SHA256SUMS`
4. publish two multi-arch GHCR images (`linux/amd64`, `linux/arm64`):
   - default runtime image from `Dockerfile`
   - alternate runtime image from `Dockerfile.allure3`
5. create or update the matching GitHub Release and attach the built assets

## Versioning and tagging

Use annotated semantic-version tags:

- `v0.1.0`
- `v0.2.0`
- `v1.0.0`

Guidance:

- bump **patch** for fixes and non-breaking packaging/doc corrections
- bump **minor** for backward-compatible features
- bump **major** for breaking API, config, or deployment changes

Do not use lightweight tags for releases.

## GHCR tag policy

For a release tag `vX.Y.Z`, the workflow publishes:

### Default runtime image

- `ghcr.io/belyaev-dev/testimony:vX.Y.Z`
- `ghcr.io/belyaev-dev/testimony:vX.Y`
- `ghcr.io/belyaev-dev/testimony:vX`
- `ghcr.io/belyaev-dev/testimony:latest`

### Allure 3 runtime image

- `ghcr.io/belyaev-dev/testimony:vX.Y.Z-allure3`
- `ghcr.io/belyaev-dev/testimony:vX.Y-allure3`
- `ghcr.io/belyaev-dev/testimony:vX-allure3`
- `ghcr.io/belyaev-dev/testimony:latest-allure3`

The unsuffixed image is the default runtime. The `-allure3` suffix marks the alternate runtime explicitly.

## Standard release flow

### 1. Confirm `main` is the exact release commit

```bash
git checkout main
git pull --ff-only origin main
```

### 2. Run the release verification suite locally first

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

That push triggers the GitHub `Release` workflow.

## Manual re-run flow

The workflow also supports manual dispatch against an existing tag. Use this only when you need to recreate release assets or recover a failed publication without inventing a new version.

## Release asset expectations

Each release should end up with:

- platform archives for the supported OS/architecture targets
- `SHA256SUMS`
- a GitHub Release named after the tag
- GHCR images for the default and `-allure3` variants

## Troubleshooting

If a release fails:

1. inspect the failed `Release` workflow run in GitHub Actions
2. confirm the tag points to the intended commit
3. rerun the verification commands locally
4. use manual workflow dispatch against the same tag only after the underlying issue is fixed

Do not move or retag an existing released version to hide a bad build. Cut a new version instead.
