package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/testimony-dev/testimony/internal/auth"
	_ "modernc.org/sqlite"
)

type Store interface {
	CreateProject(ctx context.Context, slug string) (Project, error)
	GetProject(ctx context.Context, slug string) (Project, error)
	ListProjects(ctx context.Context) ([]Project, error)
	SetProjectRetentionDays(ctx context.Context, projectSlug string, retentionDays *int) (Project, error)
	ResolveProjectRetentionDays(ctx context.Context, projectSlug string, globalRetentionDays int) (int, error)
	EnsureAPIKey(ctx context.Context, name, rawKey string) (APIKey, error)
	ValidateAPIKey(ctx context.Context, rawKey string) error
	CreateReport(ctx context.Context, params CreateReportParams) (Report, error)
	GetReport(ctx context.Context, reportID string) (Report, error)
	ListReports(ctx context.Context, projectSlug string) ([]Report, error)
	ListExpiredReports(ctx context.Context, globalRetentionDays int, now time.Time) ([]Report, error)
	DeleteReport(ctx context.Context, reportID string) error
	UpdateReportStatus(ctx context.Context, params UpdateReportStatusParams) (Report, error)
	Ready(ctx context.Context) error
	Close() error
}

type SQLiteStore struct {
	db          *sql.DB
	logger      *slog.Logger
	path        string
	busyTimeout time.Duration
}

func OpenSQLiteStore(ctx context.Context, path string, busyTimeout time.Duration, logger *slog.Logger) (*SQLiteStore, error) {
	cleanPath := strings.TrimSpace(path)
	if cleanPath == "" {
		return nil, fmt.Errorf("open sqlite store: empty path")
	}
	if busyTimeout <= 0 {
		return nil, fmt.Errorf("open sqlite store %q: busy timeout must be greater than zero", cleanPath)
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	if isFilesystemSQLitePath(cleanPath) {
		dir := filepath.Dir(cleanPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Error("sqlite open failed",
				"path", cleanPath,
				"error", err,
			)
			return nil, fmt.Errorf("create sqlite directory %q: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", cleanPath)
	if err != nil {
		logger.Error("sqlite open failed",
			"path", cleanPath,
			"error", err,
		)
		return nil, fmt.Errorf("open sqlite %q: %w", cleanPath, err)
	}

	store := &SQLiteStore{
		db:          db,
		logger:      logger,
		path:        cleanPath,
		busyTimeout: busyTimeout,
	}

	// Keep a single long-lived connection so busy_timeout and journal pragmas remain predictable
	// across local zero-config use.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := db.PingContext(ctx); err != nil {
		logger.Error("sqlite ping failed",
			"path", cleanPath,
			"error", err,
		)
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", cleanPath, err)
	}

	if err := RunMigrations(ctx, db, busyTimeout); err != nil {
		logger.Error("sqlite migration failed",
			"path", cleanPath,
			"error", err,
		)
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite %q: %w", cleanPath, err)
	}

	logger.Info("sqlite store ready",
		"path", cleanPath,
	)

	return store, nil
}

func (s *SQLiteStore) Ready(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sqlite store is closed")
	}

	if err := s.db.PingContext(ctx); err != nil {
		s.logger.Error("sqlite readiness check failed",
			"path", s.path,
			"error", err,
		)
		return fmt.Errorf("ping sqlite %q: %w", s.path, err)
	}

	return nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) EnsureAPIKey(ctx context.Context, name, rawKey string) (APIKey, error) {
	normalizedName := strings.TrimSpace(name)
	if normalizedName == "" {
		return APIKey{}, fmt.Errorf("ensure api key: empty name")
	}

	hashedKey, err := auth.HashAPIKey(rawKey)
	if err != nil {
		return APIKey{}, fmt.Errorf("ensure api key %q: %w", normalizedName, err)
	}

	var ensured APIKey
	if err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		existing, err := s.scanAPIKey(tx.QueryRowContext(ctx, `
SELECT id, name, key_hash, created_at, updated_at
FROM api_keys
WHERE name = ?
`, normalizedName))
		switch {
		case err == nil:
			if existing.KeyHash == hashedKey {
				ensured = existing
				return nil
			}

			updatedAt := nowUTC()
			if _, err := tx.ExecContext(ctx, `
UPDATE api_keys
SET key_hash = ?, updated_at = ?
WHERE name = ?
`, hashedKey, formatTimestamp(updatedAt), normalizedName); err != nil {
				return fmt.Errorf("update api key %q: %w", normalizedName, err)
			}

			existing.KeyHash = hashedKey
			existing.UpdatedAt = updatedAt
			ensured = existing
			return nil
		case errors.Is(err, sql.ErrNoRows):
			now := nowUTC()
			ensured = APIKey{
				ID:        uuid.NewString(),
				Name:      normalizedName,
				KeyHash:   hashedKey,
				CreatedAt: now,
				UpdatedAt: now,
			}

			if _, err := tx.ExecContext(ctx, `
INSERT INTO api_keys (id, name, key_hash, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
`, ensured.ID, ensured.Name, ensured.KeyHash, formatTimestamp(ensured.CreatedAt), formatTimestamp(ensured.UpdatedAt)); err != nil {
				return fmt.Errorf("insert api key %q: %w", normalizedName, err)
			}
			return nil
		default:
			return fmt.Errorf("query api key %q: %w", normalizedName, err)
		}
	}); err != nil {
		s.logger.Error("ensure api key failed",
			"name", normalizedName,
			"error", err,
		)
		return APIKey{}, fmt.Errorf("ensure api key %q: %w", normalizedName, err)
	}

	return ensured, nil
}

