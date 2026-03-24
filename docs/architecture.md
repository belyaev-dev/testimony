# Testimony Architecture

Testimony is a **single Go service** that accepts Allure result archives, stores raw and generated artifacts in S3-compatible object storage, records metadata in SQLite, generates reports asynchronously through the Allure CLI, and serves both a lightweight browse UI and the generated report assets.

This document describes the runtime that exists in the repository today. It is intentionally grounded in the current code and packaging surfaces rather than future ideas.

## Runtime topology

At startup, `cmd/testimony/main.go` composes the runtime in this order:

1. load environment-driven configuration from `internal/config`
2. build the JSON logger from `internal/server`
3. open the SQLite metadata store from `internal/db`
4. optionally seed and enable API-key auth from `internal/auth`
5. open the S3-compatible object store from `internal/storage`
6. build the report generator and asynchronous generation service from `internal/generate`
7. build the upload handler from `internal/upload`
8. build the browse UI and report proxy from `internal/serve`
9. start the retention worker from `internal/retention`
10. mount the router and start the HTTP server from `internal/server`

The result is one process with two persistent backends:

- **SQLite** for projects, reports, statuses, timestamps, and API-key hashes
- **S3-compatible storage** for uploaded archives and generated report files

## Request and data flow

### 1. Health and readiness

`internal/server/server.go` mounts:

- `GET /healthz` — liveness
- `GET /readyz` — readiness

Readiness is driven by a shared health state plus live dependency checks against SQLite and S3. When readiness changes, the runtime logs a structured `readiness changed` event. When a dependency probe fails, it logs `readiness probe failed` with the failing dependency name.

### 2. Upload intake

`POST /api/v1/projects/{slug}/upload` is mounted under `/api/v1` in `internal/server/server.go` and handled by `internal/upload/handler.go`.

The upload path:

1. validates the project slug from the route
2. accepts either:
   - `multipart/form-data` with a file part, or
   - a raw `application/zip` / `application/gzip` request body
3. stages the upload into the runtime temp directory
4. validates and extracts the archive with `PrepareArchive`
5. uploads the original archive to S3 under a report-scoped key
6. creates a `pending` report row in SQLite
7. moves the extracted results into a report-scoped generation workspace
8. enqueues asynchronous report generation
9. returns `202 Accepted` with JSON containing the `report_id`, `project_slug`, status, format, and archive object key

Current raw archive object keys follow this pattern:

- `projects/{slug}/reports/{reportID}/archive.zip`
- `projects/{slug}/reports/{reportID}/archive.tar.gz`

If metadata persistence fails after the archive upload, the handler attempts an object-storage rollback so the runtime does not leave an orphaned archive behind.

### 3. Authentication boundaries

`internal/auth/middleware.go` provides bearer-token middleware.

Current behavior:

- uploads are wrapped with auth middleware when `TESTIMONY_AUTH_ENABLED=true`
- browse UI and report viewing are also wrapped when `TESTIMONY_AUTH_REQUIRE_VIEWER=true`
- auth failures return `401 Unauthorized` and log structured `auth rejected` events without printing secret values

Bootstrap API keys are seeded through the configured runtime and stored as hashes in SQLite.

### 4. Asynchronous report generation

After upload acceptance, `internal/generate/service.go` runs report generation in the background.

The generation service:

1. marks the report as `processing`
2. loads ready reports for the same project from SQLite
3. asks the generator to fetch usable history from prior report output in S3
4. executes the selected Allure CLI variant
5. uploads the generated HTML tree back to S3
6. marks the report `ready` or `failed`

The service uses a semaphore sized by `TESTIMONY_GENERATE_MAX_CONCURRENCY`, so uploads can return quickly while generation stays bounded.

Current status lifecycle in SQLite:

- `pending`
- `processing`
- `ready`
- `failed`

When generation succeeds, the primary generated object key is recorded as:

- `projects/{slug}/reports/{reportID}/html/index.html`

The rest of the generated report is uploaded under the same `html/` prefix, preserving Allure asset paths.

When generation fails, the service logs `report_generation_failed` and stores a truncated error message on the report row so later inspection is possible without scraping logs alone.

### 5. Browsing and report serving

`internal/serve/ui.go` mounts the lightweight server-rendered browse UI:

- `GET /` — list projects
- `GET /projects/{slug}` — list reports for one project

The UI reads from SQLite and only links to report pages when a report is `ready`.

`internal/serve/proxy.go` mounts the report-serving surface:

- `GET /reports/{slug}/{reportID}` — redirect to the trailing-slash root
- `GET /reports/{slug}/{reportID}/`
- `GET /reports/{slug}/{reportID}/*`

The proxy resolves the report in SQLite, verifies that it belongs to the requested project and is ready, then streams the requested asset directly from S3 back to the client. This keeps generated reports in object storage while the Go service remains the stable HTTP entrypoint.

