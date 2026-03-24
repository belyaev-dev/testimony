# Contributing to Testimony

Thanks for helping improve Testimony.

This repository ships a single Go runtime that accepts Allure result archives, stores artifacts in S3-compatible storage, generates reports with the Allure CLI, and serves both a lightweight browse UI and the generated report assets. The easiest way to understand a change is to verify it against the same runtime surface that ships in Docker Compose and Helm.

## Prerequisites

Install these tools before you start:

- **Go 1.24.x** — matches `go.mod`
- **Docker Engine + Docker Compose plugin** — required for the local stack and for integration tests that use Testcontainers + MinIO
- **Helm 3** — required for chart verification
- **Bash**, **curl**, and **python3** — used by the verification scripts in `scripts/`

You do **not** need a manually installed Allure CLI just to run the existing test suite. The Go unit tests use fixtures, and the Compose workflow uses the packaged container image.

## Repository layout

- `cmd/testimony/main.go` — process entrypoint and runtime wiring
- `internal/upload` — archive intake, validation, staging, and upload acceptance
- `internal/storage` — S3-compatible object storage integration
- `internal/generate` — asynchronous Allure report generation and history merging
- `internal/serve` — browse UI and report asset proxying
- `internal/auth` — bearer-token upload/viewer auth middleware
- `internal/retention` — background cleanup worker
- `chart/` — Helm chart for Kubernetes packaging
- `docker-compose.yml` — local zero-config stack (Testimony + MinIO)
- `scripts/` — smoke and packaging verification helpers used by contributors

## Recommended development flow

### 1. Run the Go test suite

```bash
go test ./... -p 1
```

Notes:

- The repository includes both package tests and integration tests under `internal/integration`.
- The integration coverage uses Testcontainers and MinIO, so Docker must be available locally.
- The serial package pass (`-p 1`) is the most deterministic baseline on resource-constrained machines because some generator fixture tests use 1-second subprocess deadlines.
- This is the baseline check to run before and after touching Go code, route wiring, storage behavior, auth, or retention logic.

### 2. Verify the Docker Compose smoke flow

```bash
bash scripts/verify-compose-e2e.sh
```

This script exercises the user-facing local story:

1. builds the image from `Dockerfile`
2. starts `docker-compose.yml`
3. waits for `GET /readyz`
4. uploads a synthesized Allure archive to `POST /api/v1/projects/{slug}/upload`
5. confirms the browse UI and report proxy respond as expected

If the flow fails, the script prints diagnostics instead of failing silently, including:

- `docker compose ps`
- Testimony logs
- MinIO logs

Use it whenever you change upload handling, generation, serving, container packaging, or the local stack.

### 3. Verify the Helm chart

```bash
bash scripts/verify-helm-chart.sh
```

This script runs `helm lint` and `helm template` against the chart, then asserts that the rendered manifests still wire the runtime through the existing `TESTIMONY_*` environment-variable contract.

Use it for changes in:

- `chart/`
- environment-variable names or defaults in `internal/config/config.go`
- health/readiness paths
- secret wiring for S3 credentials or auth keys

### 4. Optionally inspect the stack manually

If you want a long-lived local stack for manual checks:

```bash
docker compose up --build
```

Useful endpoints with the default local stack:

- `http://127.0.0.1:18080/healthz`
- `http://127.0.0.1:18080/readyz`
- `http://127.0.0.1:18080/`
- `http://127.0.0.1:19001/` (MinIO console)

Stop the stack with `docker compose down -v` when you are done.

## Runtime and observability conventions

When you change runtime behavior, keep these conventions intact:

- **Keep the env-first contract stable.** Runtime configuration lives in `internal/config/config.go` and is expressed through `TESTIMONY_*` variables so Docker Compose and Helm package the same binary.
- **Preserve health surfaces.** `GET /healthz` and `GET /readyz` are used by local smoke checks and Kubernetes probes.
- **Keep logs structured.** The server emits JSON logs through `log/slog`. Existing events such as `http server listening`, `readiness changed`, `http request completed`, `upload accepted`, `report_generation_started`, `report_generation_completed`, `report_generation_failed`, and retention cleanup signals should stay inspectable.
- **Do not leak secrets.** Never commit real API keys, S3 credentials, or private endpoints. Examples and docs should use placeholders, env var names, or secret-backed Helm values only.
- **Keep packaging layers aligned.** If a runtime env var, port, health path, or secret name changes, update the Go code, `docker-compose.yml`, Helm chart wiring, docs, and verification scripts together.

## Code and test conventions

- Prefer small, focused changes that follow the existing package boundaries.
- Keep tests next to the code they validate (`*_test.go` in the same package or in `internal/integration`).
- Use table-driven tests where it improves readability.
- Run `gofmt` on touched Go files before submitting a PR.
- If you add new user-facing routes, config, or scripts, document them in the repository docs in the same change.

## Pull request expectations

A good PR for this repository should include:

- a clear summary of what changed and why
- the verification commands you ran (`go test ./... -p 1`, `bash scripts/verify-compose-e2e.sh`, `bash scripts/verify-helm-chart.sh`, or a relevant subset)
- doc updates when you change any external behavior, endpoint, env var, or deployment wiring
- screenshots or sample responses only when they help explain a user-visible UI/API change

Before opening a PR, check the following:

- [ ] the change is scoped to a single improvement or bug fix
- [ ] Go code is formatted
- [ ] relevant tests and scripts were run locally
- [ ] no real secrets or machine-specific values were committed
- [ ] docs still match the actual runtime behavior

## Need architecture context first?

Start with `docs/architecture.md`. It explains how `cmd/testimony/main.go`, upload/storage/generation/serving/auth/retention, Docker Compose, and the Helm chart fit together.
