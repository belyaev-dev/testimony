package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testimony-dev/testimony/internal/auth"
	"github.com/testimony-dev/testimony/internal/db"
	_ "modernc.org/sqlite"
)

func TestSQLiteStorePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "testimony.sqlite")

	store := openStore(t, ctx, path)
	project, err := store.CreateProject(ctx, "backend-tests")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}

	report, err := store.CreateReport(ctx, db.CreateReportParams{
		ProjectSlug:      project.Slug,
		ArchiveObjectKey: "projects/backend-tests/reports/report-1/archive.zip",
		ArchiveFormat:    "zip",
		SourceFilename:   "results.zip",
	})
	if err != nil {
		t.Fatalf("CreateReport() error = %v", err)
	}

	updated, err := store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID:           report.ID,
		Status:             db.ReportStatusReady,
		GeneratedObjectKey: "projects/backend-tests/reports/report-1/html/index.html",
	})
	if err != nil {
		t.Fatalf("UpdateReportStatus() error = %v", err)
	}
	if updated.CompletedAt == nil {
		t.Fatal("CompletedAt = nil, want non-nil")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened := openStore(t, ctx, path)
	defer reopened.Close()

	gotProject, err := reopened.GetProject(ctx, "backend-tests")
	if err != nil {
		t.Fatalf("GetProject() after reopen error = %v", err)
	}
	if gotProject.ID != project.ID {
		t.Fatalf("project ID after reopen = %q, want %q", gotProject.ID, project.ID)
	}

	gotReport, err := reopened.GetReport(ctx, report.ID)
	if err != nil {
		t.Fatalf("GetReport() after reopen error = %v", err)
	}
	if gotReport.ProjectSlug != project.Slug {
		t.Fatalf("report ProjectSlug after reopen = %q, want %q", gotReport.ProjectSlug, project.Slug)
	}
	if gotReport.Status != db.ReportStatusReady {
		t.Fatalf("report Status after reopen = %q, want %q", gotReport.Status, db.ReportStatusReady)
	}
	if gotReport.GeneratedObjectKey != updated.GeneratedObjectKey {
		t.Fatalf("GeneratedObjectKey after reopen = %q, want %q", gotReport.GeneratedObjectKey, updated.GeneratedObjectKey)
	}
}

func TestCreateReportAutoCreatesProjectAndIsolatesBySlug(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "testimony.sqlite"))
	defer store.Close()

	alphaReport, err := store.CreateReport(ctx, db.CreateReportParams{
		ProjectSlug:      "alpha",
		ArchiveObjectKey: "projects/alpha/reports/a1/archive.tar.gz",
		ArchiveFormat:    "tar.gz",
		SourceFilename:   "alpha-results.tar.gz",
	})
	if err != nil {
		t.Fatalf("CreateReport(alpha) error = %v", err)
	}
	betaReport, err := store.CreateReport(ctx, db.CreateReportParams{
		ProjectSlug:      "beta",
		ArchiveObjectKey: "projects/beta/reports/b1/archive.zip",
		ArchiveFormat:    "zip",
		SourceFilename:   "beta-results.zip",
	})
	if err != nil {
		t.Fatalf("CreateReport(beta) error = %v", err)
	}

	alphaReports, err := store.ListReports(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListReports(alpha) error = %v", err)
	}
	if len(alphaReports) != 1 {
		t.Fatalf("len(ListReports(alpha)) = %d, want 1", len(alphaReports))
	}
	if got := alphaReports[0].ID; got != alphaReport.ID {
		t.Fatalf("alpha report ID = %q, want %q", got, alphaReport.ID)
	}
	if got := alphaReports[0].ProjectSlug; got != "alpha" {
		t.Fatalf("alpha report ProjectSlug = %q, want %q", got, "alpha")
	}

	betaReports, err := store.ListReports(ctx, "beta")
	if err != nil {
		t.Fatalf("ListReports(beta) error = %v", err)
	}
	if len(betaReports) != 1 {
		t.Fatalf("len(ListReports(beta)) = %d, want 1", len(betaReports))
	}
	if got := betaReports[0].ID; got != betaReport.ID {
		t.Fatalf("beta report ID = %q, want %q", got, betaReport.ID)
	}

	if _, err := store.GetProject(ctx, "alpha"); err != nil {
		t.Fatalf("GetProject(alpha) error = %v", err)
	}
	if _, err := store.GetProject(ctx, "beta"); err != nil {
		t.Fatalf("GetProject(beta) error = %v", err)
	}
}

