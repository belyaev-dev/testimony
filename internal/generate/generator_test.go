package generate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/storage"
)

func TestCommandExecutor(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		script := writeExecutable(t, "success.sh", "#!/bin/sh\necho 'executor warning' >&2\nexit 0\n")

		result, err := NewCommandExecutor().Run(context.Background(), CommandSpec{
			Path:    script,
			Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if got, want := result.Path, script; got != want {
			t.Fatalf("result.Path = %q, want %q", got, want)
		}
		if got, want := result.Stderr, "executor warning"; got != want {
			t.Fatalf("result.Stderr = %q, want %q", got, want)
		}
	})

	t.Run("non-zero exit captures stderr", func(t *testing.T) {
		script := writeExecutable(t, "fail.sh", "#!/bin/sh\necho 'fixture boom' >&2\nexit 17\n")

		result, err := NewCommandExecutor().Run(context.Background(), CommandSpec{
			Path:    script,
			Timeout: time.Second,
		})
		if err == nil {
			t.Fatal("Run() error = nil, want failure")
		}

		var cmdErr *CommandError
		if !errors.As(err, &cmdErr) {
			t.Fatalf("Run() error = %T, want *CommandError", err)
		}
		if got, want := cmdErr.ExitCode, 17; got != want {
			t.Fatalf("cmdErr.ExitCode = %d, want %d", got, want)
		}
		if cmdErr.TimedOut {
			t.Fatal("cmdErr.TimedOut = true, want false")
		}
		if got, want := result.Stderr, "fixture boom"; got != want {
			t.Fatalf("result.Stderr = %q, want %q", got, want)
		}
	})

	t.Run("timeout returns deterministic error", func(t *testing.T) {
		script := writeExecutable(t, "sleep.sh", "#!/bin/sh\nsleep 1\n")

		_, err := NewCommandExecutor().Run(context.Background(), CommandSpec{
			Path:    script,
			Timeout: 50 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("Run() error = nil, want timeout")
		}

		var cmdErr *CommandError
		if !errors.As(err, &cmdErr) {
			t.Fatalf("Run() error = %T, want *CommandError", err)
		}
		if !cmdErr.TimedOut {
			t.Fatal("cmdErr.TimedOut = false, want true")
		}
	})
}

func TestAllure(t *testing.T) {
	ctx := context.Background()

	t.Run("allure2 injects copied history into deterministic output", func(t *testing.T) {
		cliPath := copyFixtureCLI(t, "allure2.sh")
		generator := newGenerator(t, config.GenerateConfig{
			Variant:      config.GenerateVariantAllure2,
			CLIPath:      cliPath,
			Timeout:      time.Second,
			HistoryDepth: 2,
		})

		resultsDir := createResultsDir(t)
		store := &memoryHistoryStore{objects: map[string][]byte{
			"projects/demo/reports/older/html/history/history-trend.json": []byte("older"),
			"projects/demo/reports/newer/html/history/history-trend.json": []byte("newer"),
			"projects/demo/reports/newer/html/history/history.json":       []byte(`{"suite":"widgets"}`),
		}}
		setHistoryStore(generator, store)

		result, err := generator.Generate(ctx, Request{
			ProjectSlug: "demo",
			ReportID:    "current",
			ResultsDir:  resultsDir,
			WorkDir:     filepath.Join(t.TempDir(), "work"),
			ReadyReports: []db.Report{
				readyReport("older", "projects/demo/reports/older/html/index.html", time.Date(2026, time.March, 24, 11, 0, 0, 0, time.UTC)),
				readyReport("newer", "projects/demo/reports/newer/html/index.html", time.Date(2026, time.March, 24, 12, 0, 0, 0, time.UTC)),
			},
		})
		if err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		if got, want := result.OutputDir, filepath.Join(filepath.Dir(result.OutputDir), "html"); got != want {
			t.Fatalf("result.OutputDir = %q, want deterministic html dir %q", got, want)
		}
		if _, err := os.Stat(result.IndexPath); err != nil {
			t.Fatalf("generated index missing: %v", err)
		}
		payload, err := os.ReadFile(filepath.Join(result.OutputDir, "history", "history-trend.json"))
		if err != nil {
			t.Fatalf("ReadFile(history-trend.json) error = %v", err)
		}
		if got, want := string(payload), "newer"; got != want {
			t.Fatalf("history-trend.json = %q, want %q", got, want)
		}
	})

	t.Run("allure3 writes merged history file via generated config", func(t *testing.T) {
		cliPath := copyFixtureCLI(t, "allure3.sh")
		generator := newGenerator(t, config.GenerateConfig{
			Variant:      config.GenerateVariantAllure3,
			CLIPath:      cliPath,
			Timeout:      time.Second,
			HistoryDepth: 2,
		})

		resultsDir := createResultsDir(t)
		store := &memoryHistoryStore{objects: map[string][]byte{
			"projects/demo/reports/r1/html/history/history.jsonl": []byte("line-1\nline-2\n"),
			"projects/demo/reports/r2/html/history/history.jsonl": []byte("line-1\nline-2\nline-3\n"),
		}}
		setHistoryStore(generator, store)

		result, err := generator.Generate(ctx, Request{
			ProjectSlug: "demo",
			ReportID:    "current",
			ResultsDir:  resultsDir,
			WorkDir:     filepath.Join(t.TempDir(), "work"),
			ReadyReports: []db.Report{
				readyReport("r1", "projects/demo/reports/r1/html/index.html", time.Date(2026, time.March, 24, 10, 0, 0, 0, time.UTC)),
				readyReport("r2", "projects/demo/reports/r2/html/index.html", time.Date(2026, time.March, 24, 11, 0, 0, 0, time.UTC)),
			},
		})
		if err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		payload, err := os.ReadFile(filepath.Join(result.OutputDir, "history", "history.jsonl"))
		if err != nil {
			t.Fatalf("ReadFile(history.jsonl) error = %v", err)
		}
		if got, want := string(payload), "line-2\nline-3\n"; got != want {
			t.Fatalf("history.jsonl = %q, want %q", got, want)
		}
	})

	t.Run("non-zero exit becomes generator error", func(t *testing.T) {
		cliPath := writeExecutable(t, "generator-fail.sh", "#!/bin/sh\necho 'fixture boom' >&2\nexit 23\n")
		generator := newGenerator(t, config.GenerateConfig{
			Variant:      config.GenerateVariantAllure2,
			CLIPath:      cliPath,
			Timeout:      time.Second,
			HistoryDepth: 0,
		})

		_, err := generator.Generate(ctx, Request{
			ProjectSlug: "demo",
			ReportID:    "current",
			ResultsDir:  createResultsDir(t),
			WorkDir:     filepath.Join(t.TempDir(), "work"),
		})
		if err == nil {
			t.Fatal("Generate() error = nil, want failure")
		}

		var genErr *GenerationError
		if !errors.As(err, &genErr) {
			t.Fatalf("Generate() error = %T, want *GenerationError", err)
		}
		if got, want := genErr.Variant, VariantAllure2; got != want {
			t.Fatalf("genErr.Variant = %q, want %q", got, want)
		}
		if !strings.Contains(genErr.Stderr, "fixture boom") {
			t.Fatalf("genErr.Stderr = %q, want fixture stderr", genErr.Stderr)
		}
	})
}

func TestHistory(t *testing.T) {
	ctx := context.Background()

	t.Run("allure2 copies older then newer so latest wins", func(t *testing.T) {
		store := &memoryHistoryStore{objects: map[string][]byte{
			"projects/demo/reports/older/html/history/history-trend.json": []byte("older"),
			"projects/demo/reports/newer/html/history/history-trend.json": []byte("newer"),
		}}

		resultsDir := t.TempDir()
		sources, err := mergeAllure2History(ctx, store, []db.Report{
			readyReport("older", "projects/demo/reports/older/html/index.html", time.Date(2026, time.March, 24, 9, 0, 0, 0, time.UTC)),
			readyReport("newer", "projects/demo/reports/newer/html/index.html", time.Date(2026, time.March, 24, 10, 0, 0, 0, time.UTC)),
		}, 2, resultsDir)
		if err != nil {
			t.Fatalf("mergeAllure2History() error = %v", err)
		}
		if len(sources) != 2 {
			t.Fatalf("len(sources) = %d, want 2", len(sources))
		}
		payload, err := os.ReadFile(filepath.Join(resultsDir, "history", "history-trend.json"))
		if err != nil {
			t.Fatalf("ReadFile(history-trend.json) error = %v", err)
		}
		if got, want := string(payload), "newer"; got != want {
			t.Fatalf("history-trend.json = %q, want %q", got, want)
		}
	})

	t.Run("allure3 rejects malformed empty history input", func(t *testing.T) {
		store := &memoryHistoryStore{objects: map[string][]byte{
			"projects/demo/reports/r1/html/history/history.jsonl": []byte("\n"),
		}}

		_, err := mergeAllure3History(ctx, store, []db.Report{
			readyReport("r1", "projects/demo/reports/r1/html/index.html", time.Date(2026, time.March, 24, 9, 0, 0, 0, time.UTC)),
		}, 1, filepath.Join(t.TempDir(), "history", "history.jsonl"))
		if err == nil {
			t.Fatal("mergeAllure3History() error = nil, want malformed history error")
		}
		if !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("mergeAllure3History() error = %v, want empty history error", err)
		}
	})
}

