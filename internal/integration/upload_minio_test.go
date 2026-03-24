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
	"strings"
	"testing"
	"time"

	testcontainers "github.com/testcontainers/testcontainers-go"
	miniomodule "github.com/testcontainers/testcontainers-go/modules/minio"
	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/server"
	"github.com/testimony-dev/testimony/internal/storage"
	"github.com/testimony-dev/testimony/internal/upload"
)

func TestUploadFoundationAgainstMinIO(t *testing.T) {
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
	defer func() {
		if err := testcontainers.TerminateContainer(minioContainer); err != nil {
			t.Fatalf("TerminateContainer() error = %v", err)
		}
	}()

	endpoint, err := minioContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("ConnectionString() error = %v", err)
	}
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}

	logger, err := server.NewLogger("debug", io.Discard)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	sqlitePath := filepath.Join(t.TempDir(), "testimony.sqlite")
	sqliteStore, err := db.OpenSQLiteStore(ctx, sqlitePath, 5*time.Second, logger)
	if err != nil {
		t.Fatalf("OpenSQLiteStore() error = %v", err)
	}
	defer sqliteStore.Close()

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

	uploadHandler, err := upload.NewHandler(upload.HandlerOptions{
		Logger:  logger,
		Store:   sqliteStore,
		Storage: s3Store,
		TempDir: filepath.Join(t.TempDir(), "uploads"),
		NewID:   func() string { return "report-minio-001" },
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
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
	}))
	defer httpServer.Close()

	assertStatus(t, httpServer.URL+"/healthz", http.StatusOK)
	assertStatus(t, httpServer.URL+"/readyz", http.StatusOK)

	fixturePath := filepath.Join("testdata", "allure-results.zip")
	payload, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", fixturePath, err)
	}

	req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/projects/backend-tests/upload", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("Content-Disposition", `attachment; filename="allure-results.zip"`)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request error = %v", err)
	}
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusAccepted; got != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status = %d, want %d, body = %s", got, want, body)
	}

	var uploadResponse struct {
		ReportID         string `json:"report_id"`
		ProjectSlug      string `json:"project_slug"`
		Status           string `json:"status"`
		ArchiveFormat    string `json:"archive_format"`
		ArchiveObjectKey string `json:"archive_object_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&uploadResponse); err != nil {
		t.Fatalf("decode upload response error = %v", err)
	}

	if got, want := uploadResponse.ReportID, "report-minio-001"; got != want {
		t.Fatalf("report_id = %q, want %q", got, want)
	}
	if got, want := uploadResponse.ProjectSlug, "backend-tests"; got != want {
		t.Fatalf("project_slug = %q, want %q", got, want)
	}
	if got, want := uploadResponse.Status, "pending"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	if got, want := uploadResponse.ArchiveFormat, "zip"; got != want {
		t.Fatalf("archive_format = %q, want %q", got, want)
	}

	expectedObjectKey := fmt.Sprintf("projects/%s/reports/%s/archive.zip", uploadResponse.ProjectSlug, uploadResponse.ReportID)
	if got, want := uploadResponse.ArchiveObjectKey, expectedObjectKey; got != want {
		t.Fatalf("archive_object_key = %q, want %q", got, want)
	}

	object, err := s3Store.Download(ctx, expectedObjectKey)
	if err != nil {
		t.Fatalf("Download(%q) error = %v", expectedObjectKey, err)
	}
	defer object.Body.Close()

	downloaded, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatalf("io.ReadAll(downloaded) error = %v", err)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Fatal("downloaded archive did not match uploaded fixture")
	}

	objects, err := s3Store.List(ctx, fmt.Sprintf("projects/%s/reports/%s/", uploadResponse.ProjectSlug, uploadResponse.ReportID))
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("len(objects) = %d, want 1", len(objects))
	}
	if got, want := objects[0].Key, expectedObjectKey; got != want {
		t.Fatalf("objects[0].Key = %q, want %q", got, want)
	}

	project, err := sqliteStore.GetProject(ctx, uploadResponse.ProjectSlug)
	if err != nil {
		t.Fatalf("GetProject() error = %v", err)
	}
	if got, want := project.Slug, uploadResponse.ProjectSlug; got != want {
		t.Fatalf("project.Slug = %q, want %q", got, want)
	}

	report, err := sqliteStore.GetReport(ctx, uploadResponse.ReportID)
	if err != nil {
		t.Fatalf("GetReport() error = %v", err)
	}
	if got, want := report.ProjectSlug, uploadResponse.ProjectSlug; got != want {
		t.Fatalf("report.ProjectSlug = %q, want %q", got, want)
	}
	if got, want := string(report.Status), "pending"; got != want {
		t.Fatalf("report.Status = %q, want %q", got, want)
	}
	if got, want := report.ArchiveObjectKey, expectedObjectKey; got != want {
		t.Fatalf("report.ArchiveObjectKey = %q, want %q", got, want)
	}
	if got, want := report.SourceFilename, "allure-results.zip"; got != want {
		t.Fatalf("report.SourceFilename = %q, want %q", got, want)
	}

	assertStatus(t, httpServer.URL+"/readyz", http.StatusOK)
}

func assertStatus(t *testing.T, url string, want int) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	defer resp.Body.Close()

	if got := resp.StatusCode; got != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s status = %d, want %d, body = %s", url, got, want, body)
	}
}