func TestEnsureAPIKeyStoresOnlyHashAndValidates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "testimony.sqlite")
	store := openStore(t, ctx, path)
	defer store.Close()

	rawKey := "bootstrap-key-test"
	expectedHash, err := auth.HashAPIKey(rawKey)
	if err != nil {
		t.Fatalf("HashAPIKey() error = %v", err)
	}

	apiKey, err := store.EnsureAPIKey(ctx, db.APIKeyBootstrapName, rawKey)
	if err != nil {
		t.Fatalf("EnsureAPIKey() error = %v", err)
	}
	if got, want := apiKey.Name, db.APIKeyBootstrapName; got != want {
		t.Fatalf("apiKey.Name = %q, want %q", got, want)
	}
	if got, want := apiKey.KeyHash, expectedHash; got != want {
		t.Fatalf("apiKey.KeyHash = %q, want %q", got, want)
	}
	if apiKey.KeyHash == rawKey {
		t.Fatal("apiKey.KeyHash persisted plaintext key")
	}
	if err := store.ValidateAPIKey(ctx, rawKey); err != nil {
		t.Fatalf("ValidateAPIKey(valid) error = %v", err)
	}
	if err := store.ValidateAPIKey(ctx, "wrong-key"); !errors.Is(err, auth.ErrInvalidAPIKey) {
		t.Fatalf("ValidateAPIKey(invalid) error = %v, want ErrInvalidAPIKey", err)
	}

	sqliteDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer sqliteDB.Close()

	var storedName string
	var storedHash string
	if err := sqliteDB.QueryRowContext(ctx, `
SELECT name, key_hash
FROM api_keys
WHERE name = ?
`, db.APIKeyBootstrapName).Scan(&storedName, &storedHash); err != nil {
		t.Fatalf("query api_keys row error = %v", err)
	}
	if got, want := storedName, db.APIKeyBootstrapName; got != want {
		t.Fatalf("stored name = %q, want %q", got, want)
	}
	if got, want := storedHash, expectedHash; got != want {
		t.Fatalf("stored key_hash = %q, want %q", got, want)
	}
	if storedHash == rawKey {
		t.Fatal("stored key_hash matched plaintext key")
	}
}