### 6. Retention cleanup

`internal/retention/worker.go` starts as part of normal runtime startup.

On each pass it:

1. asks SQLite for expired reports using the global retention default plus any per-project overrides
2. lists generated objects under the report's `html/` prefix
3. deletes S3 objects first
4. deletes the SQLite row last

This ordering prevents metadata from disappearing before storage cleanup succeeds. Cleanup outcomes are logged with structured success/failure events so operators can see whether a report was deleted or retained for retry.

## Package responsibilities

| Package | Responsibility |
|---|---|
| `cmd/testimony` | Compose the runtime, start HTTP serving, manage readiness transitions, and perform graceful shutdown. |
| `internal/config` | Load and validate the `TESTIMONY_*` environment contract. |
| `internal/server` | Build the JSON logger, request logging middleware, health/readiness surfaces, and HTTP server. |
| `internal/upload` | Accept uploads, validate/extract archives, persist raw archives, create report rows, and dispatch generation. |
| `internal/storage` | Talk to S3-compatible backends and ensure the configured bucket exists. |
| `internal/generate` | Merge history, invoke the Allure CLI, upload generated output, and persist report status transitions. |
| `internal/serve` | Render the project/report browse pages and proxy generated report assets from S3. |
| `internal/auth` | Validate bearer tokens against stored API-key hashes. |
| `internal/retention` | Delete expired report artifacts and metadata on a background schedule. |
| `internal/db` | Persist projects, reports, statuses, timestamps, and API-key hashes in SQLite. |

## Configuration contract

The runtime is configured through environment variables loaded in `internal/config/config.go`.

Important characteristics of the current contract:

- the HTTP server binds with `TESTIMONY_SERVER_*`
- S3 access is controlled with `TESTIMONY_S3_*`
- SQLite uses `TESTIMONY_SQLITE_*`
- auth uses `TESTIMONY_AUTH_*`
- report generation uses `TESTIMONY_GENERATE_*`
- cleanup and shutdown use `TESTIMONY_RETENTION_*` and `TESTIMONY_SHUTDOWN_*`

Both Docker Compose and Helm package the same binary by translating their settings into this existing env contract rather than inventing a second configuration layer.

## Deployment surfaces

### Docker Compose

`docker-compose.yml` packages the real runtime for local development:

- builds the image from `Dockerfile`
- starts MinIO alongside Testimony
- maps Testimony `8080` to host `18080`
- maps MinIO API/console to `19000` and `19001`
- mounts persistent data under `/var/lib/testimony`
- wires Testimony to MinIO through `TESTIMONY_S3_*` variables
- probes `http://127.0.0.1:8080/readyz` inside the Testimony container

This is the easiest way to exercise the same upload, generation, browse, and proxy flow that contributors and new users see first.

### Helm chart

`chart/` packages the same runtime for Kubernetes:

- `chart/values.yaml` exposes runtime settings under a structured values surface
- the Deployment template maps those values back onto `TESTIMONY_*` environment variables
- credentials come from a generated Secret by default or an existing Secret when configured
- the container still serves the same health and readiness paths: `/healthz` and `/readyz`
- persistence mounts the same runtime path used by the container image

In other words, Helm is a packaging layer over the existing runtime contract, not a separate implementation.

## Shutdown behavior

`cmd/testimony/main.go` handles `SIGINT` and `SIGTERM`.

On shutdown the process:

1. marks readiness false
2. logs `shutdown requested` and `shutdown drain started`
3. waits for the configured drain delay
4. shuts down the HTTP server with a timeout
5. stops the retention worker
6. logs `shutdown complete` when the server exits cleanly

This behavior is important for Kubernetes rollouts because `/readyz` stops advertising readiness before in-flight work is drained.

## Observability and diagnostics

Useful built-in inspection surfaces:

- **HTTP status surfaces** — `GET /healthz` and `GET /readyz`
- **Request logs** — `http request completed` includes request ID, method, path, status, and duration
- **Startup logs** — `sqlite store ready`, `s3 store ready`, `http server listening`
- **Upload/generation logs** — `upload accepted`, `report_generation_started`, `report_generation_completed`, `report_generation_failed`
- **Retention logs** — `retention worker started`, `retention cleanup succeeded`, `retention cleanup failed`
- **Manual verification scripts** — `scripts/verify-compose-e2e.sh` and `scripts/verify-helm-chart.sh`

For local packaging regressions, prefer the scripts in `scripts/` because they surface failure context automatically:

- the Compose smoke script prints `docker compose ps` and both service logs on failure
- the Helm verification script fails on specific rendered-manifest assertions after `helm lint` / `helm template`

## What is intentionally not here

The current repository does **not** implement a separate worker service, a message queue, or an admin API for dynamic API-key management. The runtime today is one Go process with background goroutines and packaging for local Docker Compose and Kubernetes deployment.
