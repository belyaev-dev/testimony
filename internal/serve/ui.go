package serve

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/testimony-dev/testimony/internal/db"
)

//go:embed templates/*.html
var uiTemplateFS embed.FS

var (
	uiTemplatesOnce sync.Once
	uiTemplates     *template.Template
	uiTemplatesErr  error
)

type BrowseStore interface {
	ListProjects(ctx context.Context) ([]db.Project, error)
	GetProject(ctx context.Context, slug string) (db.Project, error)
	ListReports(ctx context.Context, projectSlug string) ([]db.Report, error)
}

type UI struct {
	logger    *slog.Logger
	store     BrowseStore
	templates *template.Template
}

type projectsPageViewModel struct {
	Title            string
	ProjectCount     int
	ReportCount      int
	ReadyReportCount int
	Projects         []projectCardViewModel
}

type projectCardViewModel struct {
	Slug              string
	ProjectURL        string
	ReportCount       int
	ReadyReportCount  int
	LatestStatusLabel string
	LatestStatusTone  string
	LatestUpdatedISO  string
	LatestUpdatedText string
	HasReports        bool
}

type reportsPageViewModel struct {
	Title       string
	ProjectSlug string
	ProjectURL  string
	Missing     bool
	ReportCount int
	Reports     []reportRowViewModel
}

type reportRowViewModel struct {
	ID              string
	StatusLabel     string
	StatusTone      string
	CreatedAtISO    string
	CreatedAtText   string
	CompletedAtISO  string
	CompletedAtText string
	SourceFilename  string
	BrowseURL       string
	CanBrowse       bool
}

func NewUI(logger *slog.Logger, store BrowseStore) (*UI, error) {
	if store == nil {
		return nil, fmt.Errorf("new ui: nil browse store")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	templates, err := loadUITemplates()
	if err != nil {
		return nil, fmt.Errorf("new ui: %w", err)
	}

	return &UI{
		logger:    logger,
		store:     store,
		templates: templates,
	}, nil
}

func loadUITemplates() (*template.Template, error) {
	uiTemplatesOnce.Do(func() {
		uiTemplates, uiTemplatesErr = template.ParseFS(uiTemplateFS, "templates/*.html")
	})
	return uiTemplates, uiTemplatesErr
}

func (u *UI) RegisterRoutes(r chi.Router) {
	if u == nil || r == nil {
		return
	}

	r.Get("/", u.listProjects)
	r.Get("/projects/{slug}", u.showProject)
}

func (u *UI) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := u.store.ListProjects(r.Context())
	if err != nil {
		u.logger.Error("browse ui list projects failed", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	viewModel := projectsPageViewModel{
		Title:        "Projects · Testimony",
		ProjectCount: len(projects),
		Projects:     make([]projectCardViewModel, 0, len(projects)),
	}

	for _, project := range projects {
		reports, err := u.store.ListReports(r.Context(), project.Slug)
		if err != nil {
			u.logger.Error("browse ui list project reports failed",
				"project_slug", project.Slug,
				"error", err,
			)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		card := projectCardViewModel{
			Slug:       project.Slug,
			ProjectURL: "/projects/" + project.Slug,
			HasReports: len(reports) > 0,
		}
		card.ReportCount = len(reports)
		viewModel.ReportCount += len(reports)

		for _, report := range reports {
			if report.Status == db.ReportStatusReady {
				card.ReadyReportCount++
				viewModel.ReadyReportCount++
			}
		}

		if len(reports) > 0 {
			card.LatestStatusLabel = string(reports[0].Status)
			card.LatestStatusTone = statusTone(reports[0].Status)
			card.LatestUpdatedISO = reports[0].CreatedAt.UTC().Format(time.RFC3339)
			card.LatestUpdatedText = formatTimestampText(reports[0].CreatedAt)
		}

		viewModel.Projects = append(viewModel.Projects, card)
	}

	u.renderHTML(w, http.StatusOK, "projects", viewModel)
}

func (u *UI) showProject(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(chi.URLParam(r, "slug"))
	if slug == "" {
		http.NotFound(w, r)
		return
	}

	project, err := u.store.GetProject(r.Context(), slug)
	if err != nil {
		if errors.Is(err, db.ErrProjectNotFound) {
			u.renderHTML(w, http.StatusNotFound, "reports", reportsPageViewModel{
				Title:       "Project not found · Testimony",
				ProjectSlug: slug,
				ProjectURL:  "/projects/" + slug,
				Missing:     true,
			})
			return
		}

		u.logger.Error("browse ui get project failed",
			"project_slug", slug,
			"error", err,
		)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	reports, err := u.store.ListReports(r.Context(), project.Slug)
	if err != nil {
		u.logger.Error("browse ui list reports failed",
			"project_slug", project.Slug,
			"error", err,
		)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	viewModel := reportsPageViewModel{
		Title:       project.Slug + " reports · Testimony",
		ProjectSlug: project.Slug,
		ProjectURL:  "/projects/" + project.Slug,
		ReportCount: len(reports),
		Reports:     make([]reportRowViewModel, 0, len(reports)),
	}

	for _, report := range reports {
		row := reportRowViewModel{
			ID:             report.ID,
			StatusLabel:    string(report.Status),
			StatusTone:     statusTone(report.Status),
			CreatedAtISO:   report.CreatedAt.UTC().Format(time.RFC3339),
			CreatedAtText:  formatTimestampText(report.CreatedAt),
			SourceFilename: strings.TrimSpace(report.SourceFilename),
		}
		if report.CompletedAt != nil {
			row.CompletedAtISO = report.CompletedAt.UTC().Format(time.RFC3339)
			row.CompletedAtText = formatTimestampText(*report.CompletedAt)
		}
		if report.Status == db.ReportStatusReady {
			row.CanBrowse = true
			row.BrowseURL = "/reports/" + project.Slug + "/" + report.ID + "/"
		}
		viewModel.Reports = append(viewModel.Reports, row)
	}

	u.renderHTML(w, http.StatusOK, "reports", viewModel)
}

func (u *UI) renderHTML(w http.ResponseWriter, status int, templateName string, data any) {
	var buf bytes.Buffer
	if err := u.templates.ExecuteTemplate(&buf, templateName, data); err != nil {
		u.logger.Error("browse ui template render failed",
			"template", templateName,
			"error", err,
		)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func statusTone(status db.ReportStatus) string {
	switch status {
	case db.ReportStatusReady:
		return "ready"
	case db.ReportStatusProcessing:
		return "processing"
	case db.ReportStatusFailed:
		return "failed"
	default:
		return "pending"
	}
}

func formatTimestampText(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.UTC().Format("2006-01-02 15:04 UTC")
}