func TestEnsureAPIKeyIsIdempotentAndRotatesHashInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "testimony.sqlite")
	store := openStore(t, ctx, path)
	defer store.Close()

	first, err := store.EnsureAPIKey(ctx, db.APIKeyBootstrapName, "bootstrap-key-test")
	if err != nil {
		t.Fatalf("EnsureAPIKey(first) error = %v", err)
	}
	duplicate, err := store.EnsureAPIKey(ctx, db.APIKeyBootstrapName, "bootstrap-key-test")
	if err != nil {
		t.Fatalf("EnsureAPIKey(duplicate) error = %v", err)
	}
	if duplicate.ID != first.ID {
		t.Fatalf("duplicate api key ID = %q, want %q", duplicate.ID, first.ID)
	}
	if !duplicate.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("duplicate UpdatedAt = %s, want %s", duplicate.UpdatedAt, first.UpdatedAt)
	}

	rotated, err := store.EnsureAPIKey(ctx, db.APIKeyBootstrapName, "rotated-bootstrap-key")
	if err != nil {
		t.Fatalf("EnsureAPIKey(rotated) error = %v", err)
	}
	if rotated.ID != first.ID {
		t.Fatalf("rotated api key ID = %q, want %q", rotated.ID, first.ID)
	}
	if rotated.KeyHash == first.KeyHash {
		t.Fatal("rotated KeyHash did not change")
	}
	if !rotated.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("rotated UpdatedAt = %s, want after %s", rotated.UpdatedAt, first.UpdatedAt)
	}
	if err := store.ValidateAPIKey(ctx, "rotated-bootstrap-key"); err != nil {
		t.Fatalf("ValidateAPIKey(rotated) error = %v", err)
	}
	if err := store.ValidateAPIKey(ctx, "bootstrap-key-test"); !errors.Is(err, auth.ErrInvalidAPIKey) {
		t.Fatalf("ValidateAPIKey(old key) error = %v, want ErrInvalidAPIKey", err)
	}

	sqliteDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer sqliteDB.Close()

	var rowCount int
	if err := sqliteDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE name = ?`, db.APIKeyBootstrapName).Scan(&rowCount); err != nil {
		t.Fatalf("count api_keys rows error = %v", err)
	}
	if got, want := rowCount, 1; got != want {
		t.Fatalf("api_keys row count = %d, want %d", got, want)
	}
}

func TestListProjectsAndIdempotentCreateProject(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "testimony.sqlite"))
	defer store.Close()

	first, err := store.CreateProject(ctx, "zeta")
	if err != nil {
		t.Fatalf("CreateProject(zeta) error = %v", err)
	}
	second, err := store.CreateProject(ctx, "alpha")
	if err != nil {
		t.Fatalf("CreateProject(alpha) error = %v", err)
	}
	dup, err := store.CreateProject(ctx, "zeta")
	if err != nil {
		t.Fatalf("CreateProject(zeta duplicate) error = %v", err)
	}
	if dup.ID != first.ID {
		t.Fatalf("duplicate project ID = %q, want %q", dup.ID, first.ID)
	}

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects() error = %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("len(ListProjects()) = %d, want 2", len(projects))
	}
	if got, want := projects[0].Slug, second.Slug; got != want {
		t.Fatalf("projects[0].Slug = %q, want %q", got, want)
	}
	if got, want := projects[1].Slug, first.Slug; got != want {
		t.Fatalf("projects[1].Slug = %q, want %q", got, want)
	}
}

func TestUpdateReportStatusTracksLifecycleFields(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "testimony.sqlite"))
	defer store.Close()

	report, err := store.CreateReport(ctx, db.CreateReportParams{
		ProjectSlug:      "lifecycle",
		ArchiveObjectKey: "projects/lifecycle/reports/r1/archive.zip",
		ArchiveFormat:    "zip",
		SourceFilename:   "results.zip",
	})
	if err != nil {
		t.Fatalf("CreateReport() error = %v", err)
	}
	if report.Status != db.ReportStatusPending {
		t.Fatalf("initial report.Status = %q, want %q", report.Status, db.ReportStatusPending)
	}

	processing, err := store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID: report.ID,
		Status:   db.ReportStatusProcessing,
	})
	if err != nil {
		t.Fatalf("UpdateReportStatus(processing) error = %v", err)
	}
	if processing.StartedAt == nil {
		t.Fatal("StartedAt = nil after processing, want non-nil")
	}
	if processing.CompletedAt != nil {
		t.Fatalf("CompletedAt after processing = %v, want nil", processing.CompletedAt)
	}

	completedAt := time.Date(2026, time.March, 24, 10, 0, 0, 0, time.UTC)
	failed, err := store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID:            report.ID,
		Status:              db.ReportStatusFailed,
		ErrorMessage:        "allure generate failed",
		CompletedAtOverride: &completedAt,
	})
	if err != nil {
		t.Fatalf("UpdateReportStatus(failed) error = %v", err)
	}
	if failed.CompletedAt == nil {
		t.Fatal("CompletedAt after failed = nil, want non-nil")
	}
	if got, want := failed.CompletedAt.UTC(), completedAt; !got.Equal(want) {
		t.Fatalf("CompletedAt after failed = %s, want %s", got, want)
	}
	if got, want := failed.ErrorMessage, "allure generate failed"; got != want {
		t.Fatalf("ErrorMessage after failed = %q, want %q", got, want)
	}

	ready, err := store.UpdateReportStatus(ctx, db.UpdateReportStatusParams{
		ReportID:           report.ID,
		Status:             db.ReportStatusReady,
		GeneratedObjectKey: "projects/lifecycle/reports/r1/html/index.html",
	})
	if err != nil {
		t.Fatalf("UpdateReportStatus(ready) error = %v", err)
	}
	if ready.CompletedAt == nil {
		t.Fatal("CompletedAt after ready = nil, want non-nil")
	}
	if ready.ErrorMessage != "" {
		t.Fatalf("ErrorMessage after ready = %q, want empty", ready.ErrorMessage)
	}
	if ready.GeneratedObjectKey == "" {
		t.Fatal("GeneratedObjectKey after ready = empty, want non-empty")
	}

	got, err := store.GetReport(ctx, report.ID)
	if err != nil {
		t.Fatalf("GetReport() error = %v", err)
	}
	if got.Status != db.ReportStatusReady {
		t.Fatalf("persisted report.Status = %q, want %q", got.Status, db.ReportStatusReady)
	}
	if got.GeneratedObjectKey != ready.GeneratedObjectKey {
		t.Fatalf("persisted GeneratedObjectKey = %q, want %q", got.GeneratedObjectKey, ready.GeneratedObjectKey)
	}
}

func TestProjectRetentionOverrideAndResolution(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "testimony.sqlite"))
	defer store.Close()

	project, err := store.CreateProject(ctx, "retained")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	if project.RetentionDays != nil {
		t.Fatalf("project.RetentionDays = %v, want nil", *project.RetentionDays)
	}

	resolved, err := store.ResolveProjectRetentionDays(ctx, project.Slug, 30)
	if err != nil {
		t.Fatalf("ResolveProjectRetentionDays(global) error = %v", err)
	}
	if got, want := resolved, 30; got != want {
		t.Fatalf("ResolveProjectRetentionDays(global) = %d, want %d", got, want)
	}

	override := 7
	project, err = store.SetProjectRetentionDays(ctx, project.Slug, &override)
	if err != nil {
		t.Fatalf("SetProjectRetentionDays(override) error = %v", err)
	}
	if project.RetentionDays == nil || *project.RetentionDays != override {
		t.Fatalf("project.RetentionDays after override = %v, want %d", project.RetentionDays, override)
	}

	resolved, err = store.ResolveProjectRetentionDays(ctx, project.Slug, 30)
	if err != nil {
		t.Fatalf("ResolveProjectRetentionDays(override) error = %v", err)
	}
	if got, want := resolved, override; got != want {
		t.Fatalf("ResolveProjectRetentionDays(override) = %d, want %d", got, want)
	}

	disabled := 0
	project, err = store.SetProjectRetentionDays(ctx, project.Slug, &disabled)
	if err != nil {
		t.Fatalf("SetProjectRetentionDays(disabled override) error = %v", err)
	}
	if project.RetentionDays == nil || *project.RetentionDays != disabled {
		t.Fatalf("project.RetentionDays after disabled override = %v, want %d", project.RetentionDays, disabled)
	}

	resolved, err = store.ResolveProjectRetentionDays(ctx, project.Slug, 30)
	if err != nil {
		t.Fatalf("ResolveProjectRetentionDays(disabled override) error = %v", err)
	}
	if got, want := resolved, 0; got != want {
		t.Fatalf("ResolveProjectRetentionDays(disabled override) = %d, want %d", got, want)
	}

	project, err = store.SetProjectRetentionDays(ctx, project.Slug, nil)
	if err != nil {
		t.Fatalf("SetProjectRetentionDays(clear) error = %v", err)
	}
	if project.RetentionDays != nil {
		t.Fatalf("project.RetentionDays after clear = %v, want nil", *project.RetentionDays)
	}

	resolved, err = store.ResolveProjectRetentionDays(ctx, project.Slug, 0)
	if err != nil {
		t.Fatalf("ResolveProjectRetentionDays(disabled global) error = %v", err)
	}
	if got, want := resolved, 0; got != want {
		t.Fatalf("ResolveProjectRetentionDays(disabled global) = %d, want %d", got, want)
	}
}

func TestListExpiredReportsRespectsGlobalAndProjectRetention(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "testimony.sqlite")
	store := openStore(t, ctx, path)
	defer store.Close()

	now := time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC)

	alphaOld := createCompletedReport(t, ctx, store, "alpha", "alpha-old", db.ReportStatusReady, now.Add(-40*24*time.Hour))
	_ = createCompletedReport(t, ctx, store, "alpha", "alpha-new", db.ReportStatusReady, now.Add(-10*24*time.Hour))
	betaOld := createCompletedReport(t, ctx, store, "beta", "beta-old", db.ReportStatusReady, now.Add(-8*24*time.Hour))
	gammaOld := createCompletedReport(t, ctx, store, "gamma", "gamma-old", db.ReportStatusReady, now.Add(-100*24*time.Hour))
	deltaFailed := createCompletedReport(t, ctx, store, "delta", "delta-failed", db.ReportStatusFailed, now.Add(-45*24*time.Hour))
	pending := createPendingReport(t, ctx, store, "epsilon", "epsilon-pending")

	betaRetention := 7
	if _, err := store.SetProjectRetentionDays(ctx, "beta", &betaRetention); err != nil {
		t.Fatalf("SetProjectRetentionDays(beta) error = %v", err)
	}
	gammaRetention := 0
	if _, err := store.SetProjectRetentionDays(ctx, "gamma", &gammaRetention); err != nil {
		t.Fatalf("SetProjectRetentionDays(gamma) error = %v", err)
	}
	backdateReportCreatedAt(t, path, pending.ID, now.Add(-365*24*time.Hour))

	reports, err := store.ListExpiredReports(ctx, 30, now)
	if err != nil {
		t.Fatalf("ListExpiredReports() error = %v", err)
	}

	gotByID := make(map[string]db.Report, len(reports))
	for _, report := range reports {
		gotByID[report.ID] = report
	}
	if got, want := len(gotByID), 3; got != want {
		t.Fatalf("len(ListExpiredReports()) = %d, want %d", got, want)
	}
	for _, report := range []db.Report{alphaOld, betaOld, deltaFailed} {
		if _, ok := gotByID[report.ID]; !ok {
			t.Fatalf("expected expired report %q to be returned; got IDs=%v", report.ID, mapKeys(gotByID))
		}
	}
	for _, report := range []db.Report{gammaOld, pending} {
		if _, ok := gotByID[report.ID]; ok {
			t.Fatalf("report %q unexpectedly returned as expired", report.ID)
		}
	}
}

func TestListExpiredReportsAllowsProjectOverrideWhenGlobalDisabled(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "testimony.sqlite"))
	defer store.Close()

	now := time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC)
	overridden := createCompletedReport(t, ctx, store, "override-on", "override-old", db.ReportStatusReady, now.Add(-10*24*time.Hour))
	_ = createCompletedReport(t, ctx, store, "global-off", "global-old", db.ReportStatusReady, now.Add(-10*24*time.Hour))

	override := 5
	if _, err := store.SetProjectRetentionDays(ctx, "override-on", &override); err != nil {
		t.Fatalf("SetProjectRetentionDays(override-on) error = %v", err)
	}

	reports, err := store.ListExpiredReports(ctx, 0, now)
	if err != nil {
		t.Fatalf("ListExpiredReports(global disabled) error = %v", err)
	}
	if got, want := len(reports), 1; got != want {
		t.Fatalf("len(ListExpiredReports(global disabled)) = %d, want %d", got, want)
	}
	if got, want := reports[0].ID, overridden.ID; got != want {
		t.Fatalf("expired report ID = %q, want %q", got, want)
	}
}

func TestDeleteReportRemovesRow(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "testimony.sqlite"))
	defer store.Close()

	report := createCompletedReport(t, ctx, store, "delete-me", "delete-ready", db.ReportStatusReady, time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC))
	if err := store.DeleteReport(ctx, report.ID); err != nil {
		t.Fatalf("DeleteReport() error = %v", err)
	}
	if _, err := store.GetReport(ctx, report.ID); !errors.Is(err, db.ErrReportNotFound) {
		t.Fatalf("GetReport(after delete) error = %v, want ErrReportNotFound", err)
	}
}

func TestRunMigrationsSetsSchemaAndPragmas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "testimony.sqlite")

	sqliteDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer sqliteDB.Close()
	sqliteDB.SetMaxOpenConns(1)
	sqliteDB.SetMaxIdleConns(1)

	if err := db.RunMigrations(ctx, sqliteDB, 5*time.Second); err != nil {
		t.Fatalf("RunMigrations() error = %v", err)
	}
	if err := db.RunMigrations(ctx, sqliteDB, 5*time.Second); err != nil {
		t.Fatalf("RunMigrations() second pass error = %v", err)
	}

	for _, table := range []string{"schema_migrations", "projects", "reports", "api_keys"} {
		var name string
		if err := sqliteDB.QueryRowContext(ctx, `
