# GitHub repository baseline

This repository ships CI upload examples for *other* repositories under [`examples/ci/`](../examples/ci/), but Testimony still needs its own GitHub-side automation.

## Do we need GitHub CI in this repository?

Yes.

A repository that publishes a Go binary, a Docker image, and a Helm chart needs repository-local CI even if the product itself is meant to be called *from* other CI systems. Without it, contributors can easily break one of the packaged paths without noticing until after merge.

The minimum useful baseline for this repository is:

1. **Pull request + main-branch CI** for formatting, `go test ./... -p 1`, docs/CI example verification, and Helm chart verification.
2. **A slower Docker Compose smoke workflow** for the packaged end-to-end path that starts the stack, uploads a real archive, and proves the report becomes browseable.
3. **Release automation later, not first**. Add GitHub Releases and publishing only after tag/version policy, image naming, and changelog expectations are settled.

That baseline keeps contributor feedback fast while still protecting the real shipped paths.

## Workflows tracked in this repository

- [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) — fast branch protection workflow for pull requests and pushes to `main`
- [`.github/workflows/compose-smoke.yml`](../.github/workflows/compose-smoke.yml) — slower packaged-stack smoke check for `main` and manual runs

These workflows intentionally call the existing repository-owned verification commands instead of duplicating logic inline:

- `go test ./... -p 1`
- `bash scripts/verify-docs-and-ci.sh`
- `bash scripts/verify-helm-chart.sh`
- `bash scripts/verify-compose-e2e.sh`

## Release recommendation

Do **not** add a full release workflow yet.

This repository is still missing the surrounding GitHub repository hygiene that should exist before automating tags and releases:

- final repository description
- public website/docs URL, if one exists
- topic taxonomy
- explicit versioning/tagging policy
- a decision on whether releases publish only a Go binary, only a GHCR image, or both

Once those are stable, the next GitHub automation step should be a tag-based release workflow that:

1. builds the tested binary
2. publishes the container image to GHCR
3. attaches release notes and checksums to a GitHub Release

Until then, CI is mandatory; release automation is intentionally deferred. The current manual release path is documented in [docs/release-guide.md](release-guide.md).

## Suggested GitHub repository metadata

Because GitHub repository metadata is configured in GitHub itself rather than committed as source, keep these values as the proposed baseline:

- **Description:** `Single-binary service for publishing Allure reports without turning your CI runner into a report host.`
- **Website:** leave blank until there is a stable public docs or product URL; once available, use that URL instead of pointing the field back at the repository itself.
- **Topics:** `allure`, `allure-report`, `ci`, `test-reporting`, `go`, `s3`, `sqlite`, `helm`, `docker`, `kubernetes`

## Why this split is the right fit right now

- **CI now:** protects code, docs, examples, and packaging on every contributor-facing change.
- **Smoke on main/manual:** protects the expensive packaged path without making every pull request slower than necessary.
- **Releases later:** avoids locking the repository into a versioning and distribution story before the public GitHub surface is finished.
