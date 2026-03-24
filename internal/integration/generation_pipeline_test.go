package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	testcontainers "github.com/testcontainers/testcontainers-go"
	miniomodule "github.com/testcontainers/testcontainers-go/modules/minio"
	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/generate"
	"github.com/testimony-dev/testimony/internal/server"
	"github.com/testimony-dev/testimony/internal/storage"
	"github.com/testimony-dev/testimony/internal/upload"
)

type uploadResponseBody struct {
	ReportID         string          `json:"report_id"`
	ProjectSlug      string          `json:"project_slug"`
	Status           db.ReportStatus `json:"status"`
	ArchiveFormat    string          `json:"archive_format"`
	ArchiveObjectKey string          `json:"archive_object_key"`
}

func TestGenerationPipelineAllure2(t *testing.T) {
	harness := newGenerationPipelineHarness(t, config.GenerateVariantAllure2, copyIntegrationCLI(t, "allure2"))
	assertStatus(t, harness.baseURL+"/readyz", http.StatusOK)

	report1 := harness.uploadAndWaitForReady(t, "history-allure2", "allure-results-history-1.zip")
	report2 := harness.uploadAndWaitForReady(t, "history-allure2", "allure-results-history-2.zip")
	report3 := harness.uploadAndWaitForReady(t, "history-allure2", "allure-results-history-3.zip")

	assertReportHistoryJSON(t, harness.downloadReportObject(t, report1, "history/history-trend.json"), []string{"history-1"})
	assertReportHistoryJSON(t, harness.downloadReportObject(t, report2, "history/history-trend.json"), []string{"history-1", "history-2"})
	assertReportHistoryJSON(t, harness.downloadReportObject(t, report3, "history/history-trend.json"), []string{"history-1", "history-2", "history-3"})

	assertObjectKeysContain(t, harness.listReportObjectKeys(t, report3),
		report3.GeneratedObjectKey,
		harness.reportObjectKey(report3, "history/history-trend.json"),
		harness.reportObjectKey(report3, "history/history.json"),
	)

	indexHTML := harness.downloadObjectString(t, report3.GeneratedObjectKey)
	if !strings.Contains(indexHTML, "allure2") || !strings.Contains(indexHTML, "history-3") {
		t.Fatalf("report3 index.html = %q, want allure2 marker for latest run", indexHTML)
	}

	waitForLogFragment(t, harness.logs, "report_generation_started")
	waitForLogFragment(t, harness.logs, "report_generation_completed")
}

func TestGenerationPipelineAllure3(t *testing.T) {
	harness := newGenerationPipelineHarness(t, config.GenerateVariantAllure3, copyIntegrationCLI(t, "allure3"))
	assertStatus(t, harness.baseURL+"/readyz", http.StatusOK)

	report1 := harness.uploadAndWaitForReady(t, "history-allure3", "allure-results-history-1.zip")
	report2 := harness.uploadAndWaitForReady(t, "history-allure3", "allure-results-history-2.zip")
	report3 := harness.uploadAndWaitForReady(t, "history-allure3", "allure-results-history-3.zip")

	assertReportHistoryLines(t, harness.downloadReportObject(t, report1, "history/history.jsonl"), []string{"history-1"})
	assertReportHistoryLines(t, harness.downloadReportObject(t, report2, "history/history.jsonl"), []string{"history-1", "history-2"})
	assertReportHistoryLines(t, harness.downloadReportObject(t, report3, "history/history.jsonl"), []string{"history-1", "history-2", "history-3"})

	assertObjectKeysContain(t, harness.listReportObjectKeys(t, report3),
		report3.GeneratedObjectKey,
		harness.reportObjectKey(report3, "history/history.jsonl"),
	)

	indexHTML := harness.downloadObjectString(t, report3.GeneratedObjectKey)
	if !strings.Contains(indexHTML, "allure3") || !strings.Contains(indexHTML, "history-3") {
		t.Fatalf("report3 index.html = %q, want allure3 marker for latest run", indexHTML)
	}

	waitForLogFragment(t, harness.logs, "report_generation_started")
	waitForLogFragment(t, harness.logs, "report_generation_completed")
}

