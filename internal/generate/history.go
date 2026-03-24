package generate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/testimony-dev/testimony/internal/db"
)

const allure3HistoryObjectName = "history/history.jsonl"

func mergeAllure2History(ctx context.Context, store HistoryStore, reports []db.Report, depth int, resultsDir string) ([]string, error) {
	selected := selectReadyReports(reports, depth)
	if len(selected) == 0 {
		return nil, nil
	}
	if store == nil {
		return nil, fmt.Errorf("merge allure2 history: nil history store")
	}

	targetDir := filepath.Join(resultsDir, "history")
	copied := make([]string, 0)
	for i := len(selected) - 1; i >= 0; i-- {
		prefix, err := reportHTMLPrefix(selected[i].GeneratedObjectKey)
		if err != nil {
			return nil, fmt.Errorf("merge allure2 history for report %q: %w", selected[i].ID, err)
		}

		objects, err := store.List(ctx, prefix+"history/")
		if err != nil {
			return nil, fmt.Errorf("list allure2 history for report %q: %w", selected[i].ID, err)
		}
		if len(objects) == 0 {
			return nil, fmt.Errorf("list allure2 history for report %q: no history objects found", selected[i].ID)
		}

		sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
		for _, object := range objects {
			rel, err := historyRelativePath(prefix+"history/", object.Key)
			if err != nil {
				return nil, fmt.Errorf("resolve allure2 history object %q: %w", object.Key, err)
			}
			if err := downloadObject(ctx, store, object.Key, filepath.Join(targetDir, rel)); err != nil {
				return nil, fmt.Errorf("copy allure2 history object %q: %w", object.Key, err)
			}
			copied = append(copied, object.Key)
		}
	}

	return copied, nil
}

func mergeAllure3History(ctx context.Context, store HistoryStore, reports []db.Report, depth int, historyPath string) ([]string, error) {
	selected := selectReadyReports(reports, depth)
	if len(selected) == 0 {
		return nil, nil
	}
	if store == nil {
		return nil, fmt.Errorf("merge allure3 history: nil history store")
	}
	if strings.TrimSpace(historyPath) == "" {
		return nil, fmt.Errorf("merge allure3 history: empty history path")
	}

	lines := make([]string, 0)
	seen := make(map[string]struct{})
	sources := make([]string, 0, len(selected))
	for i := len(selected) - 1; i >= 0; i-- {
		prefix, err := reportHTMLPrefix(selected[i].GeneratedObjectKey)
		if err != nil {
			return nil, fmt.Errorf("merge allure3 history for report %q: %w", selected[i].ID, err)
		}
		objectKey := prefix + allure3HistoryObjectName
		object, err := store.Download(ctx, objectKey)
		if err != nil {
			return nil, fmt.Errorf("download allure3 history for report %q: %w", selected[i].ID, err)
		}
		payload, err := io.ReadAll(object.Body)
		object.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read allure3 history object %q: %w", objectKey, err)
		}

		scanner := bufio.NewScanner(bytes.NewReader(payload))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		hadLine := false
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			hadLine = true
			if _, ok := seen[line]; ok {
				continue
			}
			seen[line] = struct{}{}
			lines = append(lines, line)
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("scan allure3 history object %q: %w", objectKey, err)
		}
		if !hadLine {
			return nil, fmt.Errorf("allure3 history object %q is empty", objectKey)
		}
		sources = append(sources, objectKey)
	}

	if depth > 0 && len(lines) > depth {
		lines = append([]string(nil), lines[len(lines)-depth:]...)
	}
	if len(lines) == 0 {
		return nil, nil
	}

	if err := os.MkdirAll(filepath.Dir(historyPath), 0o755); err != nil {
		return nil, fmt.Errorf("create allure3 history directory for %q: %w", historyPath, err)
	}
	payload := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(historyPath, []byte(payload), 0o644); err != nil {
		return nil, fmt.Errorf("write allure3 history file %q: %w", historyPath, err)
	}

	return sources, nil
}

func selectReadyReports(reports []db.Report, depth int) []db.Report {
	if depth <= 0 {
		return nil
	}

	selected := make([]db.Report, 0, len(reports))
	for _, report := range reports {
		if report.Status != db.ReportStatusReady {
			continue
		}
		if strings.TrimSpace(report.GeneratedObjectKey) == "" {
			continue
		}
		selected = append(selected, report)
	}

	sort.SliceStable(selected, func(i, j int) bool {
		return reportSortTime(selected[i]).After(reportSortTime(selected[j]))
	})
	if len(selected) > depth {
		selected = append([]db.Report(nil), selected[:depth]...)
	}
	return selected
}

func reportSortTime(report db.Report) time.Time {
	if report.CompletedAt != nil {
		return report.CompletedAt.UTC()
	}
	return report.CreatedAt.UTC()
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

func historyRelativePath(prefix, objectKey string) (string, error) {
	trimmedPrefix := strings.TrimSpace(prefix)
	trimmedKey := strings.TrimSpace(objectKey)
	if !strings.HasPrefix(trimmedKey, trimmedPrefix) {
		return "", fmt.Errorf("history object %q does not match prefix %q", objectKey, prefix)
	}
	rel := path.Clean(strings.TrimPrefix(trimmedKey, trimmedPrefix))
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("history object %q resolves outside prefix", objectKey)
	}
	return filepath.FromSlash(rel), nil
}

func downloadObject(ctx context.Context, store HistoryStore, key, targetPath string) error {
	object, err := store.Download(ctx, key)
	if err != nil {
		return err
	}
	defer object.Body.Close()

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create history directory for %q: %w", targetPath, err)
	}

	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create history file %q: %w", targetPath, err)
	}
	defer file.Close()

	if _, err := io.Copy(file, object.Body); err != nil {
		return fmt.Errorf("write history file %q: %w", targetPath, err)
	}
	return nil
}
