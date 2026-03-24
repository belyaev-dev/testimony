package upload_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/server"
	"github.com/testimony-dev/testimony/internal/upload"
)

func TestUploadHandlerAcceptsRawZipAndCreatesPendingReport(t *testing.T) {
	ctx := context.Background()
	sqliteStore := openSQLiteStore(t, ctx)
	defer sqliteStore.Close()

	objectStore := newMemoryObjectStore()
	handler := newUploadHandler(t, sqliteStore, objectStore, nil, func() string { return "report-123" })
	router := server.NewRouter(server.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadHandler: handler})

	payload := buildZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/backend-tests/upload", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Disposition", `attachment; filename="results.tar.gz"`)
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if got, want := res.Code, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}

	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got, want := body["report_id"], "report-123"; got != want {
		t.Fatalf("report_id = %v, want %v", got, want)
	}
	if got, want := body["project_slug"], "backend-tests"; got != want {
		t.Fatalf("project_slug = %v, want %v", got, want)
	}
	if got, want := body["status"], "pending"; got != want {
		t.Fatalf("status body = %v, want %v", got, want)
	}
	if got, want := body["archive_format"], "zip"; got != want {
		t.Fatalf("archive_format = %v, want %v", got, want)
	}

	objectKey := "projects/backend-tests/reports/report-123/archive.zip"
	if got, want := objectStore.body(objectKey), payload; !bytes.Equal(got, want) {
		t.Fatalf("stored object = %q, want %q", got, want)
	}

	project, err := sqliteStore.GetProject(ctx, "backend-tests")
	if err != nil {
		t.Fatalf("GetProject() error = %v", err)
	}
	if got, want := project.Slug, "backend-tests"; got != want {
		t.Fatalf("project.Slug = %q, want %q", got, want)
	}

	report, err := sqliteStore.GetReport(ctx, "report-123")
	if err != nil {
		t.Fatalf("GetReport() error = %v", err)
	}
	if got, want := report.ProjectSlug, "backend-tests"; got != want {
		t.Fatalf("report.ProjectSlug = %q, want %q", got, want)
	}
	if got, want := report.Status, db.ReportStatusPending; got != want {
		t.Fatalf("report.Status = %q, want %q", got, want)
	}
	if got, want := report.ArchiveObjectKey, objectKey; got != want {
		t.Fatalf("report.ArchiveObjectKey = %q, want %q", got, want)
	}
	if got, want := report.SourceFilename, "results.tar.gz"; got != want {
		t.Fatalf("report.SourceFilename = %q, want %q", got, want)
	}
}

func TestUploadHandlerAcceptsMultipartTarGz(t *testing.T) {
	ctx := context.Background()
	sqliteStore := openSQLiteStore(t, ctx)
	defer sqliteStore.Close()

	objectStore := newMemoryObjectStore()
	handler := newUploadHandler(t, sqliteStore, objectStore, nil, func() string { return "report-456" })
	router := server.NewRouter(server.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadHandler: handler})

	payload := buildTarGzArchive(t, map[string]string{
		"suite/widgets-container.json": `{"uuid":"456"}`,
	})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "results.zip")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("part.Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart writer close error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/frontend-tests/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if got, want := res.Code, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}

	var response map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got, want := response["archive_format"], "tar.gz"; got != want {
		t.Fatalf("archive_format = %v, want %v", got, want)
	}
	if got, want := response["archive_object_key"], "projects/frontend-tests/reports/report-456/archive.tar.gz"; got != want {
		t.Fatalf("archive_object_key = %v, want %v", got, want)
	}

	report, err := sqliteStore.GetReport(ctx, "report-456")
	if err != nil {
		t.Fatalf("GetReport() error = %v", err)
	}
	if got, want := report.SourceFilename, "results.zip"; got != want {
		t.Fatalf("report.SourceFilename = %q, want %q", got, want)
	}
}

func TestUploadHandlerRejectsUnsafeArchiveWithoutPersistence(t *testing.T) {
	ctx := context.Background()
	sqliteStore := openSQLiteStore(t, ctx)
	defer sqliteStore.Close()

	objectStore := newMemoryObjectStore()
	handler := newUploadHandler(t, sqliteStore, objectStore, nil, func() string { return "report-unsafe" })
	router := server.NewRouter(server.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadHandler: handler})

	payload := buildZipArchive(t, map[string]string{
		"../evil-result.json": `{"name":"oops"}`,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/unsafe-project/upload", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/zip")
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if got, want := res.Code, http.StatusBadRequest; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if len(objectStore.keys()) != 0 {
		t.Fatalf("stored object keys = %v, want none", objectStore.keys())
	}
	if _, err := sqliteStore.GetProject(ctx, "unsafe-project"); !errors.Is(err, db.ErrProjectNotFound) {
		t.Fatalf("GetProject(unsafe-project) error = %v, want ErrProjectNotFound", err)
	}
	if !strings.Contains(res.Body.String(), "escapes extraction root") {
		t.Fatalf("response body = %q, want traversal error", res.Body.String())
	}
}

