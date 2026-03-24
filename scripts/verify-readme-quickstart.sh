#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/docker-compose.yml"
PROJECT_NAME="testimony-readme-quickstart"
BASE_URL="http://127.0.0.1:18080"
PROJECT_SLUG="readme-quickstart"
STARTUP_TIMEOUT_SECONDS=180
REPORT_TIMEOUT_SECONDS=180
QUICKSTART_BUDGET_SECONDS=300
POLL_INTERVAL_SECONDS=2
FLOW_START_SECONDS="$(date +%s)"

TMP_DIR=""
UPLOAD_ARCHIVE=""
UPLOAD_HEADERS=""
UPLOAD_BODY=""
ROOT_BODY=""
PROJECT_BODY=""
REPORT_BODY=""
SUMMARY_BODY=""
READY_BODY=""
CURRENT_STAGE="init"
LAST_HTTP_URL=""
LAST_HTTP_STATUS=""
LAST_HTTP_CONTEXT=""
READY_ELAPSED_SECONDS=""
UPLOAD_ELAPSED_SECONDS=""
REPORT_ELAPSED_SECONDS=""
TOTAL_ELAPSED_SECONDS=""
REPORT_ID=""
PROJECT_URL=""
REPORT_URL=""

log() {
  printf '[verify-readme-quickstart] %s\n' "$*"
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

elapsed_seconds() {
  printf '%s\n' "$(( $(date +%s) - FLOW_START_SECONDS ))"
}

record_http() {
  LAST_HTTP_URL="$1"
  LAST_HTTP_STATUS="$2"
  LAST_HTTP_CONTEXT="${3:-}"
}

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

cleanup_compose() {
  compose down -v --remove-orphans >/dev/null 2>&1 || true
}

print_failure_diagnostics() {
  TOTAL_ELAPSED_SECONDS="$(elapsed_seconds)"
  log "failing stage: $CURRENT_STAGE"
  log "elapsed checkpoints: readyz=${READY_ELAPSED_SECONDS:-n/a}s upload=${UPLOAD_ELAPSED_SECONDS:-n/a}s report_browseable=${REPORT_ELAPSED_SECONDS:-n/a}s total=${TOTAL_ELAPSED_SECONDS}s"
  if [[ -n "$LAST_HTTP_URL" || -n "$LAST_HTTP_STATUS" || -n "$LAST_HTTP_CONTEXT" ]]; then
    log "last known HTTP: status=${LAST_HTTP_STATUS:-n/a} url=${LAST_HTTP_URL:-n/a} context=${LAST_HTTP_CONTEXT:-n/a}"
  fi
  if [[ -n "$PROJECT_URL" ]]; then
    log "project URL: $PROJECT_URL"
  fi
  if [[ -n "$REPORT_URL" ]]; then
    log "report URL: $REPORT_URL"
  fi
  log "docker compose ps"
  compose ps || true
  log "compose logs --no-color testimony"
  compose logs --no-color testimony || true
  log "compose logs --no-color minio"
  compose logs --no-color minio || true
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

fail() {
  echo "$*" >&2
  exit 1
}

check_budget() {
  local checkpoint=$1
  TOTAL_ELAPSED_SECONDS="$(elapsed_seconds)"
  if (( TOTAL_ELAPSED_SECONDS > QUICKSTART_BUDGET_SECONDS )); then
    CURRENT_STAGE="budget_exceeded_after_${checkpoint}"
    fail "README quickstart exceeded ${QUICKSTART_BUDGET_SECONDS}s after ${checkpoint}: total=${TOTAL_ELAPSED_SECONDS}s"
  fi
}

wait_for_ready() {
  local url="$BASE_URL/readyz"
  local started_at
  READY_BODY="$TMP_DIR/readyz.body"
  started_at=$(date +%s)
  CURRENT_STAGE="wait_for_readyz"

  while true; do
    local status_code
    status_code=$(curl --silent --output "$READY_BODY" --write-out '%{http_code}' "$url" || true)
    record_http "$url" "$status_code" "waiting for /readyz"

    if [[ "$status_code" == "200" ]]; then
      READY_ELAPSED_SECONDS="$(elapsed_seconds)"
      log "readyz OK: status=$status_code elapsed=${READY_ELAPSED_SECONDS}s url=$url"
      return 0
    fi

    if (( $(date +%s) - started_at >= STARTUP_TIMEOUT_SECONDS )); then
      fail "timed out waiting for /readyz at $url after ${STARTUP_TIMEOUT_SECONDS}s (last_status=$status_code)"
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done
}

create_archive() {
  CURRENT_STAGE="create_archive"
  UPLOAD_ARCHIVE="$TMP_DIR/allure-results-readme.zip"
  local results_dir="$TMP_DIR/allure-results"
  mkdir -p "$results_dir"

  python3 - <<'PY' "$results_dir" "$UPLOAD_ARCHIVE"
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
}

upload_archive() {
  CURRENT_STAGE="upload_archive"
  UPLOAD_HEADERS="$TMP_DIR/upload.headers"
  UPLOAD_BODY="$TMP_DIR/upload.json"

  local status_code
  status_code=$(curl --silent --show-error \
    --output "$UPLOAD_BODY" \
    --dump-header "$UPLOAD_HEADERS" \
    --write-out '%{http_code}' \
    --request POST \
    --header 'Content-Type: application/zip' \
    --header 'Content-Disposition: attachment; filename="allure-results-readme.zip"' \
    --data-binary "@$UPLOAD_ARCHIVE" \
    "$BASE_URL/api/v1/projects/$PROJECT_SLUG/upload")
  record_http "$BASE_URL/api/v1/projects/$PROJECT_SLUG/upload" "$status_code" "upload archive"

  if [[ "$status_code" != "202" ]]; then
    fail "upload returned unexpected status: $status_code"
  fi

  local parsed report_status archive_object_key
  parsed="$(python3 - <<'PY' "$UPLOAD_BODY"
import json
import sys
body = json.loads(open(sys.argv[1], encoding='utf-8').read())
print(body['report_id'])
print(body['status'])
print(body.get('archive_object_key', ''))
PY
)"
  REPORT_ID="$(printf '%s\n' "$parsed" | sed -n '1p')"
  report_status="$(printf '%s\n' "$parsed" | sed -n '2p')"
  archive_object_key="$(printf '%s\n' "$parsed" | sed -n '3p')"
  PROJECT_URL="$BASE_URL/projects/$PROJECT_SLUG"
  REPORT_URL="$BASE_URL/reports/$PROJECT_SLUG/$REPORT_ID/"
  UPLOAD_ELAPSED_SECONDS="$(elapsed_seconds)"

  log "upload accepted: http_status=$status_code elapsed=${UPLOAD_ELAPSED_SECONDS}s report_id=$REPORT_ID status=$report_status archive_object_key=$archive_object_key"
}

wait_for_report_ready() {
  CURRENT_STAGE="wait_for_report_browseable"
  local started_at
  started_at=$(date +%s)

  while true; do
    local project_status report_status
    project_status=$(curl --silent --output "$TMP_DIR/project-poll.html" --write-out '%{http_code}' "$PROJECT_URL" || true)
    record_http "$PROJECT_URL" "$project_status" "project page poll"
    report_status=$(curl --silent --output "$TMP_DIR/report-poll.html" --write-out '%{http_code}' "$REPORT_URL" || true)
    record_http "$REPORT_URL" "$report_status" "report page poll"

    if [[ "$project_status" == "200" ]] && [[ "$report_status" == "200" ]] && grep -Fq "/reports/$PROJECT_SLUG/$REPORT_ID/" "$TMP_DIR/project-poll.html"; then
      REPORT_ELAPSED_SECONDS="$(elapsed_seconds)"
      log "report browseable: project_status=$project_status report_status=$report_status elapsed=${REPORT_ELAPSED_SECONDS}s url=$REPORT_URL"
      return 0
    fi

    if (( $(date +%s) - started_at >= REPORT_TIMEOUT_SECONDS )); then
      fail "timed out waiting for browseable report after ${REPORT_TIMEOUT_SECONDS}s (project_status=${project_status:-unknown} report_status=${report_status:-unknown})"
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done
}

fetch_and_assert_pages() {
  CURRENT_STAGE="assert_pages"
  ROOT_BODY="$TMP_DIR/root.html"
  PROJECT_BODY="$TMP_DIR/project.html"
  REPORT_BODY="$TMP_DIR/report.html"
  SUMMARY_BODY="$TMP_DIR/summary.json"

  local root_status project_status redirect_status report_status summary_status redirect_location

  root_status=$(curl --silent --output "$ROOT_BODY" --write-out '%{http_code}' "$BASE_URL/")
  record_http "$BASE_URL/" "$root_status" "browse UI root"
  if [[ "$root_status" != "200" ]]; then
    fail "root page returned unexpected status: $root_status"
  fi

  project_status=$(curl --silent --output "$PROJECT_BODY" --write-out '%{http_code}' "$PROJECT_URL")
  record_http "$PROJECT_URL" "$project_status" "project history page"
  if [[ "$project_status" != "200" ]]; then
    fail "project page returned unexpected status: $project_status"
  fi

  redirect_location=$(curl --silent --show-error --output /dev/null --dump-header "$TMP_DIR/report-redirect.headers" --write-out '%{redirect_url}' "$BASE_URL/reports/$PROJECT_SLUG/$REPORT_ID")
  redirect_status=$(python3 - <<'PY' "$TMP_DIR/report-redirect.headers"
import sys
for line in open(sys.argv[1], encoding='utf-8').read().splitlines():
    if line.startswith('HTTP/'):
        print(line.split()[1])
        break
PY
)
  record_http "$BASE_URL/reports/$PROJECT_SLUG/$REPORT_ID" "$redirect_status" "bare report redirect"
  if [[ "$redirect_status" != "302" ]]; then
    fail "bare report route returned unexpected status: $redirect_status"
  fi

  report_status=$(curl --silent --output "$REPORT_BODY" --write-out '%{http_code}' "$REPORT_URL")
  record_http "$REPORT_URL" "$report_status" "report root"
  if [[ "$report_status" != "200" ]]; then
    fail "report root returned unexpected status: $report_status"
  fi

  summary_status=$(curl --silent --output "$SUMMARY_BODY" --write-out '%{http_code}' "$REPORT_URL/widgets/summary.json")
  record_http "$REPORT_URL/widgets/summary.json" "$summary_status" "report summary widget"
  if [[ "$summary_status" != "200" ]]; then
    fail "report summary returned unexpected status: $summary_status"
  fi

  assert_contains "$(cat "$ROOT_BODY")" "Browse generated reports" "root page should expose the browse UI"
  assert_contains "$(cat "$ROOT_BODY")" "$PROJECT_SLUG" "root page should list the uploaded project"
  assert_contains "$(cat "$PROJECT_BODY")" "$REPORT_ID" "project page should list the uploaded report"
  assert_contains "$(cat "$PROJECT_BODY")" "/reports/$PROJECT_SLUG/$REPORT_ID/" "project page should link to the ready report"
  assert_contains "$redirect_location" "/reports/$PROJECT_SLUG/$REPORT_ID/" "bare report route should redirect to the trailing slash URL"
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
    fail "missing compose file: $COMPOSE_FILE"
  fi

  TMP_DIR="$(mktemp -d)"

  CURRENT_STAGE="clean_previous_state"
  cleanup_compose

  CURRENT_STAGE="compose_up"
  log "starting clean Compose stack"
  compose up --build -d

  wait_for_ready
  check_budget "readyz"

  CURRENT_STAGE="compose_status"
  compose ps

  create_archive
  upload_archive
  check_budget "upload"

  wait_for_report_ready
  fetch_and_assert_pages

  TOTAL_ELAPSED_SECONDS="$(elapsed_seconds)"
  if (( TOTAL_ELAPSED_SECONDS > QUICKSTART_BUDGET_SECONDS )); then
    CURRENT_STAGE="budget_exceeded_after_report_browseable"
    fail "README quickstart exceeded ${QUICKSTART_BUDGET_SECONDS}s: total=${TOTAL_ELAPSED_SECONDS}s"
  fi

  log "PASS README quickstart under 5 minutes: readyz=${READY_ELAPSED_SECONDS}s upload=${UPLOAD_ELAPSED_SECONDS}s report_browseable=${REPORT_ELAPSED_SECONDS}s total=${TOTAL_ELAPSED_SECONDS}s project_url=$PROJECT_URL report_url=$REPORT_URL"
}

main "$@"