func TestGenerationFailureMarksReportFailed(t *testing.T) {
	failureCLI := writeIntegrationCLI(t, "allure-fail.sh", "#!/bin/sh\necho 'fixture boom' >&2\nexit 23\n")
	harness := newGenerationPipelineHarness(t, config.GenerateVariantAllure2, failureCLI)

	response := harness.uploadArchive(t, "history-failure", "allure-results-history-1.zip")
	report := harness.waitForTerminalReport(t, response.ReportID)
	assertFailedReport(t, report, response.ProjectSlug)

	if got := report.GeneratedObjectKey; got != "" {
		t.Fatalf("report.GeneratedObjectKey = %q, want empty", got)
	}
	if got := report.ErrorMessage; !strings.Contains(got, "fixture boom") {
		t.Fatalf("report.ErrorMessage = %q, want fixture stderr", got)
	}

	objects := harness.listReportObjectKeys(t, report)
	if len(objects) != 0 {
		t.Fatalf("generated object count = %d, want 0 for failed report: %v", len(objects), objects)
	}

	reports, err := harness.sqliteStore.ListReports(context.Background(), response.ProjectSlug)
	if err != nil {
		t.Fatalf("ListReports(%q) error = %v", response.ProjectSlug, err)
	}
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1", len(reports))
	}
	if got, want := reports[0].Status, db.ReportStatusFailed; got != want {
		t.Fatalf("reports[0].Status = %q, want %q", got, want)
	}

	waitForLogFragment(t, harness.logs, "report_generation_failed")
	waitForLogFragment(t, harness.logs, response.ReportID)
	waitForLogFragment(t, harness.logs, response.ProjectSlug)
}

type generationPipelineHarnessOptions struct {
	registerRoutes func(r chi.Router, sqliteStore *db.SQLiteStore, s3Store *storage.S3Store)
}

type generationDispatcherAdapter struct {
	service *generate.Service
}

func (a generationDispatcherAdapter) Enqueue(req upload.GenerationRequest) error {
	if a.service == nil {
		return fmt.Errorf("generation dispatcher adapter: nil service")
	}
	return a.service.Enqueue(generate.Job{
		ProjectSlug: req.ProjectSlug,
		ReportID:    req.ReportID,
		ResultsDir:  req.ResultsDir,
	})
}

type generationPipelineHarness struct {
	baseURL     string
	logs        *bytes.Buffer
	sqliteStore *db.SQLiteStore
	s3Store     *storage.S3Store
}

func newGenerationPipelineHarness(t *testing.T, variant config.GenerateVariant, cliPath string) *generationPipelineHarness {
	return newGenerationPipelineHarnessWithOptions(t, variant, cliPath, generationPipelineHarnessOptions{})
}

