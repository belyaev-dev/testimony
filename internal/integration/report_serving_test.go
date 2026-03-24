package integration_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
	servepkg "github.com/testimony-dev/testimony/internal/serve"
	"github.com/testimony-dev/testimony/internal/storage"
)

func TestReportServingUIAndProxy(t *testing.T) {
	cliPath := writeIntegrationCLI(t, "allure-serving.sh", `#!/bin/sh
set -eu

if [ "$1" != "generate" ]; then
  echo "unexpected command: $*" >&2
  exit 2
fi

RESULTS_DIR="$2"
if [ "$3" != "--clean" ] || [ "$4" != "-o" ]; then
  echo "unexpected args: $*" >&2
  exit 3
fi
OUTPUT_DIR="$5"
RESULT_FILE="$RESULTS_DIR/widgets-result.json"
if [ ! -f "$RESULT_FILE" ]; then
  echo "missing widgets-result.json" >&2
  exit 4
fi

RUN_ID=$(sed -n 's/.*"run":"\([^"]*\)".*/\1/p' "$RESULT_FILE")
if [ -z "$RUN_ID" ]; then
  echo "missing run in widgets-result.json" >&2
  exit 5
fi

mkdir -p "$OUTPUT_DIR/assets"
printf '<!doctype html><html><head><link rel="stylesheet" href="./assets/app.css"></head><body><h1>allure-ui %s</h1><script src="./assets/app.js"></script></body></html>\n' "$RUN_ID" > "$OUTPUT_DIR/index.html"
printf 'body{font-family:sans-serif;background:#f8fbff;}\n' > "$OUTPUT_DIR/assets/app.css"
printf 'console.log("run:%s")\n' "$RUN_ID" > "$OUTPUT_DIR/assets/app.js"
`)

	harness := newGenerationPipelineHarnessWithOptions(t, config.GenerateVariantAllure2, cliPath, generationPipelineHarnessOptions{
		registerRoutes: func(r chi.Router, sqliteStore *db.SQLiteStore, s3Store *storage.S3Store) {
			ui, err := servepkg.NewUI(nil, sqliteStore)
			if err != nil {
				t.Fatalf("NewUI() error = %v", err)
			}
			proxy, err := servepkg.NewReportProxy(nil, sqliteStore, s3Store)
			if err != nil {
				t.Fatalf("NewReportProxy() error = %v", err)
			}
			ui.RegisterRoutes(r)
			proxy.RegisterRoutes(r)
		},
	})

	payload := buildServingZipArchive(t, map[string]string{
		"widgets-result.json": `{"run":"ui-run-1"}`,
	})
	response := harness.uploadArchiveBytes(t, "widgets-ui", "allure-results-ui.zip", payload)
	report := harness.waitForTerminalReport(t, response.ReportID)
	assertReadyReport(t, report, response.ProjectSlug)

	rootResp, err := http.Get(harness.baseURL + "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	defer rootResp.Body.Close()
	if got, want := rootResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET / status = %d, want %d", got, want)
	}
	rootBody, err := io.ReadAll(rootResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(/) error = %v", err)
	}
	for _, fragment := range []string{"Browse generated reports", "/projects/widgets-ui", "widgets-ui", "Ready reports"} {
		if !strings.Contains(string(rootBody), fragment) {
			t.Fatalf("root page missing %q in %q", fragment, string(rootBody))
		}
	}

	projectResp, err := http.Get(harness.baseURL + "/projects/widgets-ui")
	if err != nil {
		t.Fatalf("GET /projects/widgets-ui error = %v", err)
	}
	defer projectResp.Body.Close()
	if got, want := projectResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("project page status = %d, want %d", got, want)
	}
	projectBody, err := io.ReadAll(projectResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(project page) error = %v", err)
	}
	readyLink := "/reports/widgets-ui/" + report.ID + "/"
	for _, fragment := range []string{"Project history", report.ID, "ready", readyLink} {
		if !strings.Contains(string(projectBody), fragment) {
			t.Fatalf("project page missing %q in %q", fragment, string(projectBody))
		}
	}

	reportResp, err := http.Get(harness.baseURL + readyLink)
	if err != nil {
		t.Fatalf("GET report root error = %v", err)
	}
	defer reportResp.Body.Close()
	if got, want := reportResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("report root status = %d, want %d", got, want)
	}
	reportBody, err := io.ReadAll(reportResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(report root) error = %v", err)
	}
	for _, fragment := range []string{"allure-ui ui-run-1", "./assets/app.js", "./assets/app.css"} {
		if !strings.Contains(string(reportBody), fragment) {
			t.Fatalf("report root missing %q in %q", fragment, string(reportBody))
		}
	}

	assetResp, err := http.Get(harness.baseURL + readyLink + "assets/app.js")
	if err != nil {
		t.Fatalf("GET nested asset error = %v", err)
	}
	defer assetResp.Body.Close()
	if got, want := assetResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("asset status = %d, want %d", got, want)
	}
	assetBody, err := io.ReadAll(assetResp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(asset) error = %v", err)
	}
	if got, want := string(assetBody), "console.log(\"run:ui-run-1\")\n"; got != want {
		t.Fatalf("asset body = %q, want %q", got, want)
	}
	if got := assetResp.Header.Get("Content-Type"); got == "" {
		t.Fatal("asset content-type = empty, want proxy metadata passthrough")
	}
}

func (h *generationPipelineHarness) uploadArchiveBytes(t *testing.T, projectSlug, filename string, payload []byte) uploadResponseBody {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/v1/projects/"+projectSlug+"/upload", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request error = %v", err)
	}
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusAccepted; got != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status = %d, want %d, body=%s", got, want, body)
	}

	var response uploadResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}
	if got, want := response.ProjectSlug, projectSlug; got != want {
		t.Fatalf("response.ProjectSlug = %q, want %q", got, want)
	}
	if got, want := response.Status, db.ReportStatusPending; got != want {
		t.Fatalf("response.Status = %q, want %q", got, want)
	}
	return response
}

func buildServingZipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, payload := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
		if _, err := entry.Write([]byte(payload)); err != nil {
			t.Fatalf("Write(%q) error = %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip writer close error = %v", err)
	}
	return buf.Bytes()
}
