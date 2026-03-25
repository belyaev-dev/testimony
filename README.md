# Testimony

Testimony is a single-binary service for publishing Allure reports without turning your CI system into a report host.

It accepts a zipped Allure results archive over HTTP, stores raw and generated artifacts in S3-compatible storage, records report metadata in SQLite, generates the final report asynchronously with the Allure CLI, and serves a lightweight browse UI plus the generated report assets from one stable base URL.

## Features

- **CI-friendly upload API** — send a `.zip` or `.tar.gz` archive to `POST /api/v1/projects/{slug}/upload` and get a `202 Accepted` response with the new `report_id`.
- **Asynchronous report generation** — uploads return quickly while Testimony runs the Allure CLI in the background with bounded concurrency.
- **Built-in browsing surface** — open `/` to list projects, `/projects/{slug}` to list reports, and `/reports/{slug}/{reportID}/` to view generated output.
- **S3-compatible artifact storage** — raw archives and generated HTML live in an object store instead of the CI runner filesystem.
- **Zero-config local stack** — `docker-compose.yml` packages Testimony with MinIO for a first run on `http://127.0.0.1:18080`.
- **Helm packaging** — the chart maps Kubernetes values back onto the same `TESTIMONY_*` runtime contract instead of introducing a second config model.
- **Operator-visible diagnostics** — `GET /healthz`, `GET /readyz`, and structured logs expose startup, request, upload, generation, auth, and retention state.

## Docker Compose quickstart

The packaged local stack in `docker-compose.yml` is meant to be a literal clean-start demo path: from `docker compose up --build -d` to a browseable generated report on `http://127.0.0.1:18080` in under 5 minutes on a typical dev machine.

### 1. Start the stack and wait for readiness

```bash
docker compose up --build -d
until curl --fail --silent http://127.0.0.1:18080/readyz >/dev/null; do sleep 2; done
```

Then open:

- `http://127.0.0.1:18080/` — Testimony browse UI
- `http://127.0.0.1:18080/healthz` — liveness
- `http://127.0.0.1:18080/readyz` — readiness
- `http://127.0.0.1:19001/` — MinIO console for the local demo stack

Published local ports:

| Service | Host port | Container port |
|---|---:|---:|
| Testimony HTTP | `18080` | `8080` |
| MinIO API | `19000` | `9000` |
| MinIO console | `19001` | `9001` |

### 2. Create a tiny Allure archive and upload it to the real route

The local Compose stack keeps auth **disabled** by default, so this upload does not need an API key.

```bash
tmp_dir="$(mktemp -d)"
mkdir -p "$tmp_dir/allure-results"

python3 - <<'PY' "$tmp_dir/allure-results" "$tmp_dir/allure-results-readme.zip"
import json
import pathlib
import sys
import zipfile

results_dir = pathlib.Path(sys.argv[1])
archive_path = pathlib.Path(sys.argv[2])
result = {
    "uuid": "22222222-2222-2222-2222-222222222222",
    "historyId": "readme-quickstart-history",
    "testCaseId": "readme-quickstart-case",
    "fullName": "readme.quickstart.demo",
    "name": "README quickstart demo",
    "status": "passed",
    "stage": "finished",
    "start": 1710000000000,
    "stop": 1710000001000,
    "labels": [
        {"name": "suite", "value": "readme"},
        {"name": "package", "value": "quickstart"},
    ],
}
result_path = results_dir / "22222222-2222-2222-2222-222222222222-result.json"
result_path.write_text(json.dumps(result), encoding="utf-8")
with zipfile.ZipFile(archive_path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
    archive.write(result_path, arcname=result_path.name)
PY

upload_status="$(curl --silent --show-error \
  --output "$tmp_dir/upload.json" \
  --write-out '%{http_code}' \
  --request POST \
  --header 'Content-Type: application/zip' \
  --header 'Content-Disposition: attachment; filename="allure-results-readme.zip"' \
  --data-binary @"$tmp_dir/allure-results-readme.zip" \
  http://127.0.0.1:18080/api/v1/projects/readme-quickstart/upload)"
test "$upload_status" = "202"

report_id="$(python3 - <<'PY' "$tmp_dir/upload.json"
import json
import sys
body = json.loads(open(sys.argv[1], encoding='utf-8').read())
print(body['report_id'])
PY
)"
printf 'Upload accepted for report_id=%s\n' "$report_id"
```

