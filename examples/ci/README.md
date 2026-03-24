# CI upload examples

This directory contains copy-paste examples that send an Allure results archive to Testimony's real upload endpoint:

- `POST /api/v1/projects/{slug}/upload`
- success response: HTTP `202 Accepted` with JSON fields such as `report_id`, `status`, `archive_format`, and `archive_object_key`
- optional auth: `Authorization: Bearer <api-key>` when upload auth is enabled

The examples live under `examples/ci/` on purpose. They are safe to commit here, and you can copy them into `.github/workflows/` or `.gitlab-ci.yml` in another repository without triggering automation in this repository.

## Required variables and secrets

Both examples expect these values to come from your CI system's variable or secret store:

- `TESTIMONY_BASE_URL` — base URL for your Testimony instance, without the upload path. For the local Docker Compose quickstart in this repository, use `http://127.0.0.1:18080`.
- `TESTIMONY_PROJECT_SLUG` — the target project slug in Testimony. It becomes the `{slug}` segment in `POST /api/v1/projects/{slug}/upload`.
- `TESTIMONY_API_KEY` — optional CI secret used only when the server requires upload auth. Leave it unset for the local Compose quickstart because `docker-compose.yml` does not enable auth by default.
- `ALLURE_RESULTS_DIR` — directory that contains raw Allure result files when your pipeline has not already created an archive. The examples default to `allure-results`.
- `TESTIMONY_RESULTS_ARCHIVE` — path to the `.zip` file uploaded to Testimony. Point this at an existing archive if your pipeline already packages results, or let the examples create one from `ALLURE_RESULTS_DIR`.

Do not commit real endpoints, tokens, or project-specific identifiers. Keep them in CI-managed variables or secrets.

## Choosing the archive path

`internal/upload/handler.go` accepts either:

- a raw archive upload body, which is what these examples send with `curl --data-binary`, or
- a multipart upload with a file part.

The handler recognizes archives by filename and content, then unpacks them before report generation. The examples use a `.zip` file because it is easy to create on GitHub-hosted and GitLab-hosted runners. Your archive should contain the files that normally live inside `allure-results/`, such as `*-result.json`, attachments, and supporting metadata.

Use one of these patterns:

1. If your tests already produce `allure-results.zip`, set `TESTIMONY_RESULTS_ARCHIVE` to that file and skip repackaging.
2. If your tests produce a directory such as `allure-results/`, keep `ALLURE_RESULTS_DIR=allure-results` and let the example zip the directory contents.
3. If your pipeline prefers `.tar.gz`, update both the filename and the `Content-Type` header to match the archive you send.

## Local Compose quickstart vs authenticated deployments

For local testing against this repository's packaged stack:

- start the stack with Docker Compose
- wait for `GET /readyz` to return `200 OK`
- use `TESTIMONY_BASE_URL=http://127.0.0.1:18080`
- omit `TESTIMONY_API_KEY`

For shared or production-style deployments:

- keep upload auth aligned with the runtime contract in `TESTIMONY_AUTH_ENABLED` and `TESTIMONY_AUTH_API_KEY`
- if you deploy via Helm, provide the API key through the chart secret value `secrets.authAPIKey`
- store the matching client credential in your CI secret store as `TESTIMONY_API_KEY`
- send it as `Authorization: Bearer $TESTIMONY_API_KEY`

## Troubleshooting and observability

If the upload step fails, inspect the same surfaces the runtime already exposes:

- `GET /healthz` and `GET /readyz` to confirm the service is alive and ready
- structured logs such as `bootstrap api key ready`, `auth rejected`, `upload rejected`, and `upload accepted`
- the `curl` response body for JSON error details from the upload handler
- `bash scripts/verify-compose-e2e.sh` when validating the local Compose stack, because it prints `docker compose ps` plus Testimony and MinIO logs on failure

A healthy upload returns `202 Accepted`, after which Testimony continues report generation asynchronously.