SELECT name
FROM sqlite_master
WHERE type = 'table' AND name = ?
`, table).Scan(&name); err != nil {
			t.Fatalf("lookup table %q error = %v", table, err)
		}
		if name != table {
			t.Fatalf("table name = %q, want %q", name, table)
		}
	}

	var retentionColumn string
	if err := sqliteDB.QueryRowContext(ctx, `
SELECT name
FROM pragma_table_info('projects')
WHERE name = 'retention_days'
`).Scan(&retentionColumn); err != nil {
		t.Fatalf("lookup projects.retention_days column error = %v", err)
	}
	if got, want := retentionColumn, "retention_days"; got != want {
		t.Fatalf("projects retention column = %q, want %q", got, want)
	}

	var mode string
	if err := sqliteDB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode error = %v", err)
	}
	if got, want := mode, "wal"; got != want {
		t.Fatalf("journal_mode = %q, want %q", got, want)
	}

	var foreignKeys int
	if err := sqliteDB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys error = %v", err)
	}
	if got, want := foreignKeys, 1; got != want {
		t.Fatalf("foreign_keys = %d, want %d", got, want)
	}

	var busyTimeout int
	if err := sqliteDB.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout error = %v", err)
	}
	if got, want := busyTimeout, 5000; got != want {
		t.Fatalf("busy_timeout = %d, want %d", got, want)
	}

	var synchronous int
	if err := sqliteDB.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("PRAGMA synchronous error = %v", err)
	}
	if got, want := synchronous, 1; got != want {
		t.Fatalf("synchronous = %d, want %d", got, want)
	}
}

func TestOpenSQLiteStoreRejectsInvalidConfig(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))

	if _, err := db.OpenSQLiteStore(ctx, "", 5*time.Second, logger); err == nil || !strings.Contains(err.Error(), "empty path") {
		t.Fatalf("OpenSQLiteStore(empty path) error = %v, want empty path validation", err)
	}

	path := filepath.Join(t.TempDir(), "testimony.sqlite")
	if _, err := db.OpenSQLiteStore(ctx, path, 0, logger); err == nil || !strings.Contains(err.Error(), "busy timeout") {
		t.Fatalf("OpenSQLiteStore(zero timeout) error = %v, want busy timeout validation", err)
	}
}

func TestGettersReturnNotFoundSentinels(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, ctx, filepath.Join(t.TempDir(), "testimony.sqlite"))
	defer store.Close()

	if _, err := store.GetProject(ctx, "missing"); !errors.Is(err, db.ErrProjectNotFound) {
		t.Fatalf("GetProject(missing) error = %v, want ErrProjectNotFound", err)
	}
	if _, err := store.GetReport(ctx, "missing"); !errors.Is(err, db.ErrReportNotFound) {
		t.Fatalf("GetReport(missing) error = %v, want ErrReportNotFound", err)
	}
}

func createCompletedReport(t *testing.T, ctx context.Context, store *db.SQLiteStore, projectSlug, reportID string, status db.ReportStatus, completedAt time.Time) db.Report {
	t.Helper()

	report, err := store.CreateReport(ctx, db.CreateReportParams{
		ReportID:         reportID,
		ProjectSlug:      projectSlug,
		ArchiveObjectKey: fmt.Sprintf("projects/%s/reports/%s/archive.zip", projectSlug, reportID),
		ArchiveFormat:    "zip",
		SourceFilename:   reportID + ".zip",
	})
	if err != nil {
		t.Fatalf("CreateReport(%q) error = %v", reportID, err)
	}

	params := db.UpdateReportStatusParams{
		ReportID:            report.ID,
		Status:              status,
		CompletedAtOverride: &completedAt,
	}
	if status == db.ReportStatusReady {
		params.GeneratedObjectKey = fmt.Sprintf("projects/%s/reports/%s/html/index.html", projectSlug, reportID)
	} else if status == db.ReportStatusFailed {
		params.ErrorMessage = "generation failed"
	}

	updated, err := store.UpdateReportStatus(ctx, params)
	if err != nil {
		t.Fatalf("UpdateReportStatus(%q) error = %v", reportID, err)
	}
	return updated
}

func createPendingReport(t *testing.T, ctx context.Context, store *db.SQLiteStore, projectSlug, reportID string) db.Report {
	t.Helper()

	report, err := store.CreateReport(ctx, db.CreateReportParams{
		ReportID:         reportID,
		ProjectSlug:      projectSlug,
		ArchiveObjectKey: fmt.Sprintf("projects/%s/reports/%s/archive.zip", projectSlug, reportID),
		ArchiveFormat:    "zip",
		SourceFilename:   reportID + ".zip",
	})
	if err != nil {
		t.Fatalf("CreateReport(%q) error = %v", reportID, err)
	}
	return report
}

func backdateReportCreatedAt(t *testing.T, sqlitePath, reportID string, createdAt time.Time) {
	t.Helper()

	sqliteDB, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatalf("sql.Open(%q) error = %v", sqlitePath, err)
	}
	defer sqliteDB.Close()

	formatted := createdAt.UTC().Format(time.RFC3339Nano)
	if _, err := sqliteDB.Exec(`
UPDATE reports
SET created_at = ?, updated_at = ?
WHERE id = ?
`, formatted, formatted, reportID); err != nil {
		t.Fatalf("backdate report %q error = %v", reportID, err)
	}
}

func mapKeys(values map[string]db.Report) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func openStore(t *testing.T, ctx context.Context, path string) *db.SQLiteStore {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))
	store, err := db.OpenSQLiteStore(ctx, path, 5*time.Second, logger)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(%q) error = %v", path, err)
	}
	return store
}

type testWriter struct {
	t *testing.T
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