That request hits the same CI-facing upload API documented elsewhere:

```text
POST /api/v1/projects/{slug}/upload
```

### 3. Poll until the report is browseable, then open the exact URL

```bash
until [ "$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:18080/reports/readme-quickstart/$report_id/")" = "200" ]; do sleep 2; done
printf 'Project page: %s\n' "http://127.0.0.1:18080/projects/readme-quickstart"
printf 'Browse report: %s\n' "http://127.0.0.1:18080/reports/readme-quickstart/$report_id/"
```

`http://127.0.0.1:18080/projects/readme-quickstart` should list the new report, and `http://127.0.0.1:18080/reports/readme-quickstart/$report_id/` should serve the generated Allure UI.

### First-run notes

- Testimony stores local demo data in the named Docker volume mounted at `/var/lib/testimony` inside the container.
- The packaged stack points Testimony at the bundled MinIO container through `TESTIMONY_S3_*` variables.
- When you are done with the demo stack, run `docker compose down -v` to return to a clean local state.

### Timed verifier for the same flow

If you want the repository to enforce the same README quickstart contract automatically, run:

```bash
bash scripts/verify-readme-quickstart.sh
```

That verifier starts from a clean Compose state, proves ready → upload accepted → report browseable, prints the ready/upload/report/total elapsed seconds, and fails if the full path takes longer than 300 seconds (under 5 minutes). On failure it prints the failing stage, the last known HTTP status/URL, `docker compose ps`, and compose logs for `testimony` and `minio`.

You can still run `bash scripts/verify-compose-e2e.sh` for the broader compose smoke path; `verify-readme-quickstart.sh` is the README-aligned timed contract.

## Architecture overview

Testimony ships as one Go process with background workers, not as a multi-service control plane.

At startup it:

1. loads the `TESTIMONY_*` environment contract from `internal/config`
2. opens SQLite for report/project metadata
3. opens S3-compatible storage for raw archives and generated HTML
4. optionally enables bearer-token auth
5. starts the async report generation service
6. mounts the upload API, browse UI, report proxy, and `/healthz` / `/readyz`
7. starts the retention worker for expired reports

Runtime components at a glance:

| Component | Responsibility |
|---|---|
| `internal/upload` | Accept and validate result archives, persist the raw upload, create a pending report, and enqueue generation. |
| `internal/generate` | Merge history, invoke the configured Allure CLI, upload generated HTML, and persist status changes. |
| `internal/serve` | Render the browse UI and proxy generated report assets from object storage. |
| `internal/auth` | Enforce optional bearer-token auth for uploads and, optionally, viewers. |
| `internal/retention` | Delete expired report artifacts and metadata on a background schedule. |
| `internal/server` | Expose request logging, `/healthz`, `/readyz`, and the HTTP router/server. |

For the full walkthrough, object-key layout, request flow, and shutdown behavior, see [docs/architecture.md](docs/architecture.md).

## Comparison

| Capability | Testimony | Raw Allure CLI on a CI runner | Static hosted report artifact |
|---|---|---|---|
| Accept raw Allure results over HTTP | Yes — `POST /api/v1/projects/{slug}/upload` | No | No |
| Generate reports asynchronously after upload | Yes | No — generation happens inline where you run it | No |
| Keep raw archives and generated HTML in S3-compatible storage | Built in | Manual wiring required | Manual wiring required |
| Browse projects and reports from one service URL | Yes | No | Usually one report per published artifact |
| Expose `/healthz`, `/readyz`, and structured logs | Yes | Runner-specific only | Host/platform-specific only |
| Package a local demo stack and Helm chart | Yes | No | No |

