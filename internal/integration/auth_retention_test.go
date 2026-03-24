package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	testcontainers "github.com/testcontainers/testcontainers-go"
	miniomodule "github.com/testcontainers/testcontainers-go/modules/minio"
	internalauth "github.com/testimony-dev/testimony/internal/auth"
	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/generate"
	"github.com/testimony-dev/testimony/internal/retention"
	servepkg "github.com/testimony-dev/testimony/internal/serve"
	"github.com/testimony-dev/testimony/internal/server"
	"github.com/testimony-dev/testimony/internal/storage"
	"github.com/testimony-dev/testimony/internal/upload"
)

func TestAuthAndRetentionAgainstMinIO(t *testing.T) {
	const apiKey = "bootstrap-key-test"
	cliPath := copyIntegrationCLI(t, "allure2")

	t.Run("default viewer stays open and per-project retention override cleans objects", func(t *testing.T) {
		harness := newAuthRetentionHarness(t, authRetentionHarnessOptions{
			APIKey:              apiKey,
			CLIPath:             cliPath,
			Variant:             config.GenerateVariantAllure2,
			GlobalRetentionDays: 0,
			CleanupInterval:     50 * time.Millisecond,
		})

		assertStatus(t, harness.baseURL+"/healthz", http.StatusOK)
		assertStatus(t, harness.baseURL+"/readyz", http.StatusOK)

		unauthorizedResp := harness.uploadArchiveRequest(t, "history-auth", "allure-results-history-1.zip", "")
		if got, want := unauthorizedResp.StatusCode, http.StatusUnauthorized; got != want {
			body := readIntegrationResponseBody(t, unauthorizedResp)
			unauthorizedResp.Body.Close()
			t.Fatalf("unauthorized upload status = %d, want %d, body=%q", got, want, body)
		}
		if got, want := unauthorizedResp.Header.Get("WWW-Authenticate"), "Bearer"; got != want {
			body := readIntegrationResponseBody(t, unauthorizedResp)
			unauthorizedResp.Body.Close()
			t.Fatalf("unauthorized upload challenge = %q, want %q, body=%q", got, want, body)
		}
		unauthorizedResp.Body.Close()
		waitForLogFragment(t, harness.logs, "auth rejected")
		waitForLogFragment(t, harness.logs, "/api/v1/projects/history-auth/upload")
		if strings.Contains(harness.logs.String(), apiKey) {
			t.Fatalf("integration logs leaked bootstrap api key: %q", harness.logs.String())
		}

		response := harness.uploadArchive(t, "history-auth", "allure-results-history-1.zip", apiKey)
		report := harness.waitForTerminalReport(t, response.ReportID)
		assertReadyReport(t, report, response.ProjectSlug)

		projectResp := harness.getRequest(t, "/projects/history-auth", "")
		if got, want := projectResp.StatusCode, http.StatusOK; got != want {
			body := readIntegrationResponseBody(t, projectResp)
			projectResp.Body.Close()
			t.Fatalf("GET /projects/history-auth status = %d, want %d, body=%q", got, want, body)
		}
		projectResp.Body.Close()

		reportResp := harness.getRequest(t, "/reports/history-auth/"+report.ID+"/", "")
		if got, want := reportResp.StatusCode, http.StatusOK; got != want {
			body := readIntegrationResponseBody(t, reportResp)
			reportResp.Body.Close()
			t.Fatalf("GET report root status = %d, want %d, body=%q", got, want, body)
		}
		reportBody := readIntegrationResponseBody(t, reportResp)
		reportResp.Body.Close()
		if !strings.Contains(reportBody, "allure2") {
			t.Fatalf("report root body = %q, want allure2 marker", reportBody)
		}

		overrideDays := 1
		if _, err := harness.sqliteStore.SetProjectRetentionDays(context.Background(), response.ProjectSlug, &overrideDays); err != nil {
			t.Fatalf("SetProjectRetentionDays(%q) error = %v", response.ProjectSlug, err)
		}

		expiredAt := time.Now().UTC().Add(-48 * time.Hour)
		if _, err := harness.sqliteStore.UpdateReportStatus(context.Background(), db.UpdateReportStatusParams{
			ReportID:            report.ID,
			Status:              db.ReportStatusReady,
			GeneratedObjectKey:  report.GeneratedObjectKey,
			CompletedAtOverride: &expiredAt,
		}); err != nil {
			t.Fatalf("UpdateReportStatus(expired ready) error = %v", err)
		}

		harness.waitForReportDeleted(t, report.ID)
		harness.waitForHTMLObjectsDeleted(t, report)
		harness.waitForObjectDeleted(t, report.ArchiveObjectKey)
		waitForLogFragment(t, harness.logs, "retention cleanup succeeded")
		waitForLogFragment(t, harness.logs, report.ID)
	})

	t.Run("viewer auth toggle protects browse and report routes", func(t *testing.T) {
		harness := newAuthRetentionHarness(t, authRetentionHarnessOptions{
			APIKey:              apiKey,
			CLIPath:             cliPath,
			Variant:             config.GenerateVariantAllure2,
			GlobalRetentionDays: 0,
			CleanupInterval:     time.Hour,
			RequireViewer:       true,
		})

		assertStatus(t, harness.baseURL+"/healthz", http.StatusOK)
		assertStatus(t, harness.baseURL+"/readyz", http.StatusOK)

		response := harness.uploadArchive(t, "history-protected", "allure-results-history-1.zip", apiKey)
		report := harness.waitForTerminalReport(t, response.ReportID)
		assertReadyReport(t, report, response.ProjectSlug)

		for _, path := range []string{"/", "/projects/history-protected", "/reports/history-protected/" + report.ID + "/"} {
			resp := harness.getRequest(t, path, "")
			if got, want := resp.StatusCode, http.StatusUnauthorized; got != want {
				body := readIntegrationResponseBody(t, resp)
				resp.Body.Close()
				t.Fatalf("GET %s status = %d, want %d, body=%q", path, got, want, body)
			}
			if got, want := resp.Header.Get("WWW-Authenticate"), "Bearer"; got != want {
				body := readIntegrationResponseBody(t, resp)
				resp.Body.Close()
				t.Fatalf("GET %s challenge = %q, want %q, body=%q", path, got, want, body)
			}
			resp.Body.Close()
		}

		rootResp := harness.getRequest(t, "/", apiKey)
		if got, want := rootResp.StatusCode, http.StatusOK; got != want {
			body := readIntegrationResponseBody(t, rootResp)
			rootResp.Body.Close()
			t.Fatalf("authorized GET / status = %d, want %d, body=%q", got, want, body)
		}
		rootResp.Body.Close()

		reportResp := harness.getRequest(t, "/reports/history-protected/"+report.ID+"/", apiKey)
		if got, want := reportResp.StatusCode, http.StatusOK; got != want {
			body := readIntegrationResponseBody(t, reportResp)
			reportResp.Body.Close()
			t.Fatalf("authorized GET report root status = %d, want %d, body=%q", got, want, body)
		}
		reportResp.Body.Close()

		waitForLogFragment(t, harness.logs, "auth rejected")
		waitForLogFragment(t, harness.logs, "/projects/history-protected")
		if strings.Contains(harness.logs.String(), apiKey) {
			t.Fatalf("integration logs leaked bootstrap api key: %q", harness.logs.String())
		}
	})
}

