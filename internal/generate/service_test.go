package generate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/storage"
)

func TestGenerationService(t *testing.T) {
	t.Run("marks report ready uploads html and cleans workdir", func(t *testing.T) {
		ctx := context.Background()
		store := openGenerationSQLiteStore(t, ctx)
		defer store.Close()

		reportID := "report-ready"
		projectSlug := "widgets"
		if _, err := store.CreateReport(ctx, db.CreateReportParams{
			ReportID:         reportID,
			ProjectSlug:      projectSlug,
			ArchiveObjectKey: "projects/widgets/reports/report-ready/archive.zip",
			ArchiveFormat:    "zip",
			SourceFilename:   "results.zip",
		}); err != nil {
			t.Fatalf("CreateReport() error = %v", err)
		}

		tempDir := t.TempDir()
		resultsDir := createGenerationResultsDir(t, filepath.Join(tempDir, "report-generation", reportID, "results"))
		storageStore := newGenerationMemoryStore()
		generator := &stubGenerator{generateFn: func(_ context.Context, req Request) (Result, error) {
			if got, want := req.ProjectSlug, projectSlug; got != want {
				t.Fatalf("req.ProjectSlug = %q, want %q", got, want)
			}
			if got, want := req.ReportID, reportID; got != want {
				t.Fatalf("req.ReportID = %q, want %q", got, want)
			}
			if got, want := req.ResultsDir, resultsDir; got != want {
				t.Fatalf("req.ResultsDir = %q, want %q", got, want)
			}

			outputDir := filepath.Join(req.WorkDir, "html")
			if err := os.MkdirAll(filepath.Join(outputDir, "assets"), 0o755); err != nil {
				t.Fatalf("MkdirAll(outputDir) error = %v", err)
			}
			indexPath := filepath.Join(outputDir, "index.html")
			if err := os.WriteFile(indexPath, []byte("<html>ok</html>"), 0o644); err != nil {
				t.Fatalf("WriteFile(index) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(outputDir, "assets", "app.js"), []byte("console.log('ok')"), 0o644); err != nil {
				t.Fatalf("WriteFile(asset) error = %v", err)
			}
			return Result{
				Variant:        VariantAllure2,
				ProjectSlug:    req.ProjectSlug,
				ReportID:       req.ReportID,
				OutputDir:      outputDir,
				IndexPath:      indexPath,
				HistorySources: []string{"projects/widgets/reports/older/html/history/history-trend.json"},
			}, nil
		}}

		service := newGenerationService(t, ServiceOptions{
			Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			Store:          store,
			Storage:        storageStore,
			Generator:      generator,
			TempDir:        tempDir,
			MaxConcurrency: 2,
		})

		if err := service.Enqueue(Job{ProjectSlug: projectSlug, ReportID: reportID, ResultsDir: resultsDir}); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}

		report := waitForReportStatus(t, ctx, store, reportID, db.ReportStatusReady)
		if got, want := report.GeneratedObjectKey, "projects/widgets/reports/report-ready/html/index.html"; got != want {
			t.Fatalf("report.GeneratedObjectKey = %q, want %q", got, want)
		}
		if report.StartedAt == nil {
			t.Fatal("report.StartedAt = nil, want value")
		}
		if report.CompletedAt == nil {
			t.Fatal("report.CompletedAt = nil, want value")
		}
		if got := report.ErrorMessage; got != "" {
			t.Fatalf("report.ErrorMessage = %q, want empty", got)
		}

		if got := string(storageStore.object("projects/widgets/reports/report-ready/html/index.html")); got != "<html>ok</html>" {
			t.Fatalf("uploaded index = %q, want generated html", got)
		}
		if got := string(storageStore.object("projects/widgets/reports/report-ready/html/assets/app.js")); got != "console.log('ok')" {
			t.Fatalf("uploaded asset = %q, want generated asset", got)
		}

		waitForPathMissing(t, filepath.Join(tempDir, "report-generation", reportID))
	})

	t.Run("marks report failed truncates error and cleans workdir", func(t *testing.T) {
		ctx := context.Background()
		store := openGenerationSQLiteStore(t, ctx)
		defer store.Close()

		reportID := "report-failed"
		projectSlug := "widgets"
		if _, err := store.CreateReport(ctx, db.CreateReportParams{
			ReportID:         reportID,
			ProjectSlug:      projectSlug,
			ArchiveObjectKey: "projects/widgets/reports/report-failed/archive.zip",
			ArchiveFormat:    "zip",
			SourceFilename:   "results.zip",
		}); err != nil {
			t.Fatalf("CreateReport() error = %v", err)
		}

		tempDir := t.TempDir()
		resultsDir := createGenerationResultsDir(t, filepath.Join(tempDir, "report-generation", reportID, "results"))
		generator := &stubGenerator{generateFn: func(_ context.Context, req Request) (Result, error) {
			return Result{}, fmt.Errorf("generator exploded: %s", stringsRepeat("stderr", 900))
		}}

		service := newGenerationService(t, ServiceOptions{
			Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			Store:          store,
			Storage:        newGenerationMemoryStore(),
			Generator:      generator,
			TempDir:        tempDir,
			MaxConcurrency: 1,
		})

		if err := service.Enqueue(Job{ProjectSlug: projectSlug, ReportID: reportID, ResultsDir: resultsDir}); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}

		report := waitForReportStatus(t, ctx, store, reportID, db.ReportStatusFailed)
		if got := report.GeneratedObjectKey; got != "" {
			t.Fatalf("report.GeneratedObjectKey = %q, want empty", got)
		}
		if report.StartedAt == nil {
			t.Fatal("report.StartedAt = nil, want value")
		}
		if report.CompletedAt == nil {
			t.Fatal("report.CompletedAt = nil, want value")
		}
		if got := report.ErrorMessage; got == "" {
			t.Fatal("report.ErrorMessage = empty, want failure text")
		} else if len(got) > defaultFailureMessageLimit {
			t.Fatalf("len(report.ErrorMessage) = %d, want <= %d", len(got), defaultFailureMessageLimit)
		}

		waitForPathMissing(t, filepath.Join(tempDir, "report-generation", reportID))
	})

	t.Run("bounds concurrent generation", func(t *testing.T) {
		ctx := context.Background()
		store := openGenerationSQLiteStore(t, ctx)
		defer store.Close()

		tempDir := t.TempDir()
		for _, reportID := range []string{"report-1", "report-2"} {
			if _, err := store.CreateReport(ctx, db.CreateReportParams{
				ReportID:         reportID,
				ProjectSlug:      "widgets",
				ArchiveObjectKey: fmt.Sprintf("projects/widgets/reports/%s/archive.zip", reportID),
				ArchiveFormat:    "zip",
				SourceFilename:   "results.zip",
			}); err != nil {
				t.Fatalf("CreateReport(%s) error = %v", reportID, err)
			}
			createGenerationResultsDir(t, filepath.Join(tempDir, "report-generation", reportID, "results"))
		}

		started := make(chan string, 2)
		release := make(chan struct{})
		generator := &blockingGenerator{started: started, release: release}
		service := newGenerationService(t, ServiceOptions{
			Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			Store:          store,
			Storage:        newGenerationMemoryStore(),
			Generator:      generator,
			TempDir:        tempDir,
			MaxConcurrency: 1,
		})

		if err := service.Enqueue(Job{ProjectSlug: "widgets", ReportID: "report-1", ResultsDir: filepath.Join(tempDir, "report-generation", "report-1", "results")}); err != nil {
			t.Fatalf("Enqueue(report-1) error = %v", err)
		}
		if err := service.Enqueue(Job{ProjectSlug: "widgets", ReportID: "report-2", ResultsDir: filepath.Join(tempDir, "report-generation", "report-2", "results")}); err != nil {
			t.Fatalf("Enqueue(report-2) error = %v", err)
		}

		first := waitForStartedJob(t, started)
		select {
		case second := <-started:
			t.Fatalf("second job %q started before first was released", second)
		case <-time.After(200 * time.Millisecond):
		}

		close(release)
		second := waitForStartedJob(t, started)
		if first == second {
			t.Fatalf("started jobs = %q then %q, want two different reports", first, second)
		}

		waitForReportStatus(t, ctx, store, "report-1", db.ReportStatusReady)
		waitForReportStatus(t, ctx, store, "report-2", db.ReportStatusReady)
		if got, want := generator.maxActive(), 1; got != want {
			t.Fatalf("max concurrent Generate() calls = %d, want %d", got, want)
		}
	})
}

