package generate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/testimony-dev/testimony/internal/db"
)

const (
	defaultFailureMessageLimit = 4096
	htmlObjectContentType      = "text/html; charset=utf-8"
)

type ServiceReportStore interface {
	ListReports(ctx context.Context, projectSlug string) ([]db.Report, error)
	UpdateReportStatus(ctx context.Context, params db.UpdateReportStatusParams) (db.Report, error)
}

type ServiceObjectStore interface {
	HistoryStore
	Upload(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
}

type ServiceOptions struct {
	Logger         *slog.Logger
	Store          ServiceReportStore
	Storage        ServiceObjectStore
	Generator      Generator
	TempDir        string
	MaxConcurrency int
}

type Service struct {
	logger         *slog.Logger
	store          ServiceReportStore
	storage        ServiceObjectStore
	generator      Generator
	tempDir        string
	maxConcurrency int
	semaphore      chan struct{}
}

type Job struct {
	ProjectSlug string
	ReportID    string
	ResultsDir  string
}

func NewService(opts ServiceOptions) (*Service, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("new generation service: nil report store")
	}
	if opts.Storage == nil {
		return nil, fmt.Errorf("new generation service: nil object store")
	}
	if opts.Generator == nil {
		return nil, fmt.Errorf("new generation service: nil generator")
	}
	if opts.MaxConcurrency <= 0 {
		return nil, fmt.Errorf("new generation service: max concurrency must be greater than zero")
	}

	tempDir := filepath.Clean(strings.TrimSpace(opts.TempDir))
	if tempDir == "" || tempDir == "." {
		return nil, fmt.Errorf("new generation service: empty temp dir")
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, fmt.Errorf("new generation service: create temp dir %q: %w", tempDir, err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	return &Service{
		logger:         logger,
		store:          opts.Store,
		storage:        opts.Storage,
		generator:      opts.Generator,
		tempDir:        tempDir,
		maxConcurrency: opts.MaxConcurrency,
		semaphore:      make(chan struct{}, opts.MaxConcurrency),
	}, nil
}

func (s *Service) Enqueue(job Job) error {
	if err := s.validateJob(job); err != nil {
		return err
	}

	queuedJob := Job{
		ProjectSlug: strings.TrimSpace(job.ProjectSlug),
		ReportID:    strings.TrimSpace(job.ReportID),
		ResultsDir:  filepath.Clean(strings.TrimSpace(job.ResultsDir)),
	}
	go s.runJob(queuedJob)
	return nil
}

func (s *Service) validateJob(job Job) error {
	if strings.TrimSpace(job.ProjectSlug) == "" {
		return fmt.Errorf("enqueue generation job: empty project slug")
	}
	if strings.TrimSpace(job.ReportID) == "" {
		return fmt.Errorf("enqueue generation job: empty report id")
	}
	resultsDir := filepath.Clean(strings.TrimSpace(job.ResultsDir))
	if resultsDir == "" || resultsDir == "." {
		return fmt.Errorf("enqueue generation job: empty results dir")
	}
	info, err := os.Stat(resultsDir)
	if err != nil {
		return fmt.Errorf("enqueue generation job: stat results dir %q: %w", resultsDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("enqueue generation job: results dir %q is not a directory", resultsDir)
	}
	return nil
}

func (s *Service) runJob(job Job) {
	s.semaphore <- struct{}{}
	defer func() { <-s.semaphore }()

	reportRoot := filepath.Dir(job.ResultsDir)
	defer func() {
		if err := os.RemoveAll(reportRoot); err != nil {
			s.logger.Warn("report_generation_cleanup_failed",
				"project_slug", job.ProjectSlug,
				"report_id", job.ReportID,
				"report_root", reportRoot,
				"error", err,
			)
		}
	}()

	ctx := context.Background()
	processingReport, err := s.store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID: job.ReportID,
		Status:   db.ReportStatusProcessing,
	})
	if err != nil {
		s.logger.Error("report_generation_failed",
			"project_slug", job.ProjectSlug,
			"report_id", job.ReportID,
			"error", err,
		)
		return
	}

	s.logger.Info("report_generation_started",
		"project_slug", processingReport.ProjectSlug,
		"report_id", processingReport.ID,
		"status", processingReport.Status,
	)

	readyReports, err := s.store.ListReports(ctx, job.ProjectSlug)
	if err != nil {
		s.failJob(ctx, job, fmt.Errorf("list ready reports: %w", err))
		return
	}

	result, err := s.generator.Generate(ctx, Request{
		ProjectSlug:  job.ProjectSlug,
		ReportID:     job.ReportID,
		ResultsDir:   job.ResultsDir,
		WorkDir:      filepath.Join(reportRoot, "work"),
		ReadyReports: readyReports,
	})
	if err != nil {
		s.failJob(ctx, job, err)
		return
	}

	generatedObjectKey, err := s.publishOutput(ctx, job, result.OutputDir)
	if err != nil {
		s.failJob(ctx, job, err)
		return
	}

	readyReport, err := s.store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID:           job.ReportID,
		Status:             db.ReportStatusReady,
		GeneratedObjectKey: generatedObjectKey,
	})
	if err != nil {
		s.failJob(ctx, job, fmt.Errorf("mark report ready: %w", err))
		return
	}

	s.logger.Info("report_generation_completed",
		"project_slug", readyReport.ProjectSlug,
		"report_id", readyReport.ID,
		"status", readyReport.Status,
		"generated_object_key", readyReport.GeneratedObjectKey,
		"history_source_count", len(result.HistorySources),
		"variant", result.Variant,
	)
}

