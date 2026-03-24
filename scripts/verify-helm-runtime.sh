#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="$ROOT_DIR/chart"
IMAGE_REPOSITORY="ghcr.io/testimony-dev/testimony"
IMAGE_TAG="helm-test"
IMAGE_REF="$IMAGE_REPOSITORY:$IMAGE_TAG"
RELEASE_NAME="testimony-runtime"
PROJECT_SLUG="helm-runtime-smoke"
MINIO_POD_NAME="minio"
MINIO_SERVICE_NAME="minio"
MINIO_IMAGE="minio/minio:RELEASE.2024-01-16T16-07-38Z"
MINIO_ENDPOINT_HOST="minio"
MINIO_PORT="9000"
STARTUP_TIMEOUT_SECONDS=300
REPORT_TIMEOUT_SECONDS=240
DRAIN_TIMEOUT_SECONDS=30
POLL_INTERVAL_SECONDS=2
KEEP_NAMESPACE="${KEEP_NAMESPACE:-0}"
FORCE_IMAGE_BUILD="${FORCE_IMAGE_BUILD:-0}"

TMP_DIR=""
NAMESPACE=""
DEFAULT_STORAGE_CLASS=""
SERVICE_NAME=""
TESTIMONY_POD=""
BASE_URL=""
LOCAL_PORT=""
PORT_FORWARD_PID=""
PORT_FORWARD_LOG=""
TESTIMONY_LOG_PID=""
TESTIMONY_LOG_FILE=""
UPLOAD_ARCHIVE=""
UPLOAD_HEADERS=""
UPLOAD_BODY=""
ROOT_BODY=""
PROJECT_BODY=""
REPORT_BODY=""
SUMMARY_BODY=""
LAST_HEALTH_STATUS=""
LAST_HEALTH_BODY=""
LAST_READY_STATUS=""
LAST_READY_BODY=""
LAST_UPLOAD_STATUS=""
LAST_UPLOAD_BODY=""
LAST_PROJECT_STATUS=""
LAST_REPORT_STATUS=""
LAST_REDIRECT_STATUS=""
LAST_REDIRECT_LOCATION=""
LAST_SUMMARY_STATUS=""
LAST_DRAIN_STATUS=""
LAST_DRAIN_BODY=""
LAST_REPORT_ID=""
LAST_UPLOAD_RESPONSE_STATUS=""
LAST_ARCHIVE_OBJECT_KEY=""

log() {
  printf '[verify-helm-runtime] %s\n' "$*"
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required tool: $1" >&2
    exit 1
  }
}

