package retention

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/storage"
)

func TestWorkerRunOnceDeletesObjectsBeforeRow(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	operations := make([]string, 0)

	report := db.Report{
		ID:                 "report-1",
		ProjectSlug:        "widgets",
		ArchiveObjectKey:   "projects/widgets/reports/report-1/archive.zip",
		GeneratedObjectKey: "projects/widgets/reports/report-1/html/index.html",
		Status:             db.ReportStatusReady,
	}

	store := &fakeStore{expired: []db.Report{report}, operations: &operations}
	objects := &fakeObjectStore{
		listed: map[string][]storage.ObjectInfo{
			"projects/widgets/reports/report-1/html/": {
				{Key: "projects/widgets/reports/report-1/html/assets/app.js"},
				{Key: "projects/widgets/reports/report-1/html/index.html"},
			},
		},
		operations: &operations,
	}

	worker, err := NewWorker(WorkerOptions{
		Logger:              logger,
		Store:               store,
		Storage:             objects,
		GlobalRetentionDays: 30,
		Interval:            time.Minute,
		Now:                 func() time.Time { return time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}

	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}

	wantOps := []string{
		"list:projects/widgets/reports/report-1/html/",
		"delete:projects/widgets/reports/report-1/html/assets/app.js",
		"delete:projects/widgets/reports/report-1/html/index.html",
		"delete:projects/widgets/reports/report-1/archive.zip",
		"delete_row:report-1",
	}
	if got := operations; !equalStrings(got, wantOps) {
		t.Fatalf("operations = %v, want %v", got, wantOps)
	}
	if len(store.deletedReportIDs) != 1 || store.deletedReportIDs[0] != report.ID {
		t.Fatalf("deleted report IDs = %v, want [%s]", store.deletedReportIDs, report.ID)
	}
	for _, fragment := range []string{"retention cleanup succeeded", "project_slug=widgets", "report_id=report-1", "cleanup_phase=delete_row", "deletion_outcome=deleted"} {
		if !strings.Contains(logBuf.String(), fragment) {
			t.Fatalf("expected logs to contain %q, got %q", fragment, logBuf.String())
		}
	}
}

func TestWorkerRunOnceLeavesRowForRetryWhenObjectDeleteFails(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	operations := make([]string, 0)

	report := db.Report{
		ID:                 "report-2",
		ProjectSlug:        "widgets",
		ArchiveObjectKey:   "projects/widgets/reports/report-2/archive.zip",
		GeneratedObjectKey: "projects/widgets/reports/report-2/html/index.html",
		Status:             db.ReportStatusReady,
	}

	store := &fakeStore{expired: []db.Report{report}, operations: &operations}
	objects := &fakeObjectStore{
		listed: map[string][]storage.ObjectInfo{
			"projects/widgets/reports/report-2/html/": {
				{Key: "projects/widgets/reports/report-2/html/index.html"},
			},
		},
		deleteErrs: map[string]error{
			"projects/widgets/reports/report-2/html/index.html": errors.New("boom"),
		},
		operations: &operations,
	}

	worker, err := NewWorker(WorkerOptions{
		Logger:              logger,
		Store:               store,
		Storage:             objects,
		GlobalRetentionDays: 30,
		Interval:            time.Minute,
		Now:                 func() time.Time { return time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}

	err = worker.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("RunOnce() error = %v, want delete failure", err)
	}
	if len(store.deletedReportIDs) != 0 {
		t.Fatalf("deleted report IDs = %v, want none", store.deletedReportIDs)
	}
	if got := operations; len(got) != 2 || got[0] != "list:projects/widgets/reports/report-2/html/" || got[1] != "delete:projects/widgets/reports/report-2/html/index.html" {
		t.Fatalf("operations = %v, want list then failing delete only", got)
	}
	for _, fragment := range []string{"retention cleanup failed", "cleanup_phase=delete_object", "deletion_outcome=retained_for_retry", "project_slug=widgets", "report_id=report-2"} {
		if !strings.Contains(logBuf.String(), fragment) {
			t.Fatalf("expected logs to contain %q, got %q", fragment, logBuf.String())
		}
	}
}

func TestWorkerStartStopLifecycle(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	store := &fakeStore{callCh: make(chan struct{}, 4)}
	objects := &fakeObjectStore{}
	worker, err := NewWorker(WorkerOptions{
		Logger:              logger,
		Store:               store,
		Storage:             objects,
		GlobalRetentionDays: 0,
		Interval:            time.Minute,
		Now:                 func() time.Time { return time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}

	manual := &manualTicker{ch: make(chan time.Time, 2)}
	worker.newTicker = func(time.Duration) ticker { return manual }

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(runCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	waitForCall(t, store.callCh, "initial run")
	manual.ch <- time.Date(2026, time.March, 24, 12, 1, 0, 0, time.UTC)
	waitForCall(t, store.callCh, "tick run")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := worker.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if !manual.stopped {
		t.Fatal("manual ticker was not stopped")
	}
	if got, want := store.listCalls, 2; got < want {
		t.Fatalf("store.ListExpiredReports() calls = %d, want at least %d", got, want)
	}
	for _, fragment := range []string{"retention worker started", "retention worker stopped"} {
		if !strings.Contains(logBuf.String(), fragment) {
			t.Fatalf("expected logs to contain %q, got %q", fragment, logBuf.String())
		}
	}
}

type fakeStore struct {
	mu               sync.Mutex
	expired          []db.Report
	deletedReportIDs []string
	deleteErrs       map[string]error
	listErr          error
	listCalls        int
	callCh           chan struct{}
	operations       *[]string
}

func (s *fakeStore) ListExpiredReports(_ context.Context, _ int, _ time.Time) ([]db.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.callCh != nil {
		select {
		case s.callCh <- struct{}{}:
		default:
		}
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]db.Report(nil), s.expired...), nil
}

func (s *fakeStore) DeleteReport(_ context.Context, reportID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operations != nil {
		*s.operations = append(*s.operations, "delete_row:"+reportID)
	}
	if err := s.deleteErrs[reportID]; err != nil {
		return err
	}
	s.deletedReportIDs = append(s.deletedReportIDs, reportID)
	return nil
}

type fakeObjectStore struct {
	mu         sync.Mutex
	listed     map[string][]storage.ObjectInfo
	listErrs   map[string]error
	deleteErrs map[string]error
	operations *[]string
}

func (s *fakeObjectStore) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operations != nil {
		*s.operations = append(*s.operations, "list:"+prefix)
	}
	if err := s.listErrs[prefix]; err != nil {
		return nil, err
	}
	return append([]storage.ObjectInfo(nil), s.listed[prefix]...), nil
}

func (s *fakeObjectStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operations != nil {
		*s.operations = append(*s.operations, "delete:"+key)
	}
	if err := s.deleteErrs[key]; err != nil {
		return err
	}
	return nil
}

type manualTicker struct {
	ch      chan time.Time
	stopped bool
}

func (t *manualTicker) C() <-chan time.Time {
	return t.ch
}

func (t *manualTicker) Stop() {
	t.stopped = true
}

func waitForCall(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func Example_reportHTMLPrefix() {
	prefix, _ := reportHTMLPrefix("projects/demo/reports/r1/html/index.html")
	fmt.Println(prefix)
	// Output: projects/demo/reports/r1/html/
}