type authRetentionHarnessOptions struct {
	APIKey              string
	CLIPath             string
	Variant             config.GenerateVariant
	GlobalRetentionDays int
	CleanupInterval     time.Duration
	RequireViewer       bool
}

type authRetentionHarness struct {
	baseURL     string
	logs        *bytes.Buffer
	sqliteStore *db.SQLiteStore
	s3Store     *storage.S3Store
}

func newAuthRetentionHarness(t *testing.T, opts authRetentionHarnessOptions) *authRetentionHarness {
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

	if _, err := sqliteStore.EnsureAPIKey(ctx, db.APIKeyBootstrapName, opts.APIKey); err != nil {
		t.Fatalf("EnsureAPIKey() error = %v", err)
	}

	generator, err := generate.New(config.GenerateConfig{
		Variant:        opts.Variant,
		CLIPath:        opts.CLIPath,
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

	authMiddleware, err := internalauth.NewMiddleware(internalauth.MiddlewareOptions{
		Enabled:   true,
		Validator: sqliteStore,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("auth.NewMiddleware() error = %v", err)
	}

	browseUI, err := servepkg.NewUI(logger, sqliteStore)
	if err != nil {
		t.Fatalf("serve.NewUI() error = %v", err)
	}
	reportProxy, err := servepkg.NewReportProxy(logger, sqliteStore, s3Store)
	if err != nil {
		t.Fatalf("serve.NewReportProxy() error = %v", err)
	}

	worker, err := retention.NewWorker(retention.WorkerOptions{
		Logger:              logger,
		Store:               sqliteStore,
		Storage:             s3Store,
		GlobalRetentionDays: opts.GlobalRetentionDays,
		Interval:            opts.CleanupInterval,
	})
	if err != nil {
		t.Fatalf("retention.NewWorker() error = %v", err)
	}

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	if err := worker.Start(workerCtx); err != nil {
		t.Fatalf("worker.Start() error = %v", err)
	}

	health := server.NewHealth(logger,
		server.ReadinessCheck{Name: "sqlite", Check: sqliteStore.Ready},
		server.ReadinessCheck{Name: "s3", Check: s3Store.Ready},
	)
	health.SetReady(true, "ready")

	httpServer := httptest.NewServer(server.NewRouter(server.Options{
		Logger:        logger,
		Health:        health,
		UploadHandler: authMiddleware(uploadHandler),
		RegisterRoutes: func(r chi.Router) {
			registerViewerRoutes := func(router chi.Router) {
				browseUI.RegisterRoutes(router)
				reportProxy.RegisterRoutes(router)
			}
			if opts.RequireViewer {
				r.Group(func(protected chi.Router) {
					protected.Use(authMiddleware)
					registerViewerRoutes(protected)
				})
				return
			}
			registerViewerRoutes(r)
		},
	}))

	t.Cleanup(func() {
		httpServer.Close()
		cancelWorker()
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelStop()
		if err := worker.Stop(stopCtx); err != nil {
			t.Fatalf("worker.Stop() error = %v", err)
		}
		if err := sqliteStore.Close(); err != nil {
			t.Fatalf("sqliteStore.Close() error = %v", err)
		}
		if err := testcontainers.TerminateContainer(minioContainer); err != nil {
			t.Fatalf("TerminateContainer() error = %v", err)
		}
	})

	return &authRetentionHarness{
		baseURL:     httpServer.URL,
		logs:        logs,
		sqliteStore: sqliteStore,
		s3Store:     s3Store,
	}
}

func (h *authRetentionHarness) uploadArchive(t *testing.T, projectSlug, fixtureName, apiKey string) uploadResponseBody {
	t.Helper()

	resp := h.uploadArchiveRequest(t, projectSlug, fixtureName, apiKey)
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusAccepted; got != want {
		body := readIntegrationResponseBody(t, resp)
		t.Fatalf("upload status = %d, want %d, body=%q", got, want, body)
	}

	var response uploadResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}
	return response
}

func (h *authRetentionHarness) uploadArchiveRequest(t *testing.T, projectSlug, fixtureName, apiKey string) *http.Response {
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
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request error = %v", err)
	}
	return resp
}

func (h *authRetentionHarness) getRequest(t *testing.T, requestPath, apiKey string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, h.baseURL+requestPath, nil)
	if err != nil {
		t.Fatalf("http.NewRequest(%q) error = %v", requestPath, err)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s error = %v", requestPath, err)
	}
	return resp
}