kubectl_ns() {
  kubectl --namespace "$NAMESPACE" "$@"
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

json_field() {
  local file=$1
  local field=$2
  python3 - <<'PY' "$file" "$field"
import json
import sys
payload = json.loads(open(sys.argv[1], encoding='utf-8').read())
value = payload
for part in sys.argv[2].split('.'):
    if isinstance(value, dict):
        value = value.get(part)
    else:
        value = None
        break
if value is None:
    raise SystemExit(1)
if isinstance(value, bool):
    print(str(value).lower())
elif isinstance(value, (int, float)):
    print(value)
else:
    print(str(value))
PY
}

assert_json_field() {
  local file=$1
  local field=$2
  local expected=$3
  local message=$4
  local actual
  actual="$(json_field "$file" "$field")"
  if [[ "$actual" != "$expected" ]]; then
    echo "assertion failed: $message" >&2
    echo "field: $field" >&2
    echo "expected: $expected" >&2
    echo "actual: $actual" >&2
    cat "$file" >&2
    exit 1
  fi
}

choose_local_port() {
  python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
}

stop_port_forward() {
  if [[ -n "$PORT_FORWARD_PID" ]] && kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then
    kill "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
    wait "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
  fi
  PORT_FORWARD_PID=""
  PORT_FORWARD_LOG=""
  BASE_URL=""
  LOCAL_PORT=""
}

start_port_forward() {
  local target=$1
  local remote_port=$2

  stop_port_forward

  LOCAL_PORT="$(choose_local_port)"
  PORT_FORWARD_LOG="$TMP_DIR/$(printf '%s' "$target" | tr '/:' '--').port-forward.log"

  kubectl_ns port-forward "$target" "${LOCAL_PORT}:${remote_port}" >"$PORT_FORWARD_LOG" 2>&1 &
  PORT_FORWARD_PID=$!
  BASE_URL="http://127.0.0.1:${LOCAL_PORT}"

  wait_for_http_ok "$BASE_URL/healthz" 60
  log "port-forward ready: $target -> $BASE_URL"
}

start_testimony_log_capture() {
  TESTIMONY_LOG_FILE="$TMP_DIR/testimony.log"
  : >"$TESTIMONY_LOG_FILE"
  kubectl_ns logs -f --timestamps "$TESTIMONY_POD" >"$TESTIMONY_LOG_FILE" 2>&1 &
  TESTIMONY_LOG_PID=$!
}

stop_testimony_log_capture() {
  if [[ -n "$TESTIMONY_LOG_PID" ]] && kill -0 "$TESTIMONY_LOG_PID" >/dev/null 2>&1; then
    kill "$TESTIMONY_LOG_PID" >/dev/null 2>&1 || true
    wait "$TESTIMONY_LOG_PID" >/dev/null 2>&1 || true
  fi
  TESTIMONY_LOG_PID=""
}

wait_for_http_ok() {
  local url=$1
  local timeout_seconds=$2
  local start
  start=$(date +%s)

  while true; do
    if curl --fail --silent "$url" >/dev/null 2>&1; then
      return 0
    fi

    if [[ -n "$PORT_FORWARD_PID" ]] && ! kill -0 "$PORT_FORWARD_PID" >/dev/null 2>&1; then
      echo "port-forward exited before $url became ready" >&2
      if [[ -n "$PORT_FORWARD_LOG" && -f "$PORT_FORWARD_LOG" ]]; then
        cat "$PORT_FORWARD_LOG" >&2
      fi
      return 1
    fi

    if (( $(date +%s) - start >= timeout_seconds )); then
      echo "timed out waiting for $url" >&2
      if [[ -n "$PORT_FORWARD_LOG" && -f "$PORT_FORWARD_LOG" ]]; then
        cat "$PORT_FORWARD_LOG" >&2
      fi
      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done
}

wait_for_report_ready() {
  local report_id=$1
  local start
  start=$(date +%s)

  while true; do
    LAST_PROJECT_STATUS=$(curl --silent --output "$TMP_DIR/project-poll.html" --write-out '%{http_code}' "$BASE_URL/projects/$PROJECT_SLUG")
    LAST_REPORT_STATUS=$(curl --silent --output "$TMP_DIR/report-poll.html" --write-out '%{http_code}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id/")

    if [[ "$LAST_PROJECT_STATUS" == "200" ]] && [[ "$LAST_REPORT_STATUS" == "200" ]] && grep -Fq "/reports/$PROJECT_SLUG/$report_id/" "$TMP_DIR/project-poll.html"; then
      log "report became browseable: report_id=$report_id project_status=$LAST_PROJECT_STATUS report_status=$LAST_REPORT_STATUS"
      return 0
    fi

    if (( $(date +%s) - start >= REPORT_TIMEOUT_SECONDS )); then
      echo "timed out waiting for report $report_id to become browseable (project_status=$LAST_PROJECT_STATUS report_status=$LAST_REPORT_STATUS)" >&2
      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done
}

wait_for_drain_response() {
  local start
  start=$(date +%s)

  while true; do
    LAST_DRAIN_STATUS=$(curl --silent --show-error --output "$TMP_DIR/drain-readyz.json" --write-out '%{http_code}' "$BASE_URL/readyz" || true)
    if [[ -f "$TMP_DIR/drain-readyz.json" ]]; then
      LAST_DRAIN_BODY=$(cat "$TMP_DIR/drain-readyz.json")
    else
      LAST_DRAIN_BODY=""
    fi

    if [[ "$LAST_DRAIN_STATUS" == "503" ]]; then
      if [[ -n "$LAST_DRAIN_BODY" ]] && printf '%s' "$LAST_DRAIN_BODY" | grep -F 'draining' >/dev/null 2>&1; then
        log "observed draining readiness response"
        return 0
      fi
    fi

    if (( $(date +%s) - start >= DRAIN_TIMEOUT_SECONDS )); then
      echo "timed out waiting for /readyz to report draining" >&2
      return 1
    fi

    sleep 1
  done
}

wait_for_log_fragment() {
  local fragment=$1
  local timeout_seconds=$2
  local start
  start=$(date +%s)

  while true; do
    if [[ -f "$TESTIMONY_LOG_FILE" ]] && grep -F "$fragment" "$TESTIMONY_LOG_FILE" >/dev/null 2>&1; then
      return 0
    fi

    if (( $(date +%s) - start >= timeout_seconds )); then
      echo "timed out waiting for log fragment: $fragment" >&2
      if [[ -f "$TESTIMONY_LOG_FILE" ]]; then
        cat "$TESTIMONY_LOG_FILE" >&2
      fi
      return 1
    fi

    sleep 1
  done
}

create_archive() {
  local results_dir="$TMP_DIR/allure-results"
  UPLOAD_ARCHIVE="$TMP_DIR/allure-results-helm-runtime.zip"
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
    "historyId": "helm-runtime-history",
    "testCaseId": "helm-runtime-case",
    "fullName": "helm.runtime.test",
    "name": "helm runtime test",
    "status": "passed",
    "stage": "finished",
    "start": 1710000000000,
    "stop": 1710000001000,
    "labels": [
        {"name": "suite", "value": "helm-runtime"},
        {"name": "package", "value": "smoke"},
    ],
}
result_path = results_dir / "22222222-2222-2222-2222-222222222222-result.json"
result_path.write_text(json.dumps(result), encoding="utf-8")
with zipfile.ZipFile(archive_path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
    archive.write(result_path, arcname=result_path.name)
PY
}

upload_archive() {
  UPLOAD_HEADERS="$TMP_DIR/upload.headers"
  UPLOAD_BODY="$TMP_DIR/upload.json"

  LAST_UPLOAD_STATUS=$(curl --silent --show-error \
    --output "$UPLOAD_BODY" \
    --dump-header "$UPLOAD_HEADERS" \
    --write-out '%{http_code}' \
    --request POST \
    --header 'Content-Type: application/zip' \
    --header 'Content-Disposition: attachment; filename="allure-results-helm-runtime.zip"' \
    --data-binary "@$UPLOAD_ARCHIVE" \
    "$BASE_URL/api/v1/projects/$PROJECT_SLUG/upload")
  LAST_UPLOAD_BODY=$(cat "$UPLOAD_BODY")

  if [[ "$LAST_UPLOAD_STATUS" != "202" ]]; then
    echo "upload returned unexpected status: $LAST_UPLOAD_STATUS" >&2
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

fetch_and_assert_pages() {
  local report_id=$1
  ROOT_BODY="$TMP_DIR/root.html"
  PROJECT_BODY="$TMP_DIR/project.html"
  REPORT_BODY="$TMP_DIR/report.html"
  SUMMARY_BODY="$TMP_DIR/summary.json"

  LAST_HEALTH_STATUS=$(curl --silent --output "$TMP_DIR/healthz.json" --write-out '%{http_code}' "$BASE_URL/healthz")
  LAST_READY_STATUS=$(curl --silent --output "$TMP_DIR/readyz.json" --write-out '%{http_code}' "$BASE_URL/readyz")
  LAST_HEALTH_BODY=$(cat "$TMP_DIR/healthz.json")
  LAST_READY_BODY=$(cat "$TMP_DIR/readyz.json")

  if [[ "$LAST_HEALTH_STATUS" != "200" ]]; then
    echo "/healthz returned unexpected status: $LAST_HEALTH_STATUS" >&2
    exit 1
  fi
  if [[ "$LAST_READY_STATUS" != "200" ]]; then
    echo "/readyz returned unexpected status: $LAST_READY_STATUS" >&2
    exit 1
  fi

  assert_json_field "$TMP_DIR/healthz.json" status ok "/healthz should report status=ok"
  assert_json_field "$TMP_DIR/readyz.json" status ready "/readyz should report status=ready"

  local root_status
  local project_status
  local report_status
  local summary_status

  root_status=$(curl --silent --output "$ROOT_BODY" --write-out '%{http_code}' "$BASE_URL/")
  if [[ "$root_status" != "200" ]]; then
    echo "root page returned unexpected status: $root_status" >&2
    exit 1
  fi

  project_status=$(curl --silent --output "$PROJECT_BODY" --write-out '%{http_code}' "$BASE_URL/projects/$PROJECT_SLUG")
  LAST_PROJECT_STATUS="$project_status"
  if [[ "$project_status" != "200" ]]; then
    echo "project page returned unexpected status: $project_status" >&2
    exit 1
  fi

  LAST_REDIRECT_LOCATION=$(curl --silent --show-error --output /dev/null --dump-header "$TMP_DIR/report-redirect.headers" --write-out '%{redirect_url}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id")
  LAST_REDIRECT_STATUS=$(python3 - <<'PY' "$TMP_DIR/report-redirect.headers"
import sys
status = ""
for line in open(sys.argv[1], encoding='utf-8').read().splitlines():
    if line.startswith('HTTP/'):
        status = line.split()[1]
        break
print(status)
PY
)
  if [[ "$LAST_REDIRECT_STATUS" != "302" ]]; then
    echo "bare report route returned unexpected status: $LAST_REDIRECT_STATUS" >&2
    exit 1
  fi

  report_status=$(curl --silent --output "$REPORT_BODY" --write-out '%{http_code}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id/")
  LAST_REPORT_STATUS="$report_status"
  if [[ "$report_status" != "200" ]]; then
    echo "report root returned unexpected status: $report_status" >&2
    exit 1
  fi

  summary_status=$(curl --silent --output "$SUMMARY_BODY" --write-out '%{http_code}' "$BASE_URL/reports/$PROJECT_SLUG/$report_id/widgets/summary.json")
  LAST_SUMMARY_STATUS="$summary_status"
  if [[ "$summary_status" != "200" ]]; then
    echo "report summary returned unexpected status: $summary_status" >&2
    exit 1
  fi

  assert_contains "$(cat "$ROOT_BODY")" "Browse generated reports" "root page should expose the browse UI"
  assert_contains "$(cat "$ROOT_BODY")" "$PROJECT_SLUG" "root page should list the uploaded project"
  assert_contains "$(cat "$PROJECT_BODY")" "$report_id" "project page should list the uploaded report"
  assert_contains "$(cat "$PROJECT_BODY")" "/reports/$PROJECT_SLUG/$report_id/" "project page should link to the ready report"
  assert_contains "$LAST_REDIRECT_LOCATION" "/reports/$PROJECT_SLUG/$report_id/" "bare report route should redirect to the trailing slash URL"
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

default_storage_class() {
  kubectl get storageclass -o json | python3 -c 'import json, sys
items = json.load(sys.stdin).get("items", [])
for item in items:
    annotations = item.get("metadata", {}).get("annotations", {})
    if annotations.get("storageclass.kubernetes.io/is-default-class") == "true" or annotations.get("storageclass.beta.kubernetes.io/is-default-class") == "true":
        print(item["metadata"]["name"])
        break'
}

load_image_into_cluster_if_needed() {
  local context
  context="$(kubectl config current-context)"

  if [[ "$context" == kind-* ]] && command -v kind >/dev/null 2>&1; then
    log "loading $IMAGE_REF into kind cluster ${context#kind-}"
    kind load docker-image "$IMAGE_REF" --name "${context#kind-}" >/dev/null
    return 0
  fi

  if [[ "$context" == k3d-* ]] && command -v k3d >/dev/null 2>&1; then
    log "importing $IMAGE_REF into k3d cluster ${context#k3d-}"
    k3d image import "$IMAGE_REF" -c "${context#k3d-}" >/dev/null
    return 0
  fi

  if [[ "$context" == "minikube" ]] && command -v minikube >/dev/null 2>&1; then
    log "loading $IMAGE_REF into minikube"
    minikube image load "$IMAGE_REF" >/dev/null
    return 0
  fi

  log "no explicit local-image loader for kubectl context $context; relying on the cluster runtime to see $IMAGE_REF"
}

ensure_image() {
  if [[ "$FORCE_IMAGE_BUILD" == "1" ]] || ! docker image inspect "$IMAGE_REF" >/dev/null 2>&1; then
    log "building runtime image $IMAGE_REF"
    docker build -t "$IMAGE_REF" "$ROOT_DIR" >/dev/null
  else
    log "reusing local runtime image $IMAGE_REF"
  fi

  load_image_into_cluster_if_needed
}

create_namespace() {
  NAMESPACE="testimony-helm-runtime-$(date +%s)-$RANDOM"
  kubectl create namespace "$NAMESPACE" >/dev/null
  log "created namespace $NAMESPACE"
}

install_minio() {
  log "deploying in-cluster MinIO backend"
  kubectl_ns apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${MINIO_POD_NAME}
  labels:
    app.kubernetes.io/name: minio
    app.kubernetes.io/instance: ${MINIO_POD_NAME}
spec:
  containers:
    - name: minio
      image: ${MINIO_IMAGE}
      args:
        - server
        - /data
        - --address
        - :9000
        - --console-address
        - :9001
      env:
        - name: MINIO_ROOT_USER
          value: minioadmin
        - name: MINIO_ROOT_PASSWORD
          value: minioadmin
      ports:
        - name: s3
          containerPort: 9000
        - name: console
          containerPort: 9001
      readinessProbe:
        httpGet:
          path: /minio/health/ready
          port: s3
        initialDelaySeconds: 2
        periodSeconds: 2
        timeoutSeconds: 2
        failureThreshold: 30
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: ${MINIO_SERVICE_NAME}
spec:
  selector:
    app.kubernetes.io/name: minio
    app.kubernetes.io/instance: ${MINIO_POD_NAME}
  ports:
    - name: s3
      port: 9000
      targetPort: s3
YAML

  kubectl_ns wait --for=condition=Ready "pod/${MINIO_POD_NAME}" --timeout=180s >/dev/null
  log "MinIO ready"
}

create_overrides_file() {
  local overrides_file=$1
  local endpoint="http://${MINIO_ENDPOINT_HOST}:${MINIO_PORT}"

  cat >"$overrides_file" <<YAML
image:
  repository: ${IMAGE_REPOSITORY}
  tag: ${IMAGE_TAG}
  pullPolicy: IfNotPresent
runtime:
  logLevel: debug
  s3:
    endpoint: ${endpoint}
    region: us-east-1
    bucket: testimony-runtime
    usePathStyle: true
  auth:
    enabled: false
    requireViewer: false
  shutdown:
    readyzDrainDelay: 10s
    timeout: 15s
secrets:
  s3AccessKeyId: minioadmin
  s3SecretAccessKey: minioadmin
  authAPIKey: ""
YAML

  if [[ -n "$DEFAULT_STORAGE_CLASS" ]]; then
    cat >>"$overrides_file" <<YAML
persistence:
  storageClassName: ${DEFAULT_STORAGE_CLASS}
YAML
  fi
}

install_chart() {
  local overrides_file=$1
  log "installing Helm release $RELEASE_NAME"
  helm upgrade --install "$RELEASE_NAME" "$CHART_DIR" --namespace "$NAMESPACE" --wait --timeout 5m -f "$overrides_file" >/dev/null

  SERVICE_NAME="$(kubectl_ns get service -l app.kubernetes.io/instance="$RELEASE_NAME",app.kubernetes.io/name=testimony -o jsonpath='{.items[0].metadata.name}')"
  TESTIMONY_POD="$(kubectl_ns get pod -l app.kubernetes.io/instance="$RELEASE_NAME",app.kubernetes.io/name=testimony -o jsonpath='{.items[0].metadata.name}')"

  if [[ -z "$SERVICE_NAME" || -z "$TESTIMONY_POD" ]]; then
    echo "failed to discover Helm service/pod for release $RELEASE_NAME" >&2
    exit 1
  fi

  kubectl_ns wait --for=condition=Ready pod -l app.kubernetes.io/instance="$RELEASE_NAME",app.kubernetes.io/name=testimony --timeout="${STARTUP_TIMEOUT_SECONDS}s" >/dev/null
  log "Helm release ready: service=$SERVICE_NAME pod=$TESTIMONY_POD"
  kubectl_ns get pods -o wide
}

print_failure_diagnostics() {
  log "failure diagnostics"
  if [[ -n "$NAMESPACE" ]]; then
    printf 'namespace=%s release=%s\n' "$NAMESPACE" "$RELEASE_NAME" >&2
    if [[ -n "$LAST_REPORT_ID" ]]; then
      printf 'report_id=%s upload_status=%s upload_response_status=%s archive_object_key=%s\n' "$LAST_REPORT_ID" "$LAST_UPLOAD_STATUS" "$LAST_UPLOAD_RESPONSE_STATUS" "$LAST_ARCHIVE_OBJECT_KEY" >&2
    fi
    printf 'healthz_status=%s readyz_status=%s project_status=%s report_status=%s summary_status=%s drain_status=%s\n' \
      "$LAST_HEALTH_STATUS" "$LAST_READY_STATUS" "$LAST_PROJECT_STATUS" "$LAST_REPORT_STATUS" "$LAST_SUMMARY_STATUS" "$LAST_DRAIN_STATUS" >&2
    if [[ -n "$LAST_DRAIN_BODY" ]]; then
      printf 'drain_body=%s\n' "$LAST_DRAIN_BODY" >&2
    fi

    helm status "$RELEASE_NAME" --namespace "$NAMESPACE" >&2 || true
    kubectl_ns get pods,svc,pvc >&2 || true
    kubectl_ns get events --sort-by=.lastTimestamp | tail -n 40 >&2 || true

    if [[ -n "$TESTIMONY_POD" ]]; then
      kubectl_ns describe pod "$TESTIMONY_POD" >&2 || true
      kubectl_ns logs "$TESTIMONY_POD" --tail=200 >&2 || true
    fi

    kubectl_ns describe pod "$MINIO_POD_NAME" >&2 || true
    kubectl_ns logs "$MINIO_POD_NAME" --tail=200 >&2 || true
  fi

  if [[ -n "$PORT_FORWARD_LOG" && -f "$PORT_FORWARD_LOG" ]]; then
    log "port-forward log"
    cat "$PORT_FORWARD_LOG" >&2 || true
  fi

  if [[ -n "$TESTIMONY_LOG_FILE" && -f "$TESTIMONY_LOG_FILE" ]]; then
    log "captured testimony log"
    cat "$TESTIMONY_LOG_FILE" >&2 || true
  fi
}

cleanup() {
  local exit_code=$?

  stop_port_forward
  stop_testimony_log_capture

  if [[ $exit_code -ne 0 ]]; then
    print_failure_diagnostics
  fi

  if [[ -n "$NAMESPACE" && "$KEEP_NAMESPACE" != "1" ]]; then
    log "cleaning up namespace $NAMESPACE"
    kubectl delete namespace "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    kubectl wait --for=delete "namespace/$NAMESPACE" --timeout=120s >/dev/null 2>&1 || true
  elif [[ -n "$NAMESPACE" ]]; then
    log "KEEP_NAMESPACE=1 set; preserving namespace $NAMESPACE"
  fi

  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi

  trap - EXIT INT TERM
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

main() {
  require_tool kubectl
  require_tool helm
  require_tool docker
  require_tool curl
  require_tool python3

  if [[ ! -f "$CHART_DIR/Chart.yaml" ]]; then
    echo "missing chart definition: $CHART_DIR/Chart.yaml" >&2
    exit 1
  fi

  TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/verify-helm-runtime.XXXXXX")"
  DEFAULT_STORAGE_CLASS="$(default_storage_class || true)"

  log "kubectl context: $(kubectl config current-context)"
  if [[ -n "$DEFAULT_STORAGE_CLASS" ]]; then
    log "default storage class: $DEFAULT_STORAGE_CLASS"
  else
    log "no default storage class detected; PVC binding will rely on cluster defaults"
  fi

  ensure_image
  create_namespace
  install_minio

  local overrides_file="$TMP_DIR/verify-values.yaml"
  create_overrides_file "$overrides_file"
  install_chart "$overrides_file"
  start_testimony_log_capture

  start_port_forward "service/$SERVICE_NAME" 80
  create_archive

  log "checking live health endpoints"
  LAST_HEALTH_STATUS=$(curl --silent --output "$TMP_DIR/healthz.json" --write-out '%{http_code}' "$BASE_URL/healthz")
  LAST_READY_STATUS=$(curl --silent --output "$TMP_DIR/readyz.json" --write-out '%{http_code}' "$BASE_URL/readyz")
  LAST_HEALTH_BODY=$(cat "$TMP_DIR/healthz.json")
  LAST_READY_BODY=$(cat "$TMP_DIR/readyz.json")
  if [[ "$LAST_HEALTH_STATUS" != "200" ]]; then
    echo "/healthz returned unexpected status: $LAST_HEALTH_STATUS" >&2
    exit 1
  fi
  if [[ "$LAST_READY_STATUS" != "200" ]]; then
    echo "/readyz returned unexpected status: $LAST_READY_STATUS" >&2
    exit 1
  fi
  assert_json_field "$TMP_DIR/healthz.json" status ok "/healthz should report status=ok"
  assert_json_field "$TMP_DIR/readyz.json" status ready "/readyz should report status=ready"

  log "uploading synthesized Allure results archive"
  local upload_result
  upload_result="$(upload_archive)"
  LAST_REPORT_ID="$(printf '%s\n' "$upload_result" | sed -n '1p')"
  LAST_UPLOAD_RESPONSE_STATUS="$(printf '%s\n' "$upload_result" | sed -n '2p')"
  LAST_ARCHIVE_OBJECT_KEY="$(printf '%s\n' "$upload_result" | sed -n '3p')"
  log "upload accepted: report_id=$LAST_REPORT_ID status=$LAST_UPLOAD_RESPONSE_STATUS archive_object_key=$LAST_ARCHIVE_OBJECT_KEY"

  wait_for_report_ready "$LAST_REPORT_ID"
  fetch_and_assert_pages "$LAST_REPORT_ID"

  stop_port_forward
  start_port_forward "pod/$TESTIMONY_POD" 8080

  log "deleting pod $TESTIMONY_POD to verify drain/shutdown behavior"
  kubectl_ns delete pod "$TESTIMONY_POD" --wait=false >/dev/null
  wait_for_drain_response
  wait_for_log_fragment "shutdown requested" 40
  wait_for_log_fragment "shutdown drain started" 40
  wait_for_log_fragment "shutdown complete" 40

  log "helm runtime verification passed"
}

main "$@"
