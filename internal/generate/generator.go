package generate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/storage"
)

type Variant = config.GenerateVariant

const (
	VariantAllure2 = config.GenerateVariantAllure2
	VariantAllure3 = config.GenerateVariantAllure3
)

type Request struct {
	ProjectSlug  string
	ReportID     string
	ResultsDir   string
	WorkDir      string
	ReadyReports []db.Report
}

type Result struct {
	Variant            Variant
	ProjectSlug        string
	ReportID           string
	PreparedResultsDir string
	OutputDir          string
	IndexPath          string
	HistoryPath        string
	HistorySources     []string
	Command            CommandResult
}

type Generator interface {
	Generate(ctx context.Context, req Request) (Result, error)
}

type HistoryStore interface {
	Download(ctx context.Context, key string) (storage.DownloadResult, error)
	List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error)
}

type runner struct {
	variant      Variant
	cliPath      string
	timeout      time.Duration
	historyDepth int
	executor     Executor
	historyStore HistoryStore
}

type workspace struct {
	workDir            string
	preparedResultsDir string
	outputDir          string
}

type GenerationError struct {
	Variant     Variant
	ProjectSlug string
	ReportID    string
	CommandPath string
	Timeout     time.Duration
	Stderr      string
	Cause       error
}

func (e *GenerationError) Error() string {
	if e == nil {
		return "generate report: <nil>"
	}

	parts := []string{fmt.Sprintf("generate report %q for project %q with %s", e.ReportID, e.ProjectSlug, e.Variant)}
	if strings.TrimSpace(e.CommandPath) != "" {
		parts = append(parts, fmt.Sprintf("command=%q", e.CommandPath))
	}
	if e.Timeout > 0 {
		parts = append(parts, fmt.Sprintf("timeout=%s", e.Timeout))
	}
	if snippet := strings.TrimSpace(e.Stderr); snippet != "" {
		parts = append(parts, fmt.Sprintf("stderr=%q", snippet))
	}
	if e.Cause != nil {
		parts = append(parts, fmt.Sprintf("cause=%v", e.Cause))
	}
	return strings.Join(parts, " ")
}

