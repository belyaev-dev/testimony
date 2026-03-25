# GitHub repository baseline

This repository ships CI upload examples for *other* repositories under [`examples/ci/`](../examples/ci/), but Testimony also needs its own GitHub-side automation.

## Do we need GitHub CI in this repository?

Yes.

A repository that publishes a Go binary, Docker images, and a Helm chart needs repository-local GitHub automation even if the product itself is meant to be called *from* other CI systems. Without it, contributors can easily break one of the packaged paths without noticing until after merge.

The useful baseline for this repository is:

1. **Pull request + main-branch CI** for formatting, `go test ./... -p 1`, docs/CI example verification, and Helm chart verification.
2. **A slower Docker Compose smoke workflow** for the packaged end-to-end path that starts the stack, uploads a real archive, and proves the report becomes browseable.
3. **A tag-based release workflow** that re-runs the release gates, builds release archives plus checksums, publishes GHCR images, and creates or updates the GitHub Release.

That split keeps contributor feedback fast while still protecting the shipped paths.

## Workflows tracked in this repository

- [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) — fast branch protection workflow for pull requests and pushes to `main`
- [`.github/workflows/compose-smoke.yml`](../.github/workflows/compose-smoke.yml) — slower packaged-stack smoke check for `main` and manual runs
- [`.github/workflows/release.yml`](../.github/workflows/release.yml) — tag-based release workflow for `v*` tags and manual re-runs against an existing tag

These workflows intentionally call the existing repository-owned verification commands instead of duplicating logic inline:

- `go test ./... -p 1`
- `bash scripts/verify-docs-and-ci.sh`
- `bash scripts/verify-helm-chart.sh`
- `bash scripts/verify-compose-e2e.sh`

## Release workflow scope

The release workflow currently does four things:

1. re-runs the repository verification gates on the tagged ref
2. builds release archives for the supported target platforms and emits `SHA256SUMS`
3. publishes both runtime images to GHCR as multi-arch manifests for `linux/amd64` and `linux/arm64`
4. creates or updates the matching GitHub Release with the release assets attached

### GHCR tag policy

Default runtime image (`Dockerfile`, Allure 2 runtime):

- `vX.Y.Z`
- `vX.Y`
- `vX`
- `latest`

Alternate runtime image (`Dockerfile.allure3`, Allure 3 runtime):

- `vX.Y.Z-allure3`
- `vX.Y-allure3`
- `vX-allure3`
- `latest-allure3`

The unsuffixed image remains the default runtime. The `-allure3` suffix marks the alternate generator runtime explicitly.

The operational release path is documented in [docs/release-guide.md](release-guide.md).

## Suggested GitHub repository metadata

Because GitHub repository metadata is configured in GitHub itself rather than committed as source, keep these values as the current baseline:

- **Description:** `Single-binary service for publishing Allure reports without turning your CI runner into a report host.`
- **Website:** leave blank until there is a stable public docs or product URL; once available, use that URL instead of pointing the field back at the repository itself.
- **Topics:** `allure`, `allure-report`, `ci`, `test-reporting`, `go`, `s3`, `sqlite`, `helm`, `docker`, `kubernetes`

## Why this split is the right fit right now

- **CI on PRs and main:** protects code, docs, examples, and packaging on every contributor-facing change.
- **Compose smoke separately:** protects the expensive packaged path without making every pull request slower than necessary.
- **Releases on tags:** keeps publication explicit and observable, while still enforcing the same verification gates before release assets and container images are published.
- **Variant tags instead of separate repos:** keeps the default image simple while making the Allure 3 runtime opt-in and explicit.