func newGenerationService(t *testing.T, opts ServiceOptions) *Service {
	t.Helper()
	service, err := NewService(opts)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func openGenerationSQLiteStore(t *testing.T, ctx context.Context) *db.SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "testimony.sqlite")
	store, err := db.OpenSQLiteStore(ctx, path, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("OpenSQLiteStore(%q) error = %v", path, err)
	}
	return store
}

func createGenerationResultsDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "widgets-result.json"), []byte(`{"name":"widgets"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(results) error = %v", err)
	}
	return dir
}

func waitForReportStatus(t *testing.T, ctx context.Context, store *db.SQLiteStore, reportID string, want db.ReportStatus) db.Report {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		report, err := store.GetReport(ctx, reportID)
		if err == nil && report.Status == want {
			return report
		}
		time.Sleep(25 * time.Millisecond)
	}

	report, err := store.GetReport(ctx, reportID)
	if err != nil {
		t.Fatalf("GetReport(%q) error = %v", reportID, err)
	}
	t.Fatalf("report %q status = %q, want %q", reportID, report.Status, want)
	panic("unreachable")
}

func waitForPathMissing(t *testing.T, target string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(target)
		if os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("path %q still exists after timeout", target)
}

func waitForStartedJob(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case reportID := <-started:
		return reportID
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for generator to start")
		return ""
	}
}

type stubGenerator struct {
	generateFn func(ctx context.Context, req Request) (Result, error)
}

func (g *stubGenerator) Generate(ctx context.Context, req Request) (Result, error) {
	return g.generateFn(ctx, req)
}

type blockingGenerator struct {
	mu           sync.Mutex
	active       int
	maxActiveVal int
	started      chan<- string
	release      <-chan struct{}
}

func (g *blockingGenerator) Generate(_ context.Context, req Request) (Result, error) {
	g.mu.Lock()
	g.active++
	if g.active > g.maxActiveVal {
		g.maxActiveVal = g.active
	}
	g.mu.Unlock()

	g.started <- req.ReportID
	<-g.release

	outputDir := filepath.Join(req.WorkDir, "html")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return Result{}, err
	}
	indexPath := filepath.Join(outputDir, "index.html")
	if err := os.WriteFile(indexPath, []byte("<html>ok</html>"), 0o644); err != nil {
		return Result{}, err
	}

	g.mu.Lock()
	g.active--
	g.mu.Unlock()

	return Result{OutputDir: outputDir, IndexPath: indexPath}, nil
}

func (g *blockingGenerator) maxActive() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxActiveVal
}

type generationMemoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newGenerationMemoryStore() *generationMemoryStore {
	return &generationMemoryStore{objects: make(map[string][]byte)}
}

func (s *generationMemoryStore) Upload(_ context.Context, key string, body io.Reader, _ int64, _ string) error {
	payload, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.objects[key] = payload
	s.mu.Unlock()
	return nil
}

func (s *generationMemoryStore) Download(_ context.Context, key string) (storage.DownloadResult, error) {
	s.mu.Lock()
	payload, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		return storage.DownloadResult{}, fmt.Errorf("missing object %q", key)
	}
	return storage.DownloadResult{
		Body:          io.NopCloser(bytesReader(payload)),
		ContentLength: int64(len(payload)),
	}, nil
}

func (s *generationMemoryStore) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := make([]storage.ObjectInfo, 0)
	for key, payload := range s.objects {
		if stringsHasPrefix(key, prefix) {
			objects = append(objects, storage.ObjectInfo{Key: key, Size: int64(len(payload))})
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func (s *generationMemoryStore) object(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.objects[key]...)
}

func stringsRepeat(part string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += part
	}
	return result
}

func stringsHasPrefix(value, prefix string) bool {
	return len(prefix) == 0 || (len(value) >= len(prefix) && value[:len(prefix)] == prefix)
}

type byteReader struct {
	payload []byte
	offset  int
}

func bytesReader(payload []byte) *byteReader {
	return &byteReader{payload: append([]byte(nil), payload...)}
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.payload) {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.offset:])
	r.offset += n
	return n, nil
}

func (r *byteReader) Close() error { return nil }