func (s *SQLiteStore) ValidateAPIKey(ctx context.Context, rawKey string) error {
	hashedKey, err := auth.HashAPIKey(rawKey)
	if err != nil {
		return fmt.Errorf("validate api key: %w", auth.ErrInvalidAPIKey)
	}

	if _, err := s.scanAPIKey(s.db.QueryRowContext(ctx, `
SELECT id, name, key_hash, created_at, updated_at
FROM api_keys
WHERE key_hash = ?
`, hashedKey)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("validate api key: %w", auth.ErrInvalidAPIKey)
		}
		return fmt.Errorf("validate api key: %w", err)
	}

	return nil
}

func (s *SQLiteStore) CreateProject(ctx context.Context, slug string) (Project, error) {
	normalizedSlug := normalizeSlug(slug)
	if normalizedSlug == "" {
		return Project{}, fmt.Errorf("create project: empty slug")
	}

	project, err := s.createProjectIfMissing(ctx, s.db, normalizedSlug)
	if err != nil {
		s.logger.Error("create project failed",
			"project_slug", normalizedSlug,
			"error", err,
		)
		return Project{}, fmt.Errorf("create project %q: %w", normalizedSlug, err)
	}

	return project, nil
}

func (s *SQLiteStore) GetProject(ctx context.Context, slug string) (Project, error) {
	normalizedSlug := normalizeSlug(slug)
	if normalizedSlug == "" {
		return Project{}, fmt.Errorf("get project: empty slug")
	}

	project, err := s.scanProject(ctx, s.db.QueryRowContext(ctx, `
SELECT id, slug, retention_days, created_at, updated_at
FROM projects
WHERE slug = ?
`, normalizedSlug))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, fmt.Errorf("get project %q: %w", normalizedSlug, ErrProjectNotFound)
		}
		return Project{}, fmt.Errorf("get project %q: %w", normalizedSlug, err)
	}

	return project, nil
}

func (s *SQLiteStore) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, slug, retention_days, created_at, updated_at
FROM projects
ORDER BY slug ASC
`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	projects := make([]Project, 0)
	for rows.Next() {
		project, err := s.scanProject(ctx, rows)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		projects = append(projects, project)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects rows: %w", err)
	}

	return projects, nil
}

func (s *SQLiteStore) SetProjectRetentionDays(ctx context.Context, projectSlug string, retentionDays *int) (Project, error) {
	normalizedSlug := normalizeSlug(projectSlug)
	if normalizedSlug == "" {
		return Project{}, fmt.Errorf("set project retention: empty project slug")
	}
	if retentionDays != nil && *retentionDays < 0 {
		return Project{}, fmt.Errorf("set project retention for %q: retention days must be zero or greater", normalizedSlug)
	}

	updatedAt := nowUTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE projects
SET retention_days = ?, updated_at = ?
WHERE slug = ?
`, nullableInt(retentionDays), formatTimestamp(updatedAt), normalizedSlug)
	if err != nil {
		s.logger.Error("set project retention failed",
			"project_slug", normalizedSlug,
			"error", err,
		)
		return Project{}, fmt.Errorf("set project retention for %q: %w", normalizedSlug, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Project{}, fmt.Errorf("set project retention for %q: %w", normalizedSlug, err)
	}
	if rowsAffected == 0 {
		return Project{}, fmt.Errorf("set project retention for %q: %w", normalizedSlug, ErrProjectNotFound)
	}

	project, err := s.GetProject(ctx, normalizedSlug)
	if err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *SQLiteStore) ResolveProjectRetentionDays(ctx context.Context, projectSlug string, globalRetentionDays int) (int, error) {
	if globalRetentionDays < 0 {
		return 0, fmt.Errorf("resolve project retention: global retention days must be zero or greater")
	}

	project, err := s.GetProject(ctx, projectSlug)
	if err != nil {
		return 0, err
	}
	if project.RetentionDays != nil {
		return *project.RetentionDays, nil
	}
	return globalRetentionDays, nil
}