## Configuration

Testimony is **env-first**: the Go runtime reads `TESTIMONY_*` variables from `internal/config/config.go`, and Docker Compose plus Helm translate their settings back onto that same contract.

Use these surfaces together:

- [docs/configuration.md](docs/configuration.md) — the full environment-variable reference, defaults, validation rules, and Compose/Helm mapping
- `docker-compose.yml` — the zero-config local overrides for port `18080`, MinIO wiring, local SQLite path, and demo credentials
- `chart/values.yaml` — deployment-facing values that render back into the same `TESTIMONY_*` variables and secret refs

Important knobs include:

- `TESTIMONY_SERVER_*` for bind address and HTTP timeouts
- `TESTIMONY_S3_*` for object storage endpoint, bucket, region, and credentials
- `TESTIMONY_SQLITE_*` for metadata storage
- `TESTIMONY_AUTH_*` for upload/viewer auth
- `TESTIMONY_GENERATE_*` for Allure variant, CLI path, timeout, concurrency, and history depth
- `TESTIMONY_RETENTION_*` and `TESTIMONY_SHUTDOWN_*` for cleanup and graceful shutdown

Do not commit real credentials. For Helm deployments, keep S3 credentials and optional auth keys in `secrets.*` values backed by a generated or operator-managed Secret.

## CI upload examples

Copy-paste examples live under [examples/ci/](examples/ci/):

- [examples/ci/github-actions-upload.yml](examples/ci/github-actions-upload.yml)
- [examples/ci/gitlab-ci-upload.yml](examples/ci/gitlab-ci-upload.yml)
- [examples/ci/README.md](examples/ci/README.md)

They package an Allure results archive and upload it to the real route:

```text
POST /api/v1/projects/{slug}/upload
```

The examples use CI-friendly placeholders such as `TESTIMONY_BASE_URL`, `TESTIMONY_PROJECT_SLUG`, and optional `TESTIMONY_API_KEY` instead of repository-specific values.

## Troubleshooting and observability

When something goes wrong, prefer the built-in inspection surfaces before digging through code:

- `GET /healthz` — confirms the HTTP process is alive
- `GET /readyz` — confirms SQLite and S3 readiness
- structured logs — look for events such as `http server listening`, `upload accepted`, `auth rejected`, `report_generation_started`, `report_generation_completed`, `report_generation_failed`, and retention cleanup events
- `bash scripts/verify-compose-e2e.sh` — exercises the packaged local stack and prints compose diagnostics on failure
- `bash scripts/verify-helm-chart.sh` — runs `helm lint` and `helm template`, then asserts the rendered manifest still matches the env-first runtime contract

## GitHub repository baseline

The CI upload examples under [examples/ci/](examples/ci/) are meant for downstream repositories, but this repository still needs its own GitHub automation and metadata baseline. See [docs/github-repository.md](docs/github-repository.md) for the current repository-level recommendation: what to run in GitHub Actions now, what to defer until release policy is stable, and the proposed GitHub description/website/topics values.

## Repository guide

- [docs/architecture.md](docs/architecture.md) — runtime topology, request/data flow, storage layout, and observability surfaces
- [docs/configuration.md](docs/configuration.md) — full `TESTIMONY_*` configuration reference
- [docs/github-repository.md](docs/github-repository.md) — repository-level GitHub CI, release, and metadata baseline
- [docs/release-guide.md](docs/release-guide.md) — tag-driven release workflow, GHCR tag policy, and release operator guide
- [CONTRIBUTING.md](CONTRIBUTING.md) — contributor workflow, verification commands, and PR expectations
- [examples/ci/](examples/ci/) — GitHub Actions and GitLab CI upload examples
- [LICENSE](LICENSE) — Apache-2.0 license