func newGenerator(t *testing.T, cfg config.GenerateConfig) *runner {
	t.Helper()
	generator, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	runner, ok := generator.(*runner)
	if !ok {
		t.Fatalf("New() type = %T, want *runner", generator)
	}
	return runner
}

func setHistoryStore(generator *runner, store HistoryStore) {
	generator.historyStore = store
}

func createResultsDir(t *testing.T) string {
	t.Helper()
	resultsDir := filepath.Join(t.TempDir(), "allure-results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", resultsDir, err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "widgets-result.json"), []byte(`{"name":"widgets"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(results) error = %v", err)
	}
	return resultsDir
}

func readyReport(id, generatedObjectKey string, completedAt time.Time) db.Report {
	completed := completedAt.UTC()
	return db.Report{
		ID:                 id,
		Status:             db.ReportStatusReady,
		GeneratedObjectKey: generatedObjectKey,
		CreatedAt:          completedAt.Add(-time.Minute).UTC(),
		CompletedAt:        &completed,
	}
}

func writeExecutable(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}

func copyFixtureCLI(t *testing.T, name string) string {
	t.Helper()
	sourcePath := filepath.Join("testdata", name)
	payload, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", sourcePath, err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, payload, 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}

type memoryHistoryStore struct {
	objects map[string][]byte
}

func (s *memoryHistoryStore) Download(_ context.Context, key string) (storage.DownloadResult, error) {
	payload, ok := s.objects[key]
	if !ok {
		return storage.DownloadResult{}, fmt.Errorf("missing object %q", key)
	}
	return storage.DownloadResult{
		Body:          io.NopCloser(strings.NewReader(string(payload))),
		ContentLength: int64(len(payload)),
	}, nil
}

func (s *memoryHistoryStore) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	objects := make([]storage.ObjectInfo, 0)
	for key, payload := range s.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, storage.ObjectInfo{Key: key, Size: int64(len(payload))})
		}
	}
	return objects, nil
}
