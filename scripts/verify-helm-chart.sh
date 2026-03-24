#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="$ROOT_DIR/chart"
TMP_DIR=""

log() {
  printf '[verify-helm-chart] %s\n' "$*"
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required tool: $1" >&2
    exit 1
  }
}

cleanup() {
  local exit_code=$?
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi
  trap - EXIT
  exit "$exit_code"
}
trap cleanup EXIT

assert_file_contains() {
  local file=$1
  local needle=$2
  local message=$3
  if ! grep -F -- "$needle" "$file" >/dev/null 2>&1; then
    echo "assertion failed: $message" >&2
    echo "file: $file" >&2
    echo "missing text: $needle" >&2
    exit 1
  fi
}

assert_file_not_contains() {
  local file=$1
  local needle=$2
  local message=$3
  if grep -F -- "$needle" "$file" >/dev/null 2>&1; then
    echo "assertion failed: $message" >&2
    echo "file: $file" >&2
    echo "unexpected text: $needle" >&2
    exit 1
  fi
}

main() {
  require_tool helm
  require_tool grep

  if [[ ! -f "$CHART_DIR/Chart.yaml" ]]; then
    echo "missing chart definition: $CHART_DIR/Chart.yaml" >&2
    exit 1
  fi

  TMP_DIR="$(mktemp -d)"

  local overrides_file="$TMP_DIR/verify-values.yaml"
  local no_storage_class_file="$TMP_DIR/verify-values-no-storage-class.yaml"
  local render_dir="$TMP_DIR/rendered"
  local render_dir_no_sc="$TMP_DIR/rendered-no-storage-class"

  cat >"$overrides_file" <<'YAML'
image:
  repository: ghcr.io/testimony-dev/testimony
  tag: verify-chart
service:
  port: 8080
runtime:
  logLevel: debug
  server:
    host: 0.0.0.0
    port: 8080
    readTimeout: 20s
    writeTimeout: 40s
    idleTimeout: 90s
  s3:
    endpoint: https://s3.example.internal
    region: eu-central-1
    bucket: testimony-prod
    usePathStyle: false
  sqlite:
    path: /var/lib/testimony/data/prod.sqlite
    busyTimeout: 9s
  auth:
    enabled: true
    requireViewer: true
  retention:
    days: 30
    cleanupInterval: 45m
  generate:
    variant: allure2
    cliPath: /usr/local/bin/allure
    timeout: 4m
    maxConcurrency: 4
    historyDepth: 12
  tempDir: /var/lib/testimony/tmp
  shutdown:
    readyzDrainDelay: 12s
    timeout: 55s
resources:
  requests:
    cpu: 250m
    memory: 384Mi
  limits:
    cpu: 1000m
    memory: 1Gi
persistence:
  enabled: true
  mountPath: /var/lib/testimony
  size: 20Gi
  storageClassName: fast-ssd
secrets:
  s3AccessKeyId: example-access-key
  s3SecretAccessKey: example-secret-key
  authAPIKey: example-auth-key
YAML

  cat >"$no_storage_class_file" <<'YAML'
persistence:
  enabled: true
  storageClassName: ""
YAML

  log "linting chart"
  helm lint "$CHART_DIR" -f "$overrides_file" >/dev/null

  log "rendering chart with explicit overrides"
  helm template testimony "$CHART_DIR" -f "$overrides_file" --output-dir "$render_dir" >/dev/null

  local deployment_file="$render_dir/testimony/templates/deployment.yaml"
  local service_file="$render_dir/testimony/templates/service.yaml"
  local secret_file="$render_dir/testimony/templates/secret.yaml"
  local pvc_file="$render_dir/testimony/templates/persistentvolumeclaim.yaml"

  for file in "$deployment_file" "$service_file" "$secret_file" "$pvc_file"; do
    [[ -f "$file" ]] || {
      echo "expected rendered file missing: $file" >&2
      exit 1
    }
  done

  assert_file_contains "$deployment_file" 'image: "ghcr.io/testimony-dev/testimony:verify-chart"' 'deployment should use the overridden image tag'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_S3_ENDPOINT' 'deployment should expose the S3 endpoint env var'
  assert_file_contains "$deployment_file" 'value: "https://s3.example.internal"' 'deployment should render the overridden S3 endpoint'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_S3_ACCESS_KEY_ID' 'deployment should wire S3 access keys from a secret'
  assert_file_contains "$deployment_file" 'key: s3-access-key-id' 'deployment should reference the S3 access key secret entry'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_S3_SECRET_ACCESS_KEY' 'deployment should wire S3 secret keys from a secret'
  assert_file_contains "$deployment_file" 'key: s3-secret-access-key' 'deployment should reference the S3 secret key entry'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_AUTH_ENABLED' 'deployment should expose auth enablement'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_AUTH_API_KEY' 'deployment should wire the auth API key secret ref'
  assert_file_contains "$deployment_file" 'key: auth-api-key' 'deployment should reference the auth API key secret entry'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_AUTH_REQUIRE_VIEWER' 'deployment should expose the viewer auth toggle'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_SQLITE_PATH' 'deployment should expose SQLite path configuration'
  assert_file_contains "$deployment_file" 'value: "/var/lib/testimony/data/prod.sqlite"' 'deployment should render the overridden SQLite path'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_GENERATE_MAX_CONCURRENCY' 'deployment should expose generation concurrency'
  assert_file_contains "$deployment_file" 'value: "4"' 'deployment should render the overridden concurrency'
  assert_file_contains "$deployment_file" 'name: TESTIMONY_GENERATE_HISTORY_DEPTH' 'deployment should expose history depth'
  assert_file_contains "$deployment_file" 'value: "12"' 'deployment should render the overridden history depth'
  assert_file_contains "$deployment_file" 'path: "/healthz"' 'deployment should wire the liveness probe path'
  assert_file_contains "$deployment_file" 'path: "/readyz"' 'deployment should wire the readiness probe path'
  assert_file_contains "$deployment_file" 'cpu: 250m' 'deployment should render CPU requests'
  assert_file_contains "$deployment_file" 'memory: 384Mi' 'deployment should render memory requests'
  assert_file_contains "$deployment_file" 'cpu: 1000m' 'deployment should render CPU limits'
  assert_file_contains "$deployment_file" 'memory: 1Gi' 'deployment should render memory limits'
  assert_file_contains "$deployment_file" 'mountPath: "/var/lib/testimony"' 'deployment should mount persistent storage'
  assert_file_contains "$service_file" 'kind: Service' 'service manifest should render'
  assert_file_contains "$service_file" 'port: 8080' 'service should expose the overridden port'
  assert_file_contains "$service_file" 'targetPort: http' 'service should target the HTTP container port'
  assert_file_contains "$secret_file" 'kind: Secret' 'secret manifest should render'
  assert_file_contains "$secret_file" 's3-access-key-id:' 'secret should include the S3 access key entry'
  assert_file_contains "$secret_file" 's3-secret-access-key:' 'secret should include the S3 secret key entry'
  assert_file_contains "$secret_file" 'auth-api-key:' 'secret should include the auth API key entry'
  assert_file_contains "$pvc_file" 'kind: PersistentVolumeClaim' 'pvc manifest should render when persistence is enabled'
  assert_file_contains "$pvc_file" 'storage: "20Gi"' 'pvc should render the overridden size'
  assert_file_contains "$pvc_file" 'storageClassName: "fast-ssd"' 'pvc should render the explicit storageClassName'

  log "rendering chart without storageClassName"
  helm template testimony "$CHART_DIR" -f "$no_storage_class_file" --output-dir "$render_dir_no_sc" >/dev/null
  local pvc_file_no_sc="$render_dir_no_sc/testimony/templates/persistentvolumeclaim.yaml"
  [[ -f "$pvc_file_no_sc" ]] || {
    echo "expected rendered pvc missing: $pvc_file_no_sc" >&2
    exit 1
  }
  assert_file_not_contains "$pvc_file_no_sc" 'storageClassName:' 'pvc should omit storageClassName when not set'

  log "helm chart verification passed"
}

main "$@"
