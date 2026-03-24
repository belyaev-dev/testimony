package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type allure3Config struct {
	Output        string `json:"output"`
	HistoryPath   string `json:"historyPath,omitempty"`
	AppendHistory bool   `json:"appendHistory,omitempty"`
}

func (r *runner) generateAllure3(ctx context.Context, req Request, ws workspace) (Result, error) {
	historyPath := filepath.Join(ws.outputDir, "history", "history.jsonl")
	historySources, err := mergeAllure3History(ctx, r.historyStore, req.ReadyReports, r.historyDepth, historyPath)
	if err != nil {
		return Result{}, r.wrapError(req, "", err)
	}
	if len(historySources) == 0 {
		historyPath = ""
	}

	configPath, err := writeAllure3Config(ws.workDir, ws.outputDir, historyPath)
	if err != nil {
		return Result{}, r.wrapError(req, "", err)
	}

	command, err := r.executor.Run(ctx, CommandSpec{
		Path:    r.cliPath,
		Args:    []string{"generate", ws.preparedResultsDir},
		Dir:     ws.workDir,
		Timeout: r.timeout,
	})
	if err != nil {
		return Result{}, r.wrapError(req, command.Stderr, fmt.Errorf("use config %q: %w", configPath, err))
	}

	indexPath, err := ensureGeneratedIndex(ws.outputDir)
	if err != nil {
		return Result{}, r.wrapError(req, command.Stderr, err)
	}

	return Result{
		Variant:            VariantAllure3,
		ProjectSlug:        req.ProjectSlug,
		ReportID:           req.ReportID,
		PreparedResultsDir: ws.preparedResultsDir,
		OutputDir:          ws.outputDir,
		IndexPath:          indexPath,
		HistoryPath:        historyPath,
		HistorySources:     historySources,
		Command:            command,
	}, nil
}

func writeAllure3Config(workDir, outputDir, historyPath string) (string, error) {
	cfg := allure3Config{
		Output: outputDir,
	}
	if historyPath != "" {
		cfg.HistoryPath = historyPath
		cfg.AppendHistory = true
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal allure3 config: %w", err)
	}

	configPath := filepath.Join(workDir, "allurerc.json")
	if err := os.WriteFile(configPath, payload, 0o644); err != nil {
		return "", fmt.Errorf("write allure3 config %q: %w", configPath, err)
	}
	return configPath, nil
}