func newGenerationPipelineHarnessWithOptions(t *testing.T, variant config.GenerateVariant, cliPath string, opts generationPipelineHarnessOptions) *generationPipelineHarness {
	t.Helper()

	ctx := context.Background()
	minioContainer, err := miniomodule.Run(
		ctx,
		"minio/minio:RELEASE.2024-01-16T16-07-38Z",
		miniomodule.WithUsername("minioadmin"),
		miniomodule.WithPassword("minioadmin"),
	)
	if err != nil {
		t.Fatalf("minio.Run() error = %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(minioContainer); err != nil {
			t.Fatalf("TerminateContainer() error = %v", err)
		}
	})

	endpoint, err := minioContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("ConnectionString() error = %v", err)
	}
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}

	logs := &bytes.Buffer{}
	logger, err := server.NewLogger("debug", logs)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	rootDir := t.TempDir()
	sqliteStore, err := db.OpenSQLiteStore(ctx, filepath.Join(rootDir, "testimony.sqlite"), 5*time.Second, logger)
	if err != nil {
		t.Fatalf("OpenSQLiteStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := sqliteStore.Close(); err != nil {
			t.Fatalf("sqliteStore.Close() error = %v", err)
		}
	})

	s3Store, err := storage.NewS3Store(ctx, config.S3Config{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		Bucket:          "testimony",
		AccessKeyID:     "minioadmin",
		SecretAccessKey: "minioadmin",
		UsePathStyle:    true,
	}, logger)
	if err != nil {
		t.Fatalf("NewS3Store() error = %v", err)
	}

	generator, err := generate.New(config.GenerateConfig{
		Variant:        variant,
		CLIPath:        cliPath,
		Timeout:        5 * time.Second,
		MaxConcurrency: 1,
		HistoryDepth:   5,
	}, s3Store, nil)
	if err != nil {
		t.Fatalf("generate.New() error = %v", err)
	}

	service, err := generate.NewService(generate.ServiceOptions{
		Logger:         logger,
		Store:          sqliteStore,
		Storage:        s3Store,
		Generator:      generator,
		TempDir:        filepath.Join(rootDir, "tmp"),
		MaxConcurrency: 1,
	})
	if err != nil {
		t.Fatalf("generate.NewService() error = %v", err)
	}

	var nextID atomic.Int64
	uploadHandler, err := upload.NewHandler(upload.HandlerOptions{
		Logger:     logger,
		Store:      sqliteStore,
		Storage:    s3Store,
		Dispatcher: generationDispatcherAdapter{service: service},
		TempDir:    filepath.Join(rootDir, "tmp"),
		NewID: func() string {
			return fmt.Sprintf("report-%03d", nextID.Add(1))
		},
	})
	if err != nil {
		t.Fatalf("upload.NewHandler() error = %v", err)
	}

	health := server.NewHealth(logger,
		server.ReadinessCheck{Name: "sqlite", Check: sqliteStore.Ready},
		server.ReadinessCheck{Name: "s3", Check: s3Store.Ready},
	)
	health.SetReady(true, "ready")

	httpServer := httptest.NewServer(server.NewRouter(server.Options{
		Logger:        logger,
		Health:        health,
		UploadHandler: uploadHandler,
		RegisterRoutes: func(r chi.Router) {
			if opts.registerRoutes != nil {
				opts.registerRoutes(r, sqliteStore, s3Store)
			}
		},
	}))
	t.Cleanup(httpServer.Close)

	return &generationPipelineHarness{
		baseURL:     httpServer.URL,
		logs:        logs,
		sqliteStore: sqliteStore,
		s3Store:     s3Store,
	}
}

func (h *generationPipelineHarness) uploadAndWaitForReady(t *testing.T, projectSlug, fixtureName string) db.Report {
	t.Helper()
	response := h.uploadArchive(t, projectSlug, fixtureName)
	report := h.waitForTerminalReport(t, response.ReportID)
	assertReadyReport(t, report, response.ProjectSlug)
	return report
}

func (h *generationPipelineHarness) uploadArchive(t *testing.T, projectSlug, fixtureName string) uploadResponseBody {
	t.Helper()

	fixturePath := filepath.Join("testdata", fixtureName)
	payload, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", fixturePath, err)
	}

	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/v1/projects/"+projectSlug+"/upload", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, fixtureName))

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

func (h *generationPipelineHarness) waitForTerminalReport(t *testing.T, reportID string) db.Report {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		report, err := h.sqliteStore.GetReport(context.Background(), reportID)
		if err == nil && (report.Status == db.ReportStatusReady || report.Status == db.ReportStatusFailed) {
			return report
		}
		time.Sleep(50 * time.Millisecond)
	}

	report, err := h.sqliteStore.GetReport(context.Background(), reportID)
	if err != nil {
		t.Fatalf("GetReport(%q) error = %v", reportID, err)
	}
	t.Fatalf("report %q timed out in status %q; logs=%q", reportID, report.Status, h.logs.String())
	panic("unreachable")
}

func (h *generationPipelineHarness) listReportObjectKeys(t *testing.T, report db.Report) []string {
	t.Helper()

	prefix := fmt.Sprintf("projects/%s/reports/%s/html/", report.ProjectSlug, report.ID)
	objects, err := h.s3Store.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%q) error = %v", prefix, err)
	}

	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key)
	}
	sort.Strings(keys)
	return keys
}

func (h *generationPipelineHarness) reportObjectKey(report db.Report, relativePath string) string {
	return fmt.Sprintf("projects/%s/reports/%s/html/%s", report.ProjectSlug, report.ID, relativePath)
}

