# Testimony configuration reference

Testimony uses a single environment-variable contract defined in `internal/config/config.go`.

That contract is the source of truth for all deployment surfaces in this repository:

- `docker-compose.yml` overrides a small, local-friendly subset for the first-run stack on port `18080`
- `chart/values.yaml` exposes deployment-facing values that the Helm templates render back into the same `TESTIMONY_*` variables
- `chart/templates/secret.yaml` handles the secret-backed values for S3 credentials and the optional auth API key

This document lists the real variables, defaults, and where they surface in the packaged Compose and Helm flows.

## How configuration works

### Parsing and validation rules

- **Strings** are trimmed before validation.
- **Integers** use Go's `strconv.Atoi` parsing.
- **Booleans** use Go's `strconv.ParseBool` parsing.
- **Durations** use Go's `time.ParseDuration` syntax such as `15s`, `2m`, or `1h`.
- Invalid values fail startup instead of silently falling back.

### Packaging model

- **Docker Compose** is the zero-config local story. It sets the local bind port, SQLite path, temp dir, MinIO endpoint, and local demo credentials directly in `docker-compose.yml`.
- **Helm** keeps the runtime env-first. `chart/templates/deployment.yaml` maps values under `runtime.*` back onto `TESTIMONY_*`, while `chart/templates/secret.yaml` or `secrets.existingSecretName` supplies the secret-backed variables.
- **Unspecified variables** keep the defaults from `internal/config/config.go`.

### Security note

The Compose file includes local demo credentials for the bundled MinIO service. Those values are only for the packaged local stack. For shared environments, keep S3 credentials and optional auth keys in secret-backed Helm values or operator-managed secrets, not in committed plain-text files.

## Environment variable reference

### Logging and HTTP server

| Variable | Default in code | What it controls | Docker Compose | Helm |
|---|---|---|---|---|
| `TESTIMONY_LOG_LEVEL` | `INFO` | Structured log verbosity. Must be a valid `log/slog` level. | Set to `debug` for the local stack. | `runtime.logLevel` |
| `TESTIMONY_SERVER_HOST` | `0.0.0.0` | HTTP bind host. Must be non-empty. | Set to `0.0.0.0`. | `runtime.server.host` |
| `TESTIMONY_SERVER_PORT` | `8080` | HTTP listen port. Must be between `1` and `65535`. | Set to `8080`, then published as host port `18080`. | `runtime.server.port` |
| `TESTIMONY_SERVER_READ_TIMEOUT` | `15s` | Maximum request read time. Must be greater than zero. | Uses code default. | `runtime.server.readTimeout` |
| `TESTIMONY_SERVER_WRITE_TIMEOUT` | `30s` | Maximum response write time. Must be greater than zero. | Uses code default. | `runtime.server.writeTimeout` |
| `TESTIMONY_SERVER_IDLE_TIMEOUT` | `60s` | HTTP keep-alive idle timeout. Must be greater than zero. | Uses code default. | `runtime.server.idleTimeout` |

### S3-compatible object storage

| Variable | Default in code | What it controls | Docker Compose | Helm |
|---|---|---|---|---|
| `TESTIMONY_S3_ENDPOINT` | `http://127.0.0.1:9000` | S3-compatible API base URL. Must be non-empty. | Overridden to `http://minio:9000` to reach the bundled MinIO container. | `runtime.s3.endpoint` |
| `TESTIMONY_S3_REGION` | `us-east-1` | Object-store region string. Must be non-empty. | Set to `us-east-1`. | `runtime.s3.region` |
| `TESTIMONY_S3_BUCKET` | `testimony` | Bucket used for raw archives and generated HTML. Must be non-empty. | Set to `testimony`. | `runtime.s3.bucket` |
| `TESTIMONY_S3_ACCESS_KEY_ID` | `minioadmin` | Access key used to talk to S3-compatible storage. Must be non-empty. | Set to `minioadmin` for the local demo stack. | `secrets.s3AccessKeyId` → Secret → `TESTIMONY_S3_ACCESS_KEY_ID` |
| `TESTIMONY_S3_SECRET_ACCESS_KEY` | `minioadmin` | Secret key used to talk to S3-compatible storage. Must be non-empty. | Set to `minioadmin` for the local demo stack. | `secrets.s3SecretAccessKey` → Secret → `TESTIMONY_S3_SECRET_ACCESS_KEY` |
| `TESTIMONY_S3_USE_PATH_STYLE` | `true` | Whether the S3 client uses path-style requests. | Set to `true` for MinIO compatibility. | `runtime.s3.usePathStyle` |

### SQLite metadata store

| Variable | Default in code | What it controls | Docker Compose | Helm |
|---|---|---|---|---|
| `TESTIMONY_SQLITE_PATH` | `./data/testimony.sqlite` | SQLite database path for projects, reports, and API-key hashes. Must be non-empty. | Overridden to `/var/lib/testimony/data/testimony.sqlite`. | `runtime.sqlite.path` |
| `TESTIMONY_SQLITE_BUSY_TIMEOUT` | `5s` | SQLite busy timeout. Must be greater than zero. | Uses code default. | `runtime.sqlite.busyTimeout` |

### Authentication

