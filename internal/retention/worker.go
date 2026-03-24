package retention

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/storage"
)

type Store interface {
	ListExpiredReports(ctx context.Context, globalRetentionDays int, now time.Time) ([]db.Report, error)
	DeleteReport(ctx context.Context, reportID string) error
}

type ObjectStore interface {
	List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

type WorkerOptions struct {
	Logger              *slog.Logger
	Store               Store
	Storage             ObjectStore
	GlobalRetentionDays int
	Interval            time.Duration
	Now                 func() time.Time
}

type Worker struct {
	logger              *slog.Logger
	store               Store
	storage             ObjectStore
	globalRetentionDays int
	interval            time.Duration
	now                 func() time.Time
	newTicker           func(time.Duration) ticker

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct {
	inner *time.Ticker
}

func (t realTicker) C() <-chan time.Time {
	return t.inner.C
}

func (t realTicker) Stop() {
	t.inner.Stop()
}

func NewWorker(opts WorkerOptions) (*Worker, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("new retention worker: nil store")
	}
	if opts.Storage == nil {
		return nil, fmt.Errorf("new retention worker: nil storage")
	}
	if opts.GlobalRetentionDays < 0 {
		return nil, fmt.Errorf("new retention worker: global retention days must be zero or greater")
	}
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("new retention worker: interval must be greater than zero")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &Worker{
		logger:              logger,
		store:               opts.Store,
		storage:             opts.Storage,
		globalRetentionDays: opts.GlobalRetentionDays,
		interval:            opts.Interval,
		now:                 now,
		newTicker: func(interval time.Duration) ticker {
			return realTicker{inner: time.NewTicker(interval)}
		},
	}, nil
}

func (w *Worker) RunOnce(ctx context.Context) error {
	referenceTime := w.now().UTC()
	reports, err := w.store.ListExpiredReports(ctx, w.globalRetentionDays, referenceTime)
	if err != nil {
		return fmt.Errorf("list expired reports: %w", err)
	}

	var cleanupErrs []error
	for _, report := range reports {
		if err := w.cleanupReport(ctx, report); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}

	return errors.Join(cleanupErrs...)
}

func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return fmt.Errorf("retention worker already started")
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	ticker := w.newTicker(w.interval)

	w.running = true
	w.cancel = cancel
	w.done = done

	w.logger.Info("retention worker started",
		"global_retention_days", w.globalRetentionDays,
		"interval", w.interval.String(),
	)

	go func() {
		defer func() {
			ticker.Stop()
			w.mu.Lock()
			w.running = false
			w.cancel = nil
			w.done = nil
			w.mu.Unlock()
			w.logger.Info("retention worker stopped")
			close(done)
		}()

		if err := w.RunOnce(runCtx); err != nil {
			w.logger.Warn("retention worker run failed",
				"cleanup_phase", "run_once",
				"deletion_outcome", "retained_for_retry",
				"error", err,
			)
		}

		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C():
				if err := w.RunOnce(runCtx); err != nil {
					w.logger.Warn("retention worker run failed",
						"cleanup_phase", "run_once",
						"deletion_outcome", "retained_for_retry",
						"error", err,
					)
				}
			}
		}
	}()

	return nil
}

func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel := w.cancel
	done := w.done
	w.mu.Unlock()

	if cancel == nil || done == nil {
		return nil
	}

	cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop retention worker: %w", ctx.Err())
	}
}

func (w *Worker) cleanupReport(ctx context.Context, report db.Report) error {
	objectKeys, err := w.objectKeysForReport(ctx, report)
	if err != nil {
		w.logCleanupFailure(report, "list_objects", err, "")
		return fmt.Errorf("cleanup report %q: %w", report.ID, err)
	}

	for _, objectKey := range objectKeys {
		if err := w.storage.Delete(ctx, objectKey); err != nil {
			w.logCleanupFailure(report, "delete_object", err, objectKey)
			return fmt.Errorf("cleanup report %q: delete object %q: %w", report.ID, objectKey, err)
		}
	}

	if err := w.store.DeleteReport(ctx, report.ID); err != nil {
		w.logCleanupFailure(report, "delete_row", err, "")
		return fmt.Errorf("cleanup report %q: %w", report.ID, err)
	}

	w.logger.Info("retention cleanup succeeded",
		"project_slug", report.ProjectSlug,
		"report_id", report.ID,
		"cleanup_phase", "delete_row",
		"deletion_outcome", "deleted",
		"deleted_objects", len(objectKeys),
	)

	return nil
}

func (w *Worker) objectKeysForReport(ctx context.Context, report db.Report) ([]string, error) {
	keys := make([]string, 0)
	seen := make(map[string]struct{})
	addKey := func(key string) {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			return
		}
		if _, ok := seen[trimmed]; ok {
			return
		}
		seen[trimmed] = struct{}{}
		keys = append(keys, trimmed)
	}

	if trimmedGenerated := strings.TrimSpace(report.GeneratedObjectKey); trimmedGenerated != "" {
		prefix, err := reportHTMLPrefix(trimmedGenerated)
		if err != nil {
			return nil, fmt.Errorf("resolve generated object prefix: %w", err)
		}
		objects, err := w.storage.List(ctx, prefix)
		if err != nil {
			return nil, fmt.Errorf("list generated objects with prefix %q: %w", prefix, err)
		}
		sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
		for _, object := range objects {
			addKey(object.Key)
		}
	}

	addKey(report.ArchiveObjectKey)
	return keys, nil
}

func (w *Worker) logCleanupFailure(report db.Report, phase string, err error, objectKey string) {
	attrs := []any{
		"project_slug", report.ProjectSlug,
		"report_id", report.ID,
		"cleanup_phase", phase,
		"deletion_outcome", "retained_for_retry",
		"error", err,
	}
	if trimmedObjectKey := strings.TrimSpace(objectKey); trimmedObjectKey != "" {
		attrs = append(attrs, "object_key", trimmedObjectKey)
	}
	w.logger.Warn("retention cleanup failed", attrs...)
}

func reportHTMLPrefix(generatedObjectKey string) (string, error) {
	trimmed := strings.TrimSpace(generatedObjectKey)
	if trimmed == "" {
		return "", fmt.Errorf("empty generated object key")
	}

	dir := path.Dir(trimmed)
	if dir == "." || dir == "/" {
		return "", fmt.Errorf("invalid generated object key %q", generatedObjectKey)
	}

	return strings.TrimSuffix(dir, "/") + "/", nil
}
