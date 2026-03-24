package generate

import "context"

func (r *runner) generateAllure2(ctx context.Context, req Request, ws workspace) (Result, error) {
	historySources, err := mergeAllure2History(ctx, r.historyStore, req.ReadyReports, r.historyDepth, ws.preparedResultsDir)
	if err != nil {
		return Result{}, r.wrapError(req, "", err)
	}

	command, err := r.executor.Run(ctx, CommandSpec{
		Path:    r.cliPath,
		Args:    []string{"generate", ws.preparedResultsDir, "--clean", "-o", ws.outputDir},
		Dir:     ws.workDir,
		Timeout: r.timeout,
	})
	if err != nil {
		return Result{}, r.wrapError(req, command.Stderr, err)
	}

	indexPath, err := ensureGeneratedIndex(ws.outputDir)
	if err != nil {
		return Result{}, r.wrapError(req, command.Stderr, err)
	}

	return Result{
		Variant:            VariantAllure2,
		ProjectSlug:        req.ProjectSlug,
		ReportID:           req.ReportID,
		PreparedResultsDir: ws.preparedResultsDir,
		OutputDir:          ws.outputDir,
		IndexPath:          indexPath,
		HistorySources:     historySources,
		Command:            command,
	}, nil
}
