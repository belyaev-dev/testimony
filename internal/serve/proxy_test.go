package serve

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/storage"
)

func TestReportProxy(t *testing.T) {
	readyReport := db.Report{
		ID:                 "report-123",
		ProjectSlug:        "widgets",
		Status:             db.ReportStatusReady,
		GeneratedObjectKey: "projects/widgets/reports/report-123/html/index.html",
	}

	t.Run("serves report root with metadata passthrough", func(t *testing.T) {
		router := newReportProxyRouter(t,
			stubReportStore{reports: map[string]db.Report{readyReport.ID: readyReport}},
			stubObjectStore{objects: map[string]downloadFixture{
				"projects/widgets/reports/report-123/html/index.html": {
					contentType: "text/html; charset=utf-8",
					payload:     []byte("<html>generated</html>"),
				},
			}},
		)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/widgets/report-123/", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if got, want := rec.Body.String(), "<html>generated</html>"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
		if got, want := rec.Header().Get("Content-Type"), "text/html; charset=utf-8"; got != want {
			t.Fatalf("content-type = %q, want %q", got, want)
		}
		if got, want := rec.Header().Get("Content-Length"), strconv.Itoa(len("<html>generated</html>")); got != want {
			t.Fatalf("content-length = %q, want %q", got, want)
		}
	})

	t.Run("serves nested assets", func(t *testing.T) {
		router := newReportProxyRouter(t,
			stubReportStore{reports: map[string]db.Report{readyReport.ID: readyReport}},
			stubObjectStore{objects: map[string]downloadFixture{
				"projects/widgets/reports/report-123/html/assets/app.js": {
					contentType: "text/javascript; charset=utf-8",
					payload:     []byte("console.log('ok')"),
				},
			}},
		)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/widgets/report-123/assets/app.js", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if got, want := rec.Body.String(), "console.log('ok')"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
		if got, want := rec.Header().Get("Content-Type"), "text/javascript; charset=utf-8"; got != want {
			t.Fatalf("content-type = %q, want %q", got, want)
		}
	})

	t.Run("redirects bare report url to canonical root", func(t *testing.T) {
		router := newReportProxyRouter(t, stubReportStore{}, stubObjectStore{})

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/widgets/report-123", nil))

		if got, want := rec.Code, http.StatusFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if got, want := rec.Header().Get("Location"), "/reports/widgets/report-123/"; got != want {
			t.Fatalf("location = %q, want %q", got, want)
		}
	})

	t.Run("rejects slug mismatch", func(t *testing.T) {
		router := newReportProxyRouter(t,
			stubReportStore{reports: map[string]db.Report{readyReport.ID: readyReport}},
			stubObjectStore{},
		)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/other/report-123/", nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
	})

	t.Run("rejects non-ready report", func(t *testing.T) {
		notReady := readyReport
		notReady.Status = db.ReportStatusProcessing
		router := newReportProxyRouter(t,
			stubReportStore{reports: map[string]db.Report{notReady.ID: notReady}},
			stubObjectStore{},
		)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/widgets/report-123/", nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
	})

	t.Run("rejects missing report", func(t *testing.T) {
		router := newReportProxyRouter(t, stubReportStore{}, stubObjectStore{})

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/widgets/report-404/", nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
	})

	t.Run("rejects missing object", func(t *testing.T) {
		router := newReportProxyRouter(t,
			stubReportStore{reports: map[string]db.Report{readyReport.ID: readyReport}},
			stubObjectStore{},
		)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reports/widgets/report-123/", nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
	})
}

func newReportProxyRouter(t *testing.T, reports stubReportStore, objects stubObjectStore) http.Handler {
	t.Helper()

	proxy, err := NewReportProxy(slog.New(slog.NewTextHandler(io.Discard, nil)), reports, objects)
	if err != nil {
		t.Fatalf("NewReportProxy() error = %v", err)
	}

	router := chi.NewRouter()
	proxy.RegisterRoutes(router)
	return router
}

type stubReportStore struct {
	reports map[string]db.Report
	err     error
}

func (s stubReportStore) GetReport(_ context.Context, reportID string) (db.Report, error) {
	if s.err != nil {
		return db.Report{}, s.err
	}
	report, ok := s.reports[reportID]
	if !ok {
		return db.Report{}, db.ErrReportNotFound
	}
	return report, nil
}

type stubObjectStore struct {
	objects map[string]downloadFixture
	err     error
}

type downloadFixture struct {
	contentType string
	payload     []byte
}

func (s stubObjectStore) Download(_ context.Context, key string) (storage.DownloadResult, error) {
	if s.err != nil {
		return storage.DownloadResult{}, s.err
	}
	fixture, ok := s.objects[key]
	if !ok {
		return storage.DownloadResult{}, storage.ErrObjectNotFound
	}
	payload := append([]byte(nil), fixture.payload...)
	return storage.DownloadResult{
		Body:          io.NopCloser(bytes.NewReader(payload)),
		ContentType:   fixture.contentType,
		ContentLength: int64(len(payload)),
	}, nil
}