| Variable | Default in code | What it controls | Docker Compose | Helm |
|---|---|---|---|---|
| `TESTIMONY_AUTH_ENABLED` | `false` | Enables bearer-token auth for uploads. | Unset, so auth stays disabled in the local quickstart. | `runtime.auth.enabled` |
| `TESTIMONY_AUTH_API_KEY` | empty string | Bootstrap API key value. Required when auth is enabled. | Unset in the local quickstart. | `secrets.authAPIKey` → Secret → optional `TESTIMONY_AUTH_API_KEY` |
| `TESTIMONY_AUTH_REQUIRE_VIEWER` | `false` | Extends auth to the browse UI and report views. Requires `TESTIMONY_AUTH_ENABLED=true`. | Unset, so viewers remain open locally. | `runtime.auth.requireViewer` |

### Report generation

| Variable | Default in code | What it controls | Docker Compose | Helm |
|---|---|---|---|---|
| `TESTIMONY_GENERATE_VARIANT` | `allure2` | Which Allure distribution the runtime expects. Valid values: `allure2`, `allure3`. | Set to `allure2`. | `runtime.generate.variant` |
| `TESTIMONY_GENERATE_CLI_PATH` | `allure` | Path to the Allure executable. Must be non-empty. | Overridden to `/usr/local/bin/allure` inside the packaged image. | `runtime.generate.cliPath` |
| `TESTIMONY_GENERATE_TIMEOUT` | `2m` | Per-report generation timeout. Must be greater than zero. | Set to `2m`. | `runtime.generate.timeout` |
| `TESTIMONY_GENERATE_MAX_CONCURRENCY` | `2` | Max concurrent report generations. Must be greater than zero. | Set to `1` to keep the local stack predictable on smaller machines. | `runtime.generate.maxConcurrency` |
| `TESTIMONY_GENERATE_HISTORY_DEPTH` | `5` | How many prior ready reports to inspect for reusable history. Must be zero or greater. | Uses code default. | `runtime.generate.historyDepth` |

### Retention and cleanup

| Variable | Default in code | What it controls | Docker Compose | Helm |
|---|---|---|---|---|
| `TESTIMONY_RETENTION_DAYS` | `0` | Global fallback retention window in days. `0` means no global expiry by default. | Uses code default. | `runtime.retention.days` |
| `TESTIMONY_RETENTION_CLEANUP_INTERVAL` | `1h` | How often the retention worker looks for expired reports. Must be greater than zero. | Uses code default. | `runtime.retention.cleanupInterval` |

### Runtime temp directory and shutdown

| Variable | Default in code | What it controls | Docker Compose | Helm |
|---|---|---|---|---|
| `TESTIMONY_TEMP_DIR` | system temp dir joined with `testimony` | Working directory for staged uploads and report generation workspaces. Must not resolve to the current directory. | Overridden to `/var/lib/testimony/tmp`. | `runtime.tempDir` |
| `TESTIMONY_SHUTDOWN_DRAIN_DELAY` | `5s` | How long `/readyz` stays false before HTTP shutdown continues. Must be zero or greater. | Uses code default. | `runtime.shutdown.readyzDrainDelay` |
| `TESTIMONY_SHUTDOWN_TIMEOUT` | `30s` | Maximum graceful shutdown duration. Must be greater than zero. | Uses code default. | `runtime.shutdown.timeout` |

## Deployment-specific notes

### Docker Compose quickstart

`docker-compose.yml` is intentionally opinionated for the local story:

- Testimony is published on host port `18080`
- MinIO is published on host ports `19000` and `19001`
- the runtime uses `/var/lib/testimony` for persistent data and `/var/lib/testimony/tmp` for temp work
- auth is left disabled for the first-run path
- the local stack talks to MinIO over the container-network endpoint `http://minio:9000`

For automated end-to-end proof of that packaging, run `bash scripts/verify-compose-e2e.sh`. On failure it prints `docker compose ps` and compose logs for Testimony and MinIO.

### Helm chart

The chart keeps one runtime contract while giving operators a deployment-friendly values file:

- `runtime.*` mirrors the non-secret `TESTIMONY_*` knobs
- `secrets.*` supplies S3 credentials and the optional auth API key
- `secrets.existingSecretName` lets you reuse an operator-managed Secret instead of rendering credentials from values
- `probes.liveness.path` and `probes.readiness.path` default to `/healthz` and `/readyz`
- `persistence.*` controls the mount that backs SQLite and temp working data

For packaging verification, run `bash scripts/verify-helm-chart.sh`. That script runs `helm lint` and `helm template`, then asserts the rendered manifests still expose the expected env vars, secret refs, probes, and persistence wiring.

## Operational diagnostics tied to configuration

Configuration problems are visible through the same runtime surfaces the rest of the repository uses:

- startup validation errors fail fast instead of silently ignoring bad values
- `GET /healthz` confirms the HTTP server is alive
- `GET /readyz` reflects dependency readiness for SQLite and S3
- structured logs expose events such as `sqlite startup failed`, `s3 startup failed`, `bootstrap api key ready`, `http server listening`, `readiness changed`, and `shutdown drain started`

When you change a runtime variable name, default, secret source, or health path, update the Go config, `docker-compose.yml`, Helm values/templates, and repository docs together.
