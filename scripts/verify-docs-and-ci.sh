#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
README_FILE="$ROOT_DIR/README.md"
CONTRIBUTING_FILE="$ROOT_DIR/CONTRIBUTING.md"
ARCHITECTURE_FILE="$ROOT_DIR/docs/architecture.md"
CONFIG_FILE="$ROOT_DIR/docs/configuration.md"
CI_README_FILE="$ROOT_DIR/examples/ci/README.md"
GITHUB_CI_FILE="$ROOT_DIR/examples/ci/github-actions-upload.yml"
GITLAB_CI_FILE="$ROOT_DIR/examples/ci/gitlab-ci-upload.yml"

log() {
  printf '[verify-docs-and-ci] %s\n' "$*"
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required tool: $1" >&2
    exit 1
  }
}

assert_non_empty_file() {
  local file=$1
  if [[ ! -s "$file" ]]; then
    echo "required file is missing or empty: $file" >&2
    exit 1
  fi
}

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

assert_repo_has_no_placeholders() {
  if rg -n 'TODO|TBD' \
    "$README_FILE" \
    "$CONTRIBUTING_FILE" \
    "$ARCHITECTURE_FILE" \
    "$CONFIG_FILE" \
    "$ROOT_DIR/examples/ci"; then
    echo 'found TODO/TBD placeholder text in docs or CI examples' >&2
    exit 1
  fi
}

parse_ci_yaml() {
  ruby -e 'require "yaml"; ARGV.each { |path| YAML.load_file(path) }' \
    "$GITHUB_CI_FILE" \
    "$GITLAB_CI_FILE"
}

main() {
  require_tool grep
  require_tool rg
  require_tool ruby

  log "checking required documentation and example files"
  assert_non_empty_file "$README_FILE"
  assert_non_empty_file "$CONTRIBUTING_FILE"
  assert_non_empty_file "$ARCHITECTURE_FILE"
  assert_non_empty_file "$CONFIG_FILE"
  assert_non_empty_file "$CI_README_FILE"
  assert_non_empty_file "$GITHUB_CI_FILE"
  assert_non_empty_file "$GITLAB_CI_FILE"

  log "checking README landing-page sections and links"
  assert_file_contains "$README_FILE" '## Features' 'README should include a Features section'
  assert_file_contains "$README_FILE" '## Docker Compose quickstart' 'README should include the Compose quickstart'
  assert_file_contains "$README_FILE" 'docker compose up --build -d' 'README should show the packaged startup command'
  assert_file_contains "$README_FILE" 'POST /api/v1/projects/{slug}/upload' 'README should mention the literal upload route in the quickstart'
  assert_file_contains "$README_FILE" 'bash scripts/verify-readme-quickstart.sh' 'README should point to the README quickstart verifier'
  assert_file_contains "$README_FILE" '/reports/{slug}/{reportID}/' 'README should mention the browseable report route'
  assert_file_contains "$README_FILE" 'under 5 minutes' 'README should mention the quickstart time budget'
  assert_file_contains "$README_FILE" '18080' 'README should mention the published local Testimony port'
  assert_file_contains "$README_FILE" '## Architecture overview' 'README should include an architecture overview'
  assert_file_contains "$README_FILE" '## Comparison' 'README should include a comparison section'
  assert_file_contains "$README_FILE" '## Configuration' 'README should include configuration guidance'
  assert_file_contains "$README_FILE" '[docs/architecture.md](docs/architecture.md)' 'README should link to the architecture guide'
  assert_file_contains "$README_FILE" '[docs/configuration.md](docs/configuration.md)' 'README should link to the configuration guide'
  assert_file_contains "$README_FILE" '[CONTRIBUTING.md](CONTRIBUTING.md)' 'README should link to CONTRIBUTING.md'
  assert_file_contains "$README_FILE" '[examples/ci/](examples/ci/)' 'README should link to the CI examples directory'

  log "checking configuration reference coverage"
  assert_file_contains "$CONFIG_FILE" '## Environment variable reference' 'configuration guide should include the env reference heading'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_SERVER_HOST' 'configuration guide should document server host'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_S3_ENDPOINT' 'configuration guide should document the S3 endpoint'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_SQLITE_PATH' 'configuration guide should document the SQLite path'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_AUTH_ENABLED' 'configuration guide should document auth enablement'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_GENERATE_VARIANT' 'configuration guide should document generation variant'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_RETENTION_DAYS' 'configuration guide should document retention days'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_TEMP_DIR' 'configuration guide should document the temp directory'
  assert_file_contains "$CONFIG_FILE" 'TESTIMONY_SHUTDOWN_TIMEOUT' 'configuration guide should document shutdown timeout'
  assert_file_contains "$CONFIG_FILE" 'docker-compose.yml' 'configuration guide should reference docker-compose.yml'
  assert_file_contains "$CONFIG_FILE" 'chart/values.yaml' 'configuration guide should reference chart/values.yaml'

  log "checking CI example explainer references"
  assert_file_contains "$CI_README_FILE" 'POST /api/v1/projects/{slug}/upload' 'CI README should mention the upload route'
  assert_file_contains "$CI_README_FILE" 'TESTIMONY_BASE_URL' 'CI README should document the base URL variable'
  assert_file_contains "$CI_README_FILE" 'TESTIMONY_PROJECT_SLUG' 'CI README should document the project slug variable'
  assert_file_contains "$CI_README_FILE" 'TESTIMONY_API_KEY' 'CI README should document the optional API key variable'

  log "parsing CI example YAML"
  parse_ci_yaml

  log "checking CI example upload contract strings"
  rg -n '/api/v1/projects/.+/upload|TESTIMONY_BASE_URL|TESTIMONY_PROJECT_SLUG|Authorization: Bearer|Content-Type: application/zip' \
    "$ROOT_DIR/examples/ci" >/dev/null

  log "checking for placeholder text"
  assert_repo_has_no_placeholders

  log "docs and CI verification passed"
}

main "$@"