func (s *SQLiteStore) CreateReport(ctx context.Context, params CreateReportParams) (Report, error) {
	projectSlug := normalizeSlug(params.ProjectSlug)
	if projectSlug == "" {
		return Report{}, fmt.Errorf("create report: empty project slug")
	}
	if strings.TrimSpace(params.ArchiveObjectKey) == "" {
		return Report{}, fmt.Errorf("create report for %q: empty archive object key", projectSlug)
	}

	var report Report
	if err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		project, err := s.createProjectIfMissing(ctx, tx, projectSlug)
		if err != nil {
			return err
		}

		now := nowUTC()
		report = Report{
			ID:               strings.TrimSpace(params.ReportID),
			ProjectID:        project.ID,
			ProjectSlug:      project.Slug,
			Status:           ReportStatusPending,
			ArchiveObjectKey: strings.TrimSpace(params.ArchiveObjectKey),
			ArchiveFormat:    strings.TrimSpace(params.ArchiveFormat),
			SourceFilename:   strings.TrimSpace(params.SourceFilename),
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if report.ID == "" {
			report.ID = uuid.NewString()
		}

		if _, err := tx.ExecContext(ctx, `
INSERT INTO reports (
    id,
    project_id,
    status,
    archive_object_key,
    generated_object_key,
    archive_format,
    source_filename,
    error_message,
    created_at,
    updated_at,
    started_at,
    completed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
			report.ID,
			report.ProjectID,
			report.Status,
			report.ArchiveObjectKey,
			"",
			report.ArchiveFormat,
			report.SourceFilename,
			"",
			formatTimestamp(report.CreatedAt),
			formatTimestamp(report.UpdatedAt),
			nil,
			nil,
		); err != nil {
			return fmt.Errorf("insert report: %w", err)
		}

		return nil
	}); err != nil {
		s.logger.Error("create report failed",
			"project_slug", projectSlug,
			"archive_object_key", params.ArchiveObjectKey,
			"error", err,
		)
		return Report{}, fmt.Errorf("create report for project %q: %w", projectSlug, err)
	}

	s.logger.Info("report persisted",
		"project_slug", report.ProjectSlug,
		"report_id", report.ID,
		"status", report.Status,
		"archive_object_key", report.ArchiveObjectKey,
	)

	return report, nil
}

func (s *SQLiteStore) GetReport(ctx context.Context, reportID string) (Report, error) {
	normalizedID := strings.TrimSpace(reportID)
	if normalizedID == "" {
		return Report{}, fmt.Errorf("get report: empty report id")
	}

	report, err := s.scanReport(ctx, s.db.QueryRowContext(ctx, `
SELECT
    r.id,
    r.project_id,
    p.slug,
    r.status,
    r.archive_object_key,
    r.generated_object_key,
    r.archive_format,
    r.source_filename,
    r.error_message,
    r.created_at,
    r.updated_at,
    r.started_at,
    r.completed_at
FROM reports r
JOIN projects p ON p.id = r.project_id
WHERE r.id = ?
`, normalizedID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Report{}, fmt.Errorf("get report %q: %w", normalizedID, ErrReportNotFound)
		}
		return Report{}, fmt.Errorf("get report %q: %w", normalizedID, err)
	}

	return report, nil
}

func (s *SQLiteStore) ListReports(ctx context.Context, projectSlug string) ([]Report, error) {
	normalizedSlug := normalizeSlug(projectSlug)
	if normalizedSlug == "" {
		return nil, fmt.Errorf("list reports: empty project slug")
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT
    r.id,
    r.project_id,
    p.slug,
    r.status,
    r.archive_object_key,
    r.generated_object_key,
    r.archive_format,
    r.source_filename,
    r.error_message,
    r.created_at,
    r.updated_at,
    r.started_at,
    r.completed_at
FROM reports r
JOIN projects p ON p.id = r.project_id
WHERE p.slug = ?
ORDER BY r.created_at DESC, r.id DESC
`, normalizedSlug)
	if err != nil {
		return nil, fmt.Errorf("list reports for %q: %w", normalizedSlug, err)
	}
	defer rows.Close()

	reports := make([]Report, 0)
	for rows.Next() {
		report, err := s.scanReport(ctx, rows)
		if err != nil {
			return nil, fmt.Errorf("list reports for %q: %w", normalizedSlug, err)
		}
		reports = append(reports, report)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list reports rows for %q: %w", normalizedSlug, err)
	}

	return reports, nil
}

func (s *SQLiteStore) ListExpiredReports(ctx context.Context, globalRetentionDays int, now time.Time) ([]Report, error) {
	if globalRetentionDays < 0 {
		return nil, fmt.Errorf("list expired reports: global retention days must be zero or greater")
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT
    r.id,
    r.project_id,
    p.slug,
    r.status,
    r.archive_object_key,
    r.generated_object_key,
    r.archive_format,
    r.source_filename,
    r.error_message,
    r.created_at,
    r.updated_at,
    r.started_at,
    r.completed_at,
    p.retention_days
FROM reports r
JOIN projects p ON p.id = r.project_id
WHERE r.completed_at IS NOT NULL
  AND r.status IN (?, ?)
ORDER BY r.completed_at ASC, r.id ASC
`, ReportStatusReady, ReportStatusFailed)
	if err != nil {
		return nil, fmt.Errorf("list expired reports: %w", err)
	}
	defer rows.Close()

	reports := make([]Report, 0)
	referenceTime := now.UTC()
	for rows.Next() {
		candidate, retentionDays, err := s.scanReportWithRetention(rows)
		if err != nil {
			return nil, fmt.Errorf("list expired reports: %w", err)
		}

		effectiveRetentionDays := globalRetentionDays
		if retentionDays != nil {
			effectiveRetentionDays = *retentionDays
		}
		if effectiveRetentionDays <= 0 || candidate.CompletedAt == nil {
			continue
		}

		expiresAt := candidate.CompletedAt.UTC().Add(time.Duration(effectiveRetentionDays) * 24 * time.Hour)
		if expiresAt.After(referenceTime) {
			continue
		}

		reports = append(reports, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired reports rows: %w", err)
	}

	return reports, nil
}

func (s *SQLiteStore) DeleteReport(ctx context.Context, reportID string) error {
	normalizedID := strings.TrimSpace(reportID)
	if normalizedID == "" {
		return fmt.Errorf("delete report: empty report id")
	}

	result, err := s.db.ExecContext(ctx, `DELETE FROM reports WHERE id = ?`, normalizedID)
	if err != nil {
		return fmt.Errorf("delete report %q: %w", normalizedID, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete report %q: %w", normalizedID, err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("delete report %q: %w", normalizedID, ErrReportNotFound)
	}

	return nil
}

func (s *SQLiteStore) UpdateReportStatus(ctx context.Context, params UpdateReportStatusParams) (Report, error) {
	normalizedID := strings.TrimSpace(params.ReportID)
	if normalizedID == "" {
		return Report{}, fmt.Errorf("update report status: empty report id")
	}
	if !params.Status.Valid() {
		return Report{}, fmt.Errorf("update report %q: invalid status %q", normalizedID, params.Status)
	}

	var updated Report
	if err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		current, err := s.getReportTx(ctx, tx, normalizedID)
		if err != nil {
			return err
		}

		updated = current
		updated.Status = params.Status
		updated.UpdatedAt = nowUTC()
		if trimmed := strings.TrimSpace(params.GeneratedObjectKey); trimmed != "" {
			updated.GeneratedObjectKey = trimmed
		}

		switch params.Status {
		case ReportStatusPending:
			updated.StartedAt = nil
			updated.CompletedAt = nil
			updated.ErrorMessage = ""
		case ReportStatusProcessing:
			if updated.StartedAt == nil {
				started := updated.UpdatedAt
				updated.StartedAt = &started
			}
			updated.CompletedAt = nil
			updated.ErrorMessage = ""
		case ReportStatusReady:
			if updated.StartedAt == nil {
				started := updated.UpdatedAt
				updated.StartedAt = &started
			}
			completed := updated.UpdatedAt
			if params.CompletedAtOverride != nil {
				completed = params.CompletedAtOverride.UTC()
			}
			updated.CompletedAt = &completed
			updated.ErrorMessage = ""
		case ReportStatusFailed:
			if updated.StartedAt == nil {
				started := updated.UpdatedAt
				updated.StartedAt = &started
			}
			completed := updated.UpdatedAt
			if params.CompletedAtOverride != nil {
				completed = params.CompletedAtOverride.UTC()
			}
			updated.CompletedAt = &completed
			updated.ErrorMessage = strings.TrimSpace(params.ErrorMessage)
		}

		if _, err := tx.ExecContext(ctx, `
UPDATE reports
SET
    status = ?,
    generated_object_key = ?,
    error_message = ?,
    updated_at = ?,
    started_at = ?,
    completed_at = ?
WHERE id = ?
`,
			updated.Status,
			updated.GeneratedObjectKey,
			updated.ErrorMessage,
			formatTimestamp(updated.UpdatedAt),
			nullableTimestamp(updated.StartedAt),
			nullableTimestamp(updated.CompletedAt),
			normalizedID,
		); err != nil {
			return fmt.Errorf("update report %q status: %w", normalizedID, err)
		}

		return nil
	}); err != nil {
		if errors.Is(err, ErrReportNotFound) {
			return Report{}, err
		}
		s.logger.Error("update report status failed",
			"report_id", normalizedID,
			"status", params.Status,
			"error", err,
		)
		return Report{}, fmt.Errorf("update report %q status: %w", normalizedID, err)
	}

	s.logger.Info("report status changed",
		"project_slug", updated.ProjectSlug,
		"report_id", updated.ID,
		"status", updated.Status,
		"generated_object_key", updated.GeneratedObjectKey,
	)

	return updated, nil
}

func (s *SQLiteStore) getReportTx(ctx context.Context, tx *sql.Tx, reportID string) (Report, error) {
	report, err := s.scanReport(ctx, tx.QueryRowContext(ctx, `
SELECT
    r.id,
    r.project_id,
    p.slug,
    r.status,
    r.archive_object_key,
    r.generated_object_key,
    r.archive_format,
    r.source_filename,
    r.error_message,
    r.created_at,
    r.updated_at,
    r.started_at,
    r.completed_at
FROM reports r
JOIN projects p ON p.id = r.project_id
WHERE r.id = ?
`, reportID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Report{}, fmt.Errorf("get report %q: %w", reportID, ErrReportNotFound)
		}
		return Report{}, fmt.Errorf("get report %q: %w", reportID, err)
	}

	return report, nil
}

func (s *SQLiteStore) createProjectIfMissing(ctx context.Context, exec sqlExecutor, slug string) (Project, error) {
	now := nowUTC()
	if _, err := exec.ExecContext(ctx, `
INSERT INTO projects (id, slug, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(slug) DO NOTHING
`, uuid.NewString(), slug, formatTimestamp(now), formatTimestamp(now)); err != nil {
		return Project{}, fmt.Errorf("insert project %q: %w", slug, err)
	}

	return s.scanProject(ctx, exec.QueryRowContext(ctx, `
SELECT id, slug, retention_days, created_at, updated_at
FROM projects
WHERE slug = ?
`, slug))
}

func (s *SQLiteStore) scanProject(_ context.Context, scanner rowScanner) (Project, error) {
	var project Project
	var retentionDays sql.NullInt64
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(&project.ID, &project.Slug, &retentionDays, &createdAt, &updatedAt); err != nil {
		return Project{}, err
	}

	parsedCreatedAt, err := parseTimestamp(createdAt)
	if err != nil {
		return Project{}, fmt.Errorf("parse project created_at: %w", err)
	}
	parsedUpdatedAt, err := parseTimestamp(updatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("parse project updated_at: %w", err)
	}

	project.RetentionDays = nullableIntFromSQL(retentionDays)
	project.CreatedAt = parsedCreatedAt
	project.UpdatedAt = parsedUpdatedAt
	return project, nil
}

func (s *SQLiteStore) scanAPIKey(scanner rowScanner) (APIKey, error) {
	var apiKey APIKey
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(&apiKey.ID, &apiKey.Name, &apiKey.KeyHash, &createdAt, &updatedAt); err != nil {
		return APIKey{}, err
	}

	parsedCreatedAt, err := parseTimestamp(createdAt)
	if err != nil {
		return APIKey{}, fmt.Errorf("parse api key created_at: %w", err)
	}
	parsedUpdatedAt, err := parseTimestamp(updatedAt)
	if err != nil {
		return APIKey{}, fmt.Errorf("parse api key updated_at: %w", err)
	}

	apiKey.CreatedAt = parsedCreatedAt
	apiKey.UpdatedAt = parsedUpdatedAt
	return apiKey, nil
}

func (s *SQLiteStore) scanReport(_ context.Context, scanner rowScanner) (Report, error) {
	var report Report
	var createdAt string
	var updatedAt string
	var startedAt sql.NullString
	var completedAt sql.NullString
	if err := scanner.Scan(
		&report.ID,
		&report.ProjectID,
		&report.ProjectSlug,
		&report.Status,
		&report.ArchiveObjectKey,
		&report.GeneratedObjectKey,
		&report.ArchiveFormat,
		&report.SourceFilename,
		&report.ErrorMessage,
		&createdAt,
		&updatedAt,
		&startedAt,
		&completedAt,
	); err != nil {
		return Report{}, err
	}

	parsedCreatedAt, err := parseTimestamp(createdAt)
	if err != nil {
		return Report{}, fmt.Errorf("parse report created_at: %w", err)
	}
	parsedUpdatedAt, err := parseTimestamp(updatedAt)
	if err != nil {
		return Report{}, fmt.Errorf("parse report updated_at: %w", err)
	}

	report.CreatedAt = parsedCreatedAt
	report.UpdatedAt = parsedUpdatedAt
	report.StartedAt, err = parseNullableTimestamp(startedAt)
	if err != nil {
		return Report{}, fmt.Errorf("parse report started_at: %w", err)
	}
	report.CompletedAt, err = parseNullableTimestamp(completedAt)
	if err != nil {
		return Report{}, fmt.Errorf("parse report completed_at: %w", err)
	}

	return report, nil
}

func (s *SQLiteStore) scanReportWithRetention(scanner rowScanner) (Report, *int, error) {
	var report Report
	var createdAt string
	var updatedAt string
	var startedAt sql.NullString
	var completedAt sql.NullString
	var retentionDays sql.NullInt64
	if err := scanner.Scan(
		&report.ID,
		&report.ProjectID,
		&report.ProjectSlug,
		&report.Status,
		&report.ArchiveObjectKey,
		&report.GeneratedObjectKey,
		&report.ArchiveFormat,
		&report.SourceFilename,
		&report.ErrorMessage,
		&createdAt,
		&updatedAt,
		&startedAt,
		&completedAt,
		&retentionDays,
	); err != nil {
		return Report{}, nil, err
	}

	parsedCreatedAt, err := parseTimestamp(createdAt)
	if err != nil {
		return Report{}, nil, fmt.Errorf("parse report created_at: %w", err)
	}
	parsedUpdatedAt, err := parseTimestamp(updatedAt)
	if err != nil {
		return Report{}, nil, fmt.Errorf("parse report updated_at: %w", err)
	}

	report.CreatedAt = parsedCreatedAt
	report.UpdatedAt = parsedUpdatedAt
	report.StartedAt, err = parseNullableTimestamp(startedAt)
	if err != nil {
		return Report{}, nil, fmt.Errorf("parse report started_at: %w", err)
	}
	report.CompletedAt, err = parseNullableTimestamp(completedAt)
	if err != nil {
		return Report{}, nil, fmt.Errorf("parse report completed_at: %w", err)
	}

	return report, nullableIntFromSQL(retentionDays), nil
}

func parseTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func parseNullableTimestamp(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	parsed, err := parseTimestamp(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func nullableTimestamp(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTimestamp(value.UTC())
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableIntFromSQL(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func isFilesystemSQLitePath(path string) bool {
	return path != ":memory:" && !strings.HasPrefix(path, "file:")
}

func withTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
