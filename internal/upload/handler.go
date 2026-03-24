package upload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/testimony-dev/testimony/internal/db"
)

var projectSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type ReportStore interface {
	CreateReport(ctx context.Context, params db.CreateReportParams) (db.Report, error)
	UpdateReportStatus(ctx context.Context, params db.UpdateReportStatusParams) (db.Report, error)
}

type ObjectStore interface {
	Upload(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	Delete(ctx context.Context, key string) error
}

type GenerationRequest struct {
	ProjectSlug string
	ReportID    string
	ResultsDir  string
}

type GenerationDispatcher interface {
	Enqueue(req GenerationRequest) error
}

type HandlerOptions struct {
	Logger     *slog.Logger
	Store      ReportStore
	Storage    ObjectStore
	Dispatcher GenerationDispatcher
	TempDir    string
	NewID      func() string
}

type Handler struct {
	logger     *slog.Logger
	store      ReportStore
	storage    ObjectStore
	dispatcher GenerationDispatcher
	tempDir    string
	newID      func() string
}

type uploadResponse struct {
	ReportID         string          `json:"report_id"`
	ProjectSlug      string          `json:"project_slug"`
	Status           db.ReportStatus `json:"status"`
	ArchiveFormat    string          `json:"archive_format"`
	ArchiveObjectKey string          `json:"archive_object_key"`
}

type errorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type stagedUpload struct {
	Path         string
	Filename     string
	ContentType  string
	ContentSize  int64
	CleanupPaths []string
}

func NewHandler(opts HandlerOptions) (*Handler, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("new upload handler: nil report store")
	}
	if opts.Storage == nil {
		return nil, fmt.Errorf("new upload handler: nil object store")
	}
	if strings.TrimSpace(opts.TempDir) == "" {
		return nil, fmt.Errorf("new upload handler: empty temp dir")
	}
	if err := os.MkdirAll(opts.TempDir, 0o755); err != nil {
		return nil, fmt.Errorf("new upload handler: create temp dir %q: %w", opts.TempDir, err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	newID := opts.NewID
	if newID == nil {
		newID = uuid.NewString
	}

	return &Handler{
		logger:     logger,
		store:      opts.Store,
		storage:    opts.Storage,
		dispatcher: opts.Dispatcher,
		tempDir:    filepath.Clean(opts.TempDir),
		newID:      newID,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	projectSlug := strings.TrimSpace(chi.URLParam(r, "slug"))
	if !projectSlugPattern.MatchString(projectSlug) {
		h.logger.Warn("upload rejected",
			"project_slug", projectSlug,
			"error", "invalid project slug",
		)
		writeError(w, http.StatusBadRequest, "invalid_project_slug", "project slug must be URL-safe and non-empty")
		return
	}

	staged, err := h.stageUpload(r)
	if err != nil {
		h.logger.Warn("upload rejected",
			"project_slug", projectSlug,
			"error", err,
		)
		writeError(w, http.StatusBadRequest, "invalid_upload", err.Error())
		return
	}
	defer staged.cleanup()

	prepared, err := PrepareArchive(h.tempDir, staged.Path, staged.Filename, staged.ContentType)
	if err != nil {
		h.logger.Warn("upload rejected",
			"project_slug", projectSlug,
			"filename", staged.Filename,
			"error", err,
		)
		writeError(w, http.StatusBadRequest, "invalid_archive", err.Error())
		return
	}
	defer os.RemoveAll(prepared.ExtractedDir)

	reportID := strings.TrimSpace(h.newID())
	if reportID == "" {
		reportID = uuid.NewString()
	}
	objectKey := archiveObjectKey(projectSlug, reportID, prepared.Format)

	archiveFile, err := os.Open(staged.Path)
	if err != nil {
		h.logger.Error("upload rejected",
			"project_slug", projectSlug,
			"report_id", reportID,
			"object_key", objectKey,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, "archive_open_failed", "failed to reopen staged archive")
		return
	}
	defer archiveFile.Close()

	if err := h.storage.Upload(r.Context(), objectKey, archiveFile, prepared.Size, contentTypeForFormat(prepared.Format)); err != nil {
		h.logger.Error("upload storage failed",
			"project_slug", projectSlug,
			"report_id", reportID,
			"object_key", objectKey,
			"error", err,
		)
		writeError(w, http.StatusBadGateway, "storage_upload_failed", "failed to persist archive")
		return
	}

	report, err := h.store.CreateReport(r.Context(), db.CreateReportParams{
		ReportID:         reportID,
		ProjectSlug:      projectSlug,
		ArchiveObjectKey: objectKey,
		ArchiveFormat:    string(prepared.Format),
		SourceFilename:   prepared.SourceFilename,
	})
	if err != nil {
		if cleanupErr := h.storage.Delete(r.Context(), objectKey); cleanupErr != nil {
			h.logger.Error("upload metadata rollback failed",
				"project_slug", projectSlug,
				"report_id", reportID,
				"object_key", objectKey,
				"error", cleanupErr,
			)
		}
		h.logger.Error("upload metadata failed",
			"project_slug", projectSlug,
			"report_id", reportID,
			"object_key", objectKey,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, "metadata_write_failed", "failed to record pending report")
		return
	}

	if h.dispatcher != nil {
		resultsDir, err := h.prepareGenerationResultsDir(report, prepared.ExtractedDir)
		if err != nil {
			report = h.markGenerationDispatchFailed(r.Context(), report, objectKey, fmt.Errorf("prepare generation results dir: %w", err))
		} else if err := h.dispatcher.Enqueue(GenerationRequest{
			ProjectSlug: report.ProjectSlug,
			ReportID:    report.ID,
			ResultsDir:  resultsDir,
		}); err != nil {
			_ = os.RemoveAll(filepath.Dir(resultsDir))
			report = h.markGenerationDispatchFailed(r.Context(), report, objectKey, fmt.Errorf("enqueue generation: %w", err))
		}
	}

	h.logger.Info("upload accepted",
		"project_slug", report.ProjectSlug,
		"report_id", report.ID,
		"status", report.Status,
		"archive_format", report.ArchiveFormat,
		"object_key", report.ArchiveObjectKey,
	)

	writeJSON(w, http.StatusAccepted, uploadResponse{
		ReportID:         report.ID,
		ProjectSlug:      report.ProjectSlug,
		Status:           report.Status,
		ArchiveFormat:    report.ArchiveFormat,
		ArchiveObjectKey: report.ArchiveObjectKey,
	})
}

func (h *Handler) stageUpload(r *http.Request) (stagedUpload, error) {
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		return h.stageMultipartUpload(r)
	}
	return h.stageRawUpload(r)
}

func (h *Handler) stageMultipartUpload(r *http.Request) (stagedUpload, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return stagedUpload{}, fmt.Errorf("open multipart body: %w", err)
	}

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return stagedUpload{}, fmt.Errorf("multipart upload did not include a file part")
		}
		if err != nil {
			return stagedUpload{}, fmt.Errorf("read multipart body: %w", err)
		}

		if part.FileName() == "" {
			part.Close()
			continue
		}
		staged, stageErr := h.copyPartToTempFile(part)
		part.Close()
		return staged, stageErr
	}
}