func (s *Service) failJob(ctx context.Context, job Job, err error) {
	message := truncateFailureMessage(err.Error(), defaultFailureMessageLimit)
	failedReport, updateErr := s.store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID:     job.ReportID,
		Status:       db.ReportStatusFailed,
		ErrorMessage: message,
	})
	if updateErr != nil {
		s.logger.Error("report_generation_failed",
			"project_slug", job.ProjectSlug,
			"report_id", job.ReportID,
			"error", err,
			"status_update_error", updateErr,
		)
		return
	}

	s.logger.Error("report_generation_failed",
		"project_slug", failedReport.ProjectSlug,
		"report_id", failedReport.ID,
		"status", failedReport.Status,
		"error_message", failedReport.ErrorMessage,
		"error", err,
	)
}

func (s *Service) publishOutput(ctx context.Context, job Job, outputDir string) (string, error) {
	prefix := reportHTMLPrefixForJob(job)
	generatedObjectKey := reportGeneratedObjectKey(job.ProjectSlug, job.ReportID)

	err := filepath.WalkDir(outputDir, func(filePath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("publish report output: unsupported symlink %q", filePath)
		}
		if d.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(outputDir, filePath)
		if err != nil {
			return fmt.Errorf("resolve output path %q: %w", filePath, err)
		}
		if relPath == "." || relPath == "" || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
			return fmt.Errorf("publish report output: invalid relative path %q", relPath)
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat output file %q: %w", filePath, err)
		}

		body, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("open output file %q: %w", filePath, err)
		}

		objectKey := prefix + filepath.ToSlash(relPath)
		uploadErr := s.storage.Upload(ctx, objectKey, body, info.Size(), contentTypeForObject(filePath))
		closeErr := body.Close()
		if uploadErr != nil {
			return fmt.Errorf("upload generated object %q: %w", objectKey, uploadErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close output file %q: %w", filePath, closeErr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	return generatedObjectKey, nil
}

func reportHTMLPrefixForJob(job Job) string {
	return fmt.Sprintf("projects/%s/reports/%s/html/", strings.TrimSpace(job.ProjectSlug), strings.TrimSpace(job.ReportID))
}

func contentTypeForObject(filePath string) string {
	if strings.EqualFold(filepath.Ext(filePath), ".html") {
		return htmlObjectContentType
	}
	if detected := strings.TrimSpace(mime.TypeByExtension(filepath.Ext(filePath))); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

func truncateFailureMessage(message string, limit int) string {
	trimmed := strings.TrimSpace(message)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	if limit <= 3 {
		return trimmed[:limit]
	}
	return trimmed[:limit-3] + "..."
}

func reportGeneratedObjectKey(projectSlug, reportID string) string {
	return path.Join("projects", strings.TrimSpace(projectSlug), "reports", strings.TrimSpace(reportID), "html", "index.html")
}
