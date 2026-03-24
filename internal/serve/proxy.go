package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/storage"
)

type ReportLookup interface {
	GetReport(ctx context.Context, reportID string) (db.Report, error)
}

type ObjectDownloader interface {
	Download(ctx context.Context, key string) (storage.DownloadResult, error)
}

type ReportProxy struct {
	logger  *slog.Logger
	reports ReportLookup
	objects ObjectDownloader
}

func NewReportProxy(logger *slog.Logger, reports ReportLookup, objects ObjectDownloader) (*ReportProxy, error) {
	if reports == nil {
		return nil, fmt.Errorf("new report proxy: nil report store")
	}
	if objects == nil {
		return nil, fmt.Errorf("new report proxy: nil object store")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ReportProxy{
		logger:  logger,
		reports: reports,
		objects: objects,
	}, nil
}

func (p *ReportProxy) RegisterRoutes(r chi.Router) {
	if p == nil || r == nil {
		return
	}

	r.Get("/reports/{slug}/{reportID}", p.redirectToRoot)
	r.Get("/reports/{slug}/{reportID}/", p.serveReport)
	r.Get("/reports/{slug}/{reportID}/*", p.serveReport)
}

func (p *ReportProxy) redirectToRoot(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(chi.URLParam(r, "slug"))
	reportID := strings.TrimSpace(chi.URLParam(r, "reportID"))
	if slug == "" || reportID == "" {
		http.NotFound(w, r)
		return
	}

	location := path.Join("/reports", slug, reportID) + "/"
	if rawQuery := strings.TrimSpace(r.URL.RawQuery); rawQuery != "" {
		location += "?" + rawQuery
	}
	http.Redirect(w, r, location, http.StatusFound)
}

func (p *ReportProxy) serveReport(w http.ResponseWriter, r *http.Request) {
	routeSlug := strings.TrimSpace(chi.URLParam(r, "slug"))
	reportID := strings.TrimSpace(chi.URLParam(r, "reportID"))
	if routeSlug == "" || reportID == "" {
		http.NotFound(w, r)
		return
	}

	report, err := p.reports.GetReport(r.Context(), reportID)
	if err != nil {
		if errors.Is(err, db.ErrReportNotFound) {
			http.NotFound(w, r)
			return
		}
		p.logger.Error("report proxy lookup failed",
			"project_slug", routeSlug,
			"report_id", reportID,
			"error", err,
		)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	if report.Status != db.ReportStatusReady || strings.TrimSpace(report.ProjectSlug) != routeSlug {
		http.NotFound(w, r)
		return
	}

	prefix, err := reportHTMLPrefix(report)
	if err != nil {
		p.logger.Warn("report proxy rejected invalid generated object key",
			"project_slug", routeSlug,
			"report_id", reportID,
			"generated_object_key", report.GeneratedObjectKey,
			"error", err,
		)
		http.NotFound(w, r)
		return
	}

	assetPath, ok := normalizeReportAssetPath(chi.URLParam(r, "*"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	objectKey := prefix + assetPath
	result, err := p.objects.Download(r.Context(), objectKey)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			http.NotFound(w, r)
			return
		}
		p.logger.Error("report proxy download failed",
			"project_slug", routeSlug,
			"report_id", reportID,
			"object_key", objectKey,
			"error", err,
		)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	defer result.Body.Close()

	if contentType := strings.TrimSpace(result.ContentType); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if result.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.ContentLength, 10))
	}

	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, result.Body); err != nil {
		p.logger.Warn("report proxy stream interrupted",
			"project_slug", routeSlug,
			"report_id", reportID,
			"object_key", objectKey,
			"error", err,
		)
	}
}

func reportHTMLPrefix(report db.Report) (string, error) {
	trimmed := strings.TrimSpace(report.GeneratedObjectKey)
	if trimmed == "" {
		return "", fmt.Errorf("empty generated object key")
	}

	dir := path.Dir(trimmed)
	if dir == "." || dir == "/" {
		return "", fmt.Errorf("invalid generated object key %q", report.GeneratedObjectKey)
	}

	return strings.TrimSuffix(dir, "/") + "/", nil
}

func normalizeReportAssetPath(raw string) (string, bool) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(raw, "/"))
	if trimmed == "" {
		return "index.html", true
	}

	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == "/" {
		return "index.html", true
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}

	return cleaned, true
}
