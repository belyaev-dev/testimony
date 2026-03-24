#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_IMAGE="ghcr.io/testimony-dev/testimony:local-allure2"
ALLURE3_IMAGE="ghcr.io/testimony-dev/testimony:local-allure3"
MINIO_IMAGE="minio/minio:RELEASE.2024-01-16T16-07-38Z"
MODE="smoke"

if [[ "${1:-}" == "--failure-check" ]]; then
  MODE="failure-check"
elif [[ -n "${1:-}" ]]; then
  echo "usage: $0 [--failure-check]" >&2
  exit 64
fi

network_name=""
minio_name=""
app_name=""

cleanup() {
  local exit_code=$?
  if [[ $exit_code -ne 0 ]]; then
    if [[ -n "$app_name" ]]; then
      echo "--- testimony logs ($app_name) ---" >&2
      docker logs "$app_name" >&2 || true
    fi
    if [[ -n "$minio_name" ]]; then
      echo "--- minio logs ($minio_name) ---" >&2
      docker logs "$minio_name" >&2 || true
    fi
  fi

  if [[ -n "$app_name" ]]; then
    docker rm -f "$app_name" >/dev/null 2>&1 || true
  fi
  if [[ -n "$minio_name" ]]; then
    docker rm -f "$minio_name" >/dev/null 2>&1 || true
  fi
  if [[ -n "$network_name" ]]; then
    docker network rm "$network_name" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

log() {
  printf '[verify-docker-images] %s\n' "$*"
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required tool: $1" >&2
    exit 1
  }
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
    sleep 1
  done
}

container_host_port() {
  local container=$1
  local private_port=$2
  docker inspect --format "{{(index (index .NetworkSettings.Ports \"${private_port}/tcp\") 0).HostPort}}" "$container"
}

build_images() {
  log "building ${DEFAULT_IMAGE}"
  docker build \
    --file "$ROOT_DIR/Dockerfile" \
    --tag "$DEFAULT_IMAGE" \
    "$ROOT_DIR"

  log "building ${ALLURE3_IMAGE}"
  docker build \
    --file "$ROOT_DIR/Dockerfile.allure3" \
    --tag "$ALLURE3_IMAGE" \
    "$ROOT_DIR"
}

start_minio() {
  local suffix=$1
  network_name="testimony-docker-verify-${suffix}-net"
  minio_name="testimony-docker-verify-${suffix}-minio"
  docker network create "$network_name" >/dev/null

  log "starting MinIO (${minio_name})"
  docker run -d \
    --name "$minio_name" \
    --network "$network_name" \
    -p 127.0.0.1::9000 \
    -e MINIO_ROOT_USER=minioadmin \
    -e MINIO_ROOT_PASSWORD=minioadmin \
    "$MINIO_IMAGE" server /data >/dev/null

  local minio_port
  minio_port=$(container_host_port "$minio_name" 9000)
  wait_for_http_ok "http://127.0.0.1:${minio_port}/minio/health/ready" 30
}

start_app() {
  local image=$1
  local suffix=$2
  app_name="testimony-docker-verify-${suffix}-app"

  log "starting ${image} (${app_name})"
  docker run -d \
    --name "$app_name" \
    --network "$network_name" \
    -p 127.0.0.1::8080 \
    -e TESTIMONY_S3_ENDPOINT=http://testimony-docker-verify-${suffix}-minio:9000 \
    -e TESTIMONY_S3_REGION=us-east-1 \
    -e TESTIMONY_S3_BUCKET=testimony \
    -e TESTIMONY_S3_ACCESS_KEY_ID=minioadmin \
    -e TESTIMONY_S3_SECRET_ACCESS_KEY=minioadmin \
    -e TESTIMONY_S3_USE_PATH_STYLE=true \
    "$image" >/dev/null

  local app_port
  app_port=$(container_host_port "$app_name" 8080)
  wait_for_http_ok "http://127.0.0.1:${app_port}/healthz" 30
  wait_for_http_ok "http://127.0.0.1:${app_port}/readyz" 30
}

verify_default_image_contract() {
  log "verifying default image runtime contract"
  docker exec "$app_name" /bin/sh -c 'test "$TESTIMONY_GENERATE_VARIANT" = "allure2"'
  docker exec "$app_name" /bin/sh -c 'test "$TESTIMONY_GENERATE_CLI_PATH" = "/usr/local/bin/allure"'
  docker exec "$app_name" /bin/sh -c 'test -w "$(dirname "$TESTIMONY_SQLITE_PATH")" && test -w "$TESTIMONY_TEMP_DIR"'
  docker exec "$app_name" /bin/sh -c 'command -v allure >/dev/null && allure --version'
  docker exec "$app_name" /bin/sh -c 'java -version >/dev/null 2>&1'
}

verify_allure3_image_contract() {
  log "verifying Allure 3 image runtime contract"
  docker exec "$app_name" /bin/sh -c 'test "$TESTIMONY_GENERATE_VARIANT" = "allure3"'
  docker exec "$app_name" /bin/sh -c 'test "$TESTIMONY_GENERATE_CLI_PATH" = "/usr/local/bin/allure"'
  docker exec "$app_name" /bin/sh -c 'test -w "$(dirname "$TESTIMONY_SQLITE_PATH")" && test -w "$TESTIMONY_TEMP_DIR"'
  docker exec "$app_name" /bin/sh -c 'command -v allure >/dev/null && allure --version'
  docker exec "$app_name" /bin/sh -c 'node --version >/dev/null 2>&1'
}

verify_failure_output() {
  log "verifying startup failure output"
  local output
  set +e
  output=$(docker run --rm -e TESTIMONY_GENERATE_VARIANT=broken "$DEFAULT_IMAGE" 2>&1)
  local status=$?
  set -e

  if [[ $status -eq 0 ]]; then
    echo "expected startup failure when TESTIMONY_GENERATE_VARIANT is invalid" >&2
    exit 1
  fi

  assert_contains "$output" "TESTIMONY_GENERATE_VARIANT" "startup failure output should name the invalid env var"
  assert_contains "$output" "must be one of" "startup failure output should describe the validation failure"
}

run_variant_smoke() {
  local image=$1
  local suffix=$2
  local verifier=$3

  start_minio "$suffix"
  start_app "$image" "$suffix"
  "$verifier"

  docker rm -f "$app_name" >/dev/null
  app_name=""
  docker rm -f "$minio_name" >/dev/null
  minio_name=""
  docker network rm "$network_name" >/dev/null
  network_name=""
}

main() {
  require_tool docker
  require_tool curl

  build_images

  if [[ "$MODE" == "failure-check" ]]; then
    verify_failure_output
    log "failure-path verification passed"
    return
  fi

  run_variant_smoke "$DEFAULT_IMAGE" "allure2" verify_default_image_contract
  run_variant_smoke "$ALLURE3_IMAGE" "allure3" verify_allure3_image_contract
  log "all Docker image checks passed"
}

main "$@"