func (e *GenerationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func New(cfg config.GenerateConfig, historyStore HistoryStore, executor Executor) (Generator, error) {
	if executor == nil {
		executor = NewCommandExecutor()
	}

	r := &runner{
		variant:      cfg.Variant,
		cliPath:      strings.TrimSpace(cfg.CLIPath),
		timeout:      cfg.Timeout,
		historyDepth: cfg.HistoryDepth,
		executor:     executor,
		historyStore: historyStore,
	}

	if err := r.validateConfig(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *runner) Generate(ctx context.Context, req Request) (Result, error) {
	if err := r.validateRequest(req); err != nil {
		return Result{}, r.wrapError(req, "", err)
	}

	ws, err := prepareWorkspace(req)
	if err != nil {
		return Result{}, r.wrapError(req, "", err)
	}

	switch r.variant {
	case VariantAllure2:
		return r.generateAllure2(ctx, req, ws)
	case VariantAllure3:
		return r.generateAllure3(ctx, req, ws)
	default:
		return Result{}, r.wrapError(req, "", fmt.Errorf("unsupported generate variant %q", r.variant))
	}
}

func (r *runner) validateConfig() error {
	switch r.variant {
	case VariantAllure2, VariantAllure3:
	default:
		return fmt.Errorf("new generator: unsupported variant %q", r.variant)
	}
	if r.cliPath == "" {
		return fmt.Errorf("new generator: empty cli path")
	}
	if r.timeout <= 0 {
		return fmt.Errorf("new generator: timeout must be greater than zero")
	}
	if r.historyDepth < 0 {
		return fmt.Errorf("new generator: history depth must be zero or greater")
	}
	return nil
}

func (r *runner) validateRequest(req Request) error {
	if strings.TrimSpace(req.ProjectSlug) == "" {
		return fmt.Errorf("empty project slug")
	}
	if strings.TrimSpace(req.ReportID) == "" {
		return fmt.Errorf("empty report id")
	}
	resultsDir := filepath.Clean(strings.TrimSpace(req.ResultsDir))
	if resultsDir == "" || resultsDir == "." {
		return fmt.Errorf("empty results dir")
	}
	info, err := os.Stat(resultsDir)
	if err != nil {
		return fmt.Errorf("stat results dir %q: %w", resultsDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("results dir %q is not a directory", resultsDir)
	}
	workDir := filepath.Clean(strings.TrimSpace(req.WorkDir))
	if workDir == "" || workDir == "." {
		return fmt.Errorf("empty work dir")
	}
	if rel, err := filepath.Rel(resultsDir, workDir); err == nil {
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return fmt.Errorf("work dir %q must not be inside results dir %q", workDir, resultsDir)
		}
	}
	return nil
}

func prepareWorkspace(req Request) (workspace, error) {
	ws := workspace{
		workDir:            filepath.Clean(strings.TrimSpace(req.WorkDir)),
		preparedResultsDir: filepath.Join(filepath.Clean(strings.TrimSpace(req.WorkDir)), "results"),
		outputDir:          filepath.Join(filepath.Clean(strings.TrimSpace(req.WorkDir)), "html"),
	}

	if err := os.MkdirAll(ws.workDir, 0o755); err != nil {
		return workspace{}, fmt.Errorf("create work dir %q: %w", ws.workDir, err)
	}
	if err := os.RemoveAll(ws.preparedResultsDir); err != nil {
		return workspace{}, fmt.Errorf("remove prepared results dir %q: %w", ws.preparedResultsDir, err)
	}
	if err := os.RemoveAll(ws.outputDir); err != nil {
		return workspace{}, fmt.Errorf("remove output dir %q: %w", ws.outputDir, err)
	}
	if err := copyTree(filepath.Clean(strings.TrimSpace(req.ResultsDir)), ws.preparedResultsDir); err != nil {
		return workspace{}, fmt.Errorf("copy results into workspace: %w", err)
	}
	if err := os.MkdirAll(ws.outputDir, 0o755); err != nil {
		return workspace{}, fmt.Errorf("create output dir %q: %w", ws.outputDir, err)
	}

	return ws, nil
}

func copyTree(srcDir, dstDir string) error {
	srcDir = filepath.Clean(strings.TrimSpace(srcDir))
	dstDir = filepath.Clean(strings.TrimSpace(dstDir))
	if srcDir == "" || srcDir == "." {
		return fmt.Errorf("empty source dir")
	}
	if dstDir == "" || dstDir == "." {
		return fmt.Errorf("empty destination dir")
	}

	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("resolve path %q: %w", path, err)
		}
		if rel == "." {
			return os.MkdirAll(dstDir, 0o755)
		}

		targetPath := filepath.Join(dstDir, rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("copy results: unsupported symlink %q", path)
		}
		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("create destination directory for %q: %w", targetPath, err)
		}
		return copyFile(path, targetPath)
	})
}

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source file %q: %w", srcPath, err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat source file %q: %w", srcPath, err)
	}

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create destination file %q: %w", dstPath, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy %q to %q: %w", srcPath, dstPath, err)
	}

	return nil
}

func ensureGeneratedIndex(outputDir string) (string, error) {
	indexPath := filepath.Join(outputDir, "index.html")
	info, err := os.Stat(indexPath)
	if err != nil {
		return "", fmt.Errorf("generated index %q: %w", indexPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("generated index %q is a directory", indexPath)
	}
	return indexPath, nil
}

func (r *runner) wrapError(req Request, stderr string, err error) error {
	if err == nil {
		return nil
	}
	var genErr *GenerationError
	if errors.As(err, &genErr) {
		return err
	}
	return &GenerationError{
		Variant:     r.variant,
		ProjectSlug: strings.TrimSpace(req.ProjectSlug),
		ReportID:    strings.TrimSpace(req.ReportID),
		CommandPath: r.cliPath,
		Timeout:     r.timeout,
		Stderr:      strings.TrimSpace(stderr),
		Cause:       err,
	}
}
