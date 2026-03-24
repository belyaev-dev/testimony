#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/docker-compose.yml"
PROJECT_NAME="testimony-compose-smoke"
BASE_URL="http://127.0.0.1:18080"
PROJECT_SLUG="compose-smoke"
STARTUP_TIMEOUT_SECONDS=180
REPORT_TIMEOUT_SECONDS=180
POLL_INTERVAL_SECONDS=2

TMP_DIR=""
UPLOAD_ARCHIVE=""
UPLOAD_HEADERS=""
UPLOAD_BODY=""
ROOT_BODY=""
PROJECT_BODY=""
REPORT_BODY=""
SUMMARY_BODY=""

log() {
  printf '[verify-compose-e2e] %s\n' "$*"
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required tool: $1" >&2
    exit 1
  }
}

compose() {
  docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" "$@"
}

print_failure_diagnostics() {
  log "compose service status"
  compose ps || true
  log "testimony logs"
  compose logs --no-color testimony || true
  log "minio logs"
  compose logs --no-color minio || true
}

cleanup_compose() {
  compose down -v --remove-orphans >/dev/null 2>&1 || true
}

cleanup() {
  local exit_code=$?
  if [[ $exit_code -ne 0 ]]; then
    print_failure_diagnostics
  fi

  cleanup_compose

  if docker ps -a --filter "label=com.docker.compose.project=${PROJECT_NAME}" --format '{{.Names}}' | grep -q .; then
    echo "compose cleanup left containers behind for project ${PROJECT_NAME}" >&2
    docker ps -a --filter "label=com.docker.compose.project=${PROJECT_NAME}" >&2 || true
    exit_code=1
  fi
  if docker volume ls -q --filter "label=com.docker.compose.project=${PROJECT_NAME}" | grep -q .; then
    echo "compose cleanup left volumes behind for project ${PROJECT_NAME}" >&2
    docker volume ls --filter "label=com.docker.compose.project=${PROJECT_NAME}" >&2 || true
    exit_code=1
  fi

  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi

  trap - EXIT
  exit "$exit_code"
}
trap cleanup EXIT

assert_contains() {
  local haystack=$1
  local needle=$2
  local message=$3
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "assertion failed: $message" >&2
    echo "expected to find: $needle" >&2
    echo "actual output:" >&2
    printf '%s\n' "$haystack" >&2
    exit 1
  fi
}

wait_for_http_ok() {
  local url=$1
  local timeout_seconds=$2
  local start
  start=$(date +%s)

  while true; do
    if curl --fail --silent "$url" >/dev/null; then
      return 0
    fi

    if (( $(date +%s) - start >= timeout_seconds )); then
      echo "timed out waiting for $url" >&2
      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done
}