func (h *authRetentionHarness) waitForTerminalReport(t *testing.T, reportID string) db.Report {
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

func (h *authRetentionHarness) waitForReportDeleted(t *testing.T, reportID string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.sqliteStore.GetReport(context.Background(), reportID)
		if errors.Is(err, db.ErrReportNotFound) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	report, err := h.sqliteStore.GetReport(context.Background(), reportID)
	if err == nil {
		t.Fatalf("report %q still present with status %q, want deleted", reportID, report.Status)
	}
	t.Fatalf("report %q was not deleted; last error = %v", reportID, err)
}

func (h *authRetentionHarness) waitForHTMLObjectsDeleted(t *testing.T, report db.Report) {
	t.Helper()

	prefix := fmt.Sprintf("projects/%s/reports/%s/html/", report.ProjectSlug, report.ID)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		objects, err := h.s3Store.List(context.Background(), prefix)
		if err == nil && len(objects) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	objects, err := h.s3Store.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%q) error = %v", prefix, err)
	}
	t.Fatalf("html objects still present for %q: %v", report.ID, objects)
}

func (h *authRetentionHarness) waitForObjectDeleted(t *testing.T, key string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.s3Store.Download(context.Background(), key)
		if errors.Is(err, storage.ErrObjectNotFound) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	result, err := h.s3Store.Download(context.Background(), key)
	if err == nil {
		result.Body.Close()
		t.Fatalf("object %q still present after retention cleanup", key)
	}
	t.Fatalf("object %q was not deleted; last error = %v", key, err)
}

func readIntegrationResponseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(response body) error = %v", err)
	}
	return string(body)
}