func (h *Handler) stageRawUpload(r *http.Request) (stagedUpload, error) {
	filename := filenameFromHeaders(r.Header)
	if filename == "" {
		filename = "upload"
	}

	path, size, err := copyToTempFile(h.tempDir, r.Body)
	if err != nil {
		return stagedUpload{}, err
	}

	return stagedUpload{
		Path:        path,
		Filename:    filename,
		ContentType: r.Header.Get("Content-Type"),
		ContentSize: size,
		CleanupPaths: []string{
			path,
		},
	}, nil
}

func (h *Handler) copyPartToTempFile(part *multipart.Part) (stagedUpload, error) {
	path, size, err := copyToTempFile(h.tempDir, part)
	if err != nil {
		return stagedUpload{}, err
	}

	return stagedUpload{
		Path:        path,
		Filename:    part.FileName(),
		ContentType: part.Header.Get("Content-Type"),
		ContentSize: size,
		CleanupPaths: []string{
			path,
		},
	}, nil
}

func (s stagedUpload) cleanup() {
	for _, path := range s.CleanupPaths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

func copyToTempFile(tempDir string, src io.Reader) (string, int64, error) {
	file, err := os.CreateTemp(tempDir, "testimony-upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("create upload temp file: %w", err)
	}

	size, copyErr := io.Copy(file, src)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(file.Name())
		return "", 0, fmt.Errorf("stage upload body: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(file.Name())
		return "", 0, fmt.Errorf("close staged upload body: %w", closeErr)
	}
	if size == 0 {
		_ = os.Remove(file.Name())
		return "", 0, fmt.Errorf("upload body is empty")
	}

	return file.Name(), size, nil
}

func archiveObjectKey(projectSlug, reportID string, format ArchiveFormat) string {
	return fmt.Sprintf("projects/%s/reports/%s/archive.%s", projectSlug, reportID, archiveExtension(format))
}

func archiveExtension(format ArchiveFormat) string {
	switch format {
	case ArchiveFormatZIP:
		return "zip"
	case ArchiveFormatTarGz:
		return "tar.gz"
	default:
		return "bin"
	}
}

func contentTypeForFormat(format ArchiveFormat) string {
	switch format {
	case ArchiveFormatZIP:
		return "application/zip"
	case ArchiveFormatTarGz:
		return "application/gzip"
	default:
		return "application/octet-stream"
	}
}

func filenameFromHeaders(header http.Header) string {
	if value := strings.TrimSpace(header.Get("X-Filename")); value != "" {
		return filepath.Base(value)
	}

	if disposition := strings.TrimSpace(header.Get("Content-Disposition")); disposition != "" {
		_, params, err := mime.ParseMediaType(disposition)
		if err == nil {
			if filename := strings.TrimSpace(params["filename"]); filename != "" {
				return filepath.Base(filename)
			}
		}
	}

	return ""
}

func (h *Handler) prepareGenerationResultsDir(report db.Report, extractedDir string) (string, error) {
	trimmedSource := filepath.Clean(strings.TrimSpace(extractedDir))
	if trimmedSource == "" || trimmedSource == "." {
		return "", fmt.Errorf("empty extracted results dir")
	}

	reportRoot := filepath.Join(h.tempDir, "report-generation", report.ID)
	resultsDir := filepath.Join(reportRoot, "results")
	if err := os.RemoveAll(reportRoot); err != nil {
		return "", fmt.Errorf("remove prior report temp root %q: %w", reportRoot, err)
	}
	if err := os.MkdirAll(filepath.Dir(resultsDir), 0o755); err != nil {
		return "", fmt.Errorf("create report temp root %q: %w", reportRoot, err)
	}
	if err := os.Rename(trimmedSource, resultsDir); err != nil {
		return "", fmt.Errorf("move extracted results %q to %q: %w", trimmedSource, resultsDir, err)
	}
	return resultsDir, nil
}

func (h *Handler) markGenerationDispatchFailed(ctx context.Context, report db.Report, objectKey string, err error) db.Report {
	message := truncateGenerationFailure(err.Error())
	updated, updateErr := h.store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID:     report.ID,
		Status:       db.ReportStatusFailed,
		ErrorMessage: message,
	})
	if updateErr != nil {
		h.logger.Error("upload generation dispatch failed",
			"project_slug", report.ProjectSlug,
			"report_id", report.ID,
			"object_key", objectKey,
			"error", err,
			"status_update_error", updateErr,
		)
		return report
	}

	h.logger.Error("upload generation dispatch failed",
		"project_slug", updated.ProjectSlug,
		"report_id", updated.ID,
		"status", updated.Status,
		"object_key", objectKey,
		"error_message", updated.ErrorMessage,
		"error", err,
	)
	return updated
}

func truncateGenerationFailure(message string) string {
	const limit = 4096
	trimmed := strings.TrimSpace(message)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit-3] + "..."
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Error: message})
}