create_archive() {
  TMP_DIR="$(mktemp -d)"
  local results_dir="$TMP_DIR/allure-results"
  UPLOAD_ARCHIVE="$TMP_DIR/allure-results-compose.zip"
  mkdir -p "$results_dir"

  python3 - <<'PY' "$results_dir" "$UPLOAD_ARCHIVE"
import json
import pathlib
import sys
import zipfile

results_dir = pathlib.Path(sys.argv[1])
archive_path = pathlib.Path(sys.argv[2])
result = {
    "uuid": "11111111-1111-1111-1111-111111111111",
    "historyId": "compose-smoke-history",
    "testCaseId": "compose-smoke-case",
    "fullName": "compose.smoke.test",
    "name": "compose smoke test",
    "status": "passed",
    "stage": "finished",
    "start": 1710000000000,
    "stop": 1710000001000,
    "labels": [
        {"name": "suite", "value": "compose"},
        {"name": "package", "value": "smoke"},
    ],
}
result_path = results_dir / "11111111-1111-1111-1111-111111111111-result.json"
result_path.write_text(json.dumps(result), encoding="utf-8")
with zipfile.ZipFile(archive_path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
    archive.write(result_path, arcname=result_path.name)
PY
}

upload_archive() {
  UPLOAD_HEADERS="$TMP_DIR/upload.headers"
  UPLOAD_BODY="$TMP_DIR/upload.json"

  local status_code
  status_code=$(curl --silent --show-error \
    --output "$UPLOAD_BODY" \
    --dump-header "$UPLOAD_HEADERS" \
    --write-out '%{http_code}' \
    --request POST \
    --header 'Content-Type: application/zip' \
    --header 'Content-Disposition: attachment; filename="allure-results-compose.zip"' \
    --data-binary "@$UPLOAD_ARCHIVE" \
    "$BASE_URL/api/v1/projects/$PROJECT_SLUG/upload")

  if [[ "$status_code" != "202" ]]; then
    echo "upload returned unexpected status: $status_code" >&2
    cat "$UPLOAD_BODY" >&2 || true
    exit 1
  fi

  python3 - <<'PY' "$UPLOAD_BODY"
import json
import sys
body = json.loads(open(sys.argv[1], encoding='utf-8').read())
print(body['report_id'])
print(body['status'])
print(body['archive_object_key'])
PY
}

wait_for_report_ready() {
  local report_id=$1
  local start
  start=$(date +%s)

  while true; do
    local project_status report_status
    project_status=$(curl --silent --output "$TMP_DIR/project-poll.html" --write-out '%{http_code}' "$BASE_URL/projects/$PROJECT_SLUG")
    report_status=$(curl --silent --output "$TMP_DIR/report-poll.html" --write-out '%{http_code}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id/")

    if [[ "$project_status" == "200" ]] && [[ "$report_status" == "200" ]] && grep -Fq "/reports/$PROJECT_SLUG/$report_id/" "$TMP_DIR/project-poll.html"; then
      log "report became browseable: report_id=$report_id project_status=$project_status report_status=$report_status"
      return 0
    fi

    if (( $(date +%s) - start >= REPORT_TIMEOUT_SECONDS )); then
      echo "timed out waiting for report $report_id to become browseable (project_status=$project_status report_status=$report_status)" >&2
      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done
}

fetch_and_assert_pages() {
  local report_id=$1
  ROOT_BODY="$TMP_DIR/root.html"
  PROJECT_BODY="$TMP_DIR/project.html"
  REPORT_BODY="$TMP_DIR/report.html"
  SUMMARY_BODY="$TMP_DIR/summary.json"

  local root_status project_status redirect_status report_status summary_status redirect_location

  root_status=$(curl --silent --output "$ROOT_BODY" --write-out '%{http_code}' "$BASE_URL/")
  if [[ "$root_status" != "200" ]]; then
    echo "root page returned unexpected status: $root_status" >&2
    exit 1
  fi

  project_status=$(curl --silent --output "$PROJECT_BODY" --write-out '%{http_code}' "$BASE_URL/projects/$PROJECT_SLUG")
  if [[ "$project_status" != "200" ]]; then
    echo "project page returned unexpected status: $project_status" >&2
    exit 1
  fi

  redirect_location=$(curl --silent --show-error --output /dev/null --dump-header "$TMP_DIR/report-redirect.headers" --write-out '%{redirect_url}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id")
  redirect_status=$(python3 - <<'PY' "$TMP_DIR/report-redirect.headers"
import sys
for line in open(sys.argv[1], encoding='utf-8').read().splitlines():
    if line.startswith('HTTP/'):
        print(line.split()[1])
        break
PY
)
  if [[ "$redirect_status" != "302" ]]; then
    echo "bare report route returned unexpected status: $redirect_status" >&2
    exit 1
  fi

  report_status=$(curl --silent --output "$REPORT_BODY" --write-out '%{http_code}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id/")
  if [[ "$report_status" != "200" ]]; then
    echo "report root returned unexpected status: $report_status" >&2
    exit 1
  fi

  summary_status=$(curl --silent --output "$SUMMARY_BODY" --write-out '%{http_code}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id/widgets/summary.json")
  if [[ "$summary_status" != "200" ]]; then
    echo "report summary returned unexpected status: $summary_status" >&2
    exit 1
  fi

  assert_contains "$(cat "$ROOT_BODY")" "Browse generated reports" "root page should expose the browse UI"
  assert_contains "$(cat "$ROOT_BODY")" "$PROJECT_SLUG" "root page should list the uploaded project"
  assert_contains "$(cat "$PROJECT_BODY")" "$report_id" "project page should list the uploaded report"
  assert_contains "$(cat "$PROJECT_BODY")" "/reports/$PROJECT_SLUG/$report_id/" "project page should link to the ready report"
  assert_contains "$redirect_location" "/reports/$PROJECT_SLUG/$report_id/" "bare report route should redirect to the trailing slash URL"
  assert_contains "$(cat "$REPORT_BODY")" "styles.css" "report root should serve generated Allure HTML"

  python3 - <<'PY' "$SUMMARY_BODY"
import json
import sys
summary = json.loads(open(sys.argv[1], encoding='utf-8').read())
statistic = summary.get('statistic', {})
if statistic.get('passed') != 1 or statistic.get('total') != 1:
    raise SystemExit(f"unexpected summary statistics: {statistic}")
PY
}

main() {
  require_tool docker
  require_tool curl
  require_tool python3

  if [[ ! -f "$COMPOSE_FILE" ]]; then
    echo "missing compose file: $COMPOSE_FILE" >&2
    exit 1
  fi

  cleanup_compose

  log "starting compose stack"
  compose up --build -d
  log "waiting for readiness at $BASE_URL/readyz"
  wait_for_http_ok "$BASE_URL/readyz" "$STARTUP_TIMEOUT_SECONDS"
  compose ps

  create_archive
  log "uploading synthesized Allure results archive"
  local upload_result report_id upload_status archive_object_key
  upload_result="$(upload_archive)"
  report_id="$(printf '%s\n' "$upload_result" | sed -n '1p')"
  upload_status="$(printf '%s\n' "$upload_result" | sed -n '2p')"
  archive_object_key="$(printf '%s\n' "$upload_result" | sed -n '3p')"
  log "upload accepted: report_id=$report_id status=$upload_status archive_object_key=$archive_object_key"

  wait_for_report_ready "$report_id"
  fetch_and_assert_pages "$report_id"

  log "compose smoke flow passed"
}

main "$@"
