package serve

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/testimony-dev/testimony/internal/db"
)

func TestUI(t *testing.T) {
	now := time.Date(2026, time.March, 24, 16, 30, 0, 0, time.UTC)
	readyCompleted := now.Add(15 * time.Minute)

	store := stubBrowseStore{
		projects: []db.Project{
			{Slug: "alpha"},
			{Slug: "widgets"},
		},
		reports: map[string][]db.Report{
			"alpha": nil,
			"widgets": {
				{
					ID:             "report-processing",
					ProjectSlug:    "widgets",
					Status:         db.ReportStatusProcessing,
					CreatedAt:      now,
					SourceFilename: "processing.zip",
				},
				{
					ID:                 "report-ready",
					ProjectSlug:        "widgets",
					Status:             db.ReportStatusReady,
					CreatedAt:          now.Add(-time.Hour),
					CompletedAt:        &readyCompleted,
					GeneratedObjectKey: "projects/widgets/reports/report-ready/html/index.html",
					SourceFilename:     "ready.zip",
				},
			},
		},
	}

	router := newUIRouter(t, store)

	t.Run("root page lists projects with counts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}

		body := rec.Body.String()
		for _, fragment := range []string{
			"Browse generated reports",
			">2<",
			">1<",
			"/projects/widgets",
			"/projects/alpha",
			"No reports yet for this project.",
			"status-badge status-badge--processing",
		} {
			if !strings.Contains(body, fragment) {
				t.Fatalf("root page body missing %q in %q", fragment, body)
			}
		}
	})

	t.Run("root page renders empty state", func(t *testing.T) {
		emptyRouter := newUIRouter(t, stubBrowseStore{})
		rec := httptest.NewRecorder()
		emptyRouter.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if body := rec.Body.String(); !strings.Contains(body, "No projects yet") {
			t.Fatalf("empty state body = %q, want no projects message", body)
		}
	})

	t.Run("project page lists reports newest first and links ready report", func(t *testing.T) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/projects/widgets", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}

		body := rec.Body.String()
		if !strings.Contains(body, "Project history") {
			t.Fatalf("project page body = %q, want heading", body)
		}
		if !strings.Contains(body, "/reports/widgets/report-ready/") {
			t.Fatalf("project page body = %q, want ready report link", body)
		}
		if strings.Contains(body, "/reports/widgets/report-processing/") {
			t.Fatalf("project page body = %q, did not expect browse link for processing report", body)
		}
		processingIndex := strings.Index(body, "report-processing")
		readyIndex := strings.Index(body, "report-ready")
		if processingIndex == -1 || readyIndex == -1 {
			t.Fatalf("project page body = %q, want both report ids", body)
		}
		if processingIndex > readyIndex {
			t.Fatalf("project page body order = %q, want newest report first", body)
		}
	})

	t.Run("project page renders empty state for existing project without reports", func(t *testing.T) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/projects/alpha", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if body := rec.Body.String(); !strings.Contains(body, "No reports yet") {
			t.Fatalf("empty project body = %q, want no reports message", body)
		}
	})

	t.Run("missing project returns deterministic html 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/projects/missing", nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}

		body := rec.Body.String()
		for _, fragment := range []string{"Project not found", "missing"} {
			if !strings.Contains(body, fragment) {
				t.Fatalf("missing project body missing %q in %q", fragment, body)
			}
		}
	})
}

func newUIRouter(t *testing.T, store stubBrowseStore) http.Handler {
	t.Helper()

	ui, err := NewUI(slog.New(slog.NewTextHandler(io.Discard, nil)), store)
	if err != nil {
		t.Fatalf("NewUI() error = %v", err)
	}

	router := chi.NewRouter()
	ui.RegisterRoutes(router)
	return router
}

type stubBrowseStore struct {
	projects []db.Project
	reports  map[string][]db.Report
	err      error
}

func (s stubBrowseStore) ListProjects(_ context.Context) ([]db.Project, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]db.Project(nil), s.projects...), nil
}

func (s stubBrowseStore) GetProject(_ context.Context, slug string) (db.Project, error) {
	if s.err != nil {
		return db.Project{}, s.err
	}
	for _, project := range s.projects {
		if project.Slug == slug {
			return project, nil
		}
	}
	return db.Project{}, db.ErrProjectNotFound
}

func (s stubBrowseStore) ListReports(_ context.Context, projectSlug string) ([]db.Report, error) {
	if s.err != nil {
		return nil, s.err
	}
	reports := s.reports[projectSlug]
	return append([]db.Report(nil), reports...), nil
}