func (h *generationPipelineHarness) downloadReportObject(t *testing.T, report db.Report, relativePath string) string {
	t.Helper()
	return h.downloadObjectString(t, h.reportObjectKey(report, relativePath))
}

func (h *generationPipelineHarness) downloadObjectString(t *testing.T, key string) string {
	t.Helper()

	result, err := h.s3Store.Download(context.Background(), key)
	if err != nil {
		t.Fatalf("Download(%q) error = %v", key, err)
	}
	defer result.Body.Close()

	payload, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(%q) error = %v", key, err)
	}
	return string(payload)
}

func assertReadyReport(t *testing.T, report db.Report, projectSlug string) {
	t.Helper()

	if got, want := report.ProjectSlug, projectSlug; got != want {
		t.Fatalf("report.ProjectSlug = %q, want %q", got, want)
	}
	if got, want := report.Status, db.ReportStatusReady; got != want {
		t.Fatalf("report.Status = %q, want %q", got, want)
	}
	if report.StartedAt == nil {
		t.Fatal("report.StartedAt = nil, want value")
	}
	if report.CompletedAt == nil {
		t.Fatal("report.CompletedAt = nil, want value")
	}
	if got := report.ErrorMessage; got != "" {
		t.Fatalf("report.ErrorMessage = %q, want empty", got)
	}
	wantObjectKey := fmt.Sprintf("projects/%s/reports/%s/html/index.html", report.ProjectSlug, report.ID)
	if got := report.GeneratedObjectKey; got != wantObjectKey {
		t.Fatalf("report.GeneratedObjectKey = %q, want %q", got, wantObjectKey)
	}
}

func assertFailedReport(t *testing.T, report db.Report, projectSlug string) {
	t.Helper()

	if got, want := report.ProjectSlug, projectSlug; got != want {
		t.Fatalf("report.ProjectSlug = %q, want %q", got, want)
	}
	if got, want := report.Status, db.ReportStatusFailed; got != want {
		t.Fatalf("report.Status = %q, want %q", got, want)
	}
	if report.StartedAt == nil {
		t.Fatal("report.StartedAt = nil, want value")
	}
	if report.CompletedAt == nil {
		t.Fatal("report.CompletedAt = nil, want value")
	}
	if got := report.ErrorMessage; got == "" {
		t.Fatal("report.ErrorMessage = empty, want failure text")
	}
}

func assertObjectKeysContain(t *testing.T, keys []string, want ...string) {
	t.Helper()

	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	for _, key := range want {
		if _, ok := keySet[key]; !ok {
			t.Fatalf("object keys %v did not include %q", keys, key)
		}
	}
}

func assertReportHistoryJSON(t *testing.T, payload string, want []string) {
	t.Helper()

	var got []string
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("json.Unmarshal(history JSON) error = %v; payload=%q", err, payload)
	}
	if len(got) != len(want) {
		t.Fatalf("history JSON length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history JSON[%d] = %q, want %q; full=%v", i, got[i], want[i], got)
		}
	}
}

func assertReportHistoryLines(t *testing.T, payload string, want []string) {
	t.Helper()

	trimmed := strings.TrimSpace(payload)
	var got []string
	if trimmed != "" {
		got = strings.Split(trimmed, "\n")
	}
	if len(got) != len(want) {
		t.Fatalf("history line count = %d, want %d; got=%v payload=%q", len(got), len(want), got, payload)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history line[%d] = %q, want %q; full=%v", i, got[i], want[i], got)
		}
	}
}

func copyIntegrationCLI(t *testing.T, name string) string {
	t.Helper()

	sourcePath := filepath.Join("testdata", "allure-cli", name)
	payload, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", sourcePath, err)
	}

	targetPath := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(targetPath, payload, 0o755); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", targetPath, err)
	}
	return targetPath
}

func writeIntegrationCLI(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
	return path
}

func waitForLogFragment(t *testing.T, logs *bytes.Buffer, fragment string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), fragment) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("log output did not contain %q; logs=%q", fragment, logs.String())
}