func TestUploadHandlerDispatchesGenerationOnceForAcceptedUpload(t *testing.T) {
	ctx := context.Background()
	sqliteStore := openSQLiteStore(t, ctx)
	defer sqliteStore.Close()

	objectStore := newMemoryObjectStore()
	dispatcher := &spyGenerationDispatcher{}
	handler := newUploadHandler(t, sqliteStore, objectStore, dispatcher, func() string { return "report-dispatch" })
	router := server.NewRouter(server.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadHandler: handler})

	payload := buildZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/backend-tests/upload", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/zip")
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if got, want := res.Code, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := dispatcher.calls(), 1; got != want {
		t.Fatalf("dispatcher calls = %d, want %d", got, want)
	}

	request := dispatcher.lastRequest()
	if got, want := request.ProjectSlug, "backend-tests"; got != want {
		t.Fatalf("dispatcher ProjectSlug = %q, want %q", got, want)
	}
	if got, want := request.ReportID, "report-dispatch"; got != want {
		t.Fatalf("dispatcher ReportID = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(request.ResultsDir, "widgets-result.json")); err != nil {
		t.Fatalf("generation results not preserved at %q: %v", request.ResultsDir, err)
	}

	report, err := sqliteStore.GetReport(ctx, "report-dispatch")
	if err != nil {
		t.Fatalf("GetReport() error = %v", err)
	}
	if got, want := report.Status, db.ReportStatusPending; got != want {
		t.Fatalf("report.Status = %q, want %q", got, want)
	}
}

func TestUploadHandlerRollsBackArchiveWhenMetadataPersistenceFails(t *testing.T) {
	objectStore := newMemoryObjectStore()
	dispatcher := &spyGenerationDispatcher{}
	handler := newUploadHandler(t, &failingCreateReportStore{}, objectStore, dispatcher, func() string { return "report-rollback" })
	router := server.NewRouter(server.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), UploadHandler: handler})

	payload := buildZipArchive(t, map[string]string{
		"widgets-result.json": `{"name":"widgets"}`,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/backend-tests/upload", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/zip")
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if got, want := res.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := dispatcher.calls(), 0; got != want {
		t.Fatalf("dispatcher calls = %d, want %d", got, want)
	}
	if got, want := len(objectStore.deleted), 1; got != want {
		t.Fatalf("deleted object keys = %v, want one rollback delete", objectStore.deleted)
	}
	if got, want := len(objectStore.keys()), 0; got != want {
		t.Fatalf("remaining stored object count = %d, want %d", got, want)
	}
}

func openSQLiteStore(t *testing.T, ctx context.Context) *db.SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "testimony.sqlite")
	store, err := db.OpenSQLiteStore(ctx, path, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("OpenSQLiteStore(%q) error = %v", path, err)
	}
	return store
}

func newUploadHandler(t *testing.T, reportStore upload.ReportStore, objectStore upload.ObjectStore, dispatcher upload.GenerationDispatcher, newID func() string) *upload.Handler {
	t.Helper()

	handler, err := upload.NewHandler(upload.HandlerOptions{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:      reportStore,
		Storage:    objectStore,
		Dispatcher: dispatcher,
		TempDir:    t.TempDir(),
		NewID:      newID,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

type memoryObjectStore struct {
	objects map[string][]byte
	deleted []string
}

func newMemoryObjectStore() *memoryObjectStore {
	return &memoryObjectStore{objects: make(map[string][]byte)}
}

func (s *memoryObjectStore) Upload(_ context.Context, key string, body io.Reader, _ int64, _ string) error {
	payload, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = payload
	return nil
}

func (s *memoryObjectStore) Delete(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	delete(s.objects, key)
	return nil
}

func (s *memoryObjectStore) body(key string) []byte {
	payload, ok := s.objects[key]
	if !ok {
		return nil
	}
	return append([]byte(nil), payload...)
}

func (s *memoryObjectStore) keys() []string {
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		keys = append(keys, key)
	}
	return keys
}

type spyGenerationDispatcher struct {
	requests []upload.GenerationRequest
}

func (d *spyGenerationDispatcher) Enqueue(req upload.GenerationRequest) error {
	d.requests = append(d.requests, req)
	return nil
}

func (d *spyGenerationDispatcher) calls() int {
	return len(d.requests)
}

func (d *spyGenerationDispatcher) lastRequest() upload.GenerationRequest {
	if len(d.requests) == 0 {
		return upload.GenerationRequest{}
	}
	return d.requests[len(d.requests)-1]
}

type failingCreateReportStore struct{}

func (s *failingCreateReportStore) CreateReport(context.Context, db.CreateReportParams) (db.Report, error) {
	return db.Report{}, errors.New("metadata write failed")
}

func (s *failingCreateReportStore) UpdateReportStatus(context.Context, db.UpdateReportStatusParams) (db.Report, error) {
	return db.Report{}, errors.New("unexpected status update")
}
