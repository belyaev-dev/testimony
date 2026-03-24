package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	internalauth "github.com/testimony-dev/testimony/internal/auth"
	"github.com/testimony-dev/testimony/internal/config"
	"github.com/testimony-dev/testimony/internal/db"
	"github.com/testimony-dev/testimony/internal/generate"
	"github.com/testimony-dev/testimony/internal/retention"
	servepkg "github.com/testimony-dev/testimony/internal/serve"
	"github.com/testimony-dev/testimony/internal/server"
	"github.com/testimony-dev/testimony/internal/storage"
	"github.com/testimony-dev/testimony/internal/upload"
)

type runOptions struct {
	stdout         io.Writer
	lookup         config.LookupFunc
	listener       net.Listener
	registerRoutes func(chi.Router)
}

type generationDispatcherAdapter struct {
	service *generate.Service
}

func (a generationDispatcherAdapter) Enqueue(req upload.GenerationRequest) error {
	if a.service == nil {
		return fmt.Errorf("generation dispatcher adapter: nil service")
	}
	return a.service.Enqueue(generate.Job{
		ProjectSlug: req.ProjectSlug,
		ReportID:    req.ReportID,
		ResultsDir:  req.ResultsDir,
	})
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Stdout, os.LookupEnv); err != nil {
		fmt.Fprintf(os.Stderr, "testimony: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, stdout io.Writer, lookup config.LookupFunc) error {
	return runWithOptions(ctx, runOptions{stdout: stdout, lookup: lookup})
}

func runWithOptions(ctx context.Context, opts runOptions) error {
	cfg, err := config.LoadFromLookup(opts.lookup)
	if err != nil {
		return err
	}

	logger, err := server.NewLogger(cfg.LogLevel, opts.stdout)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}

	if err := os.MkdirAll(cfg.Runtime.TempDir, 0o755); err != nil {
		return fmt.Errorf("create temp dir %q: %w", cfg.Runtime.TempDir, err)
	}

	sqliteStore, err := db.OpenSQLiteStore(ctx, cfg.Database.Path, cfg.Database.BusyTimeout, logger)
	if err != nil {
		logger.Error("sqlite startup failed",
			"path", cfg.Database.Path,
			"error", err,
		)
		return fmt.Errorf("open sqlite store: %w", err)
	}
	defer sqliteStore.Close()

	if cfg.Auth.Enabled {
		if _, err := sqliteStore.EnsureAPIKey(ctx, db.APIKeyBootstrapName, cfg.Auth.APIKey); err != nil {
			logger.Error("bootstrap api key startup failed",
				"name", db.APIKeyBootstrapName,
				"error", err,
			)
			return fmt.Errorf("seed bootstrap api key: %w", err)
		}
		logger.Info("bootstrap api key ready",
			"name", db.APIKeyBootstrapName,
		)
	}

	authMiddleware, err := internalauth.NewMiddleware(internalauth.MiddlewareOptions{
		Enabled:   cfg.Auth.Enabled,
		Validator: sqliteStore,
		Logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("build auth middleware: %w", err)
	}

	s3Store, err := storage.NewS3Store(ctx, cfg.Storage, logger)
	if err != nil {
		logger.Error("s3 startup failed",
			"bucket", cfg.Storage.Bucket,
			"endpoint", cfg.Storage.Endpoint,
			"error", err,
		)
		return fmt.Errorf("open s3 store: %w", err)
	}

	generator, err := generate.New(cfg.Generate, s3Store, nil)
	if err != nil {
		return fmt.Errorf("build generator: %w", err)
	}

	generationService, err := generate.NewService(generate.ServiceOptions{
		Logger:         logger,
		Store:          sqliteStore,
		Storage:        s3Store,
		Generator:      generator,
		TempDir:        cfg.Runtime.TempDir,
		MaxConcurrency: cfg.Generate.MaxConcurrency,
	})
	if err != nil {
		return fmt.Errorf("build generation service: %w", err)
	}

	uploadHandler, err := upload.NewHandler(upload.HandlerOptions{
		Logger:     logger,
		Store:      sqliteStore,
		Storage:    s3Store,
		Dispatcher: generationDispatcherAdapter{service: generationService},
		TempDir:    cfg.Runtime.TempDir,
	})
	if err != nil {
		return fmt.Errorf("build upload handler: %w", err)
	}

	browseUI, err := servepkg.NewUI(logger, sqliteStore)
	if err != nil {
		return fmt.Errorf("build browse ui: %w", err)
	}

	reportProxy, err := servepkg.NewReportProxy(logger, sqliteStore, s3Store)
	if err != nil {
		return fmt.Errorf("build report proxy: %w", err)
	}

	retentionWorker, err := retention.NewWorker(retention.WorkerOptions{
		Logger:              logger,
		Store:               sqliteStore,
		Storage:             s3Store,
		GlobalRetentionDays: cfg.Retention.Days,
		Interval:            cfg.Retention.CleanupInterval,
	})
	if err != nil {
		return fmt.Errorf("build retention worker: %w", err)
	}
	if err := retentionWorker.Start(ctx); err != nil {
		return fmt.Errorf("start retention worker: %w", err)
	}

	retentionWorkerStopped := false
	stopRetentionWorker := func(stopCtx context.Context) error {
		if retentionWorkerStopped {
			return nil
		}
		retentionWorkerStopped = true
		return retentionWorker.Stop(stopCtx)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
		defer cancel()
		if err := stopRetentionWorker(stopCtx); err != nil {
			logger.Warn("retention worker shutdown failed", "error", err)
		}
	}()

	registerViewerRoutes := func(r chi.Router) {
		browseUI.RegisterRoutes(r)
		reportProxy.RegisterRoutes(r)
	}

	registerRoutes := func(r chi.Router) {
		if cfg.Auth.RequireViewer {
			r.Group(func(protected chi.Router) {
				protected.Use(authMiddleware)
				registerViewerRoutes(protected)
			})
		} else {
			registerViewerRoutes(r)
		}
		if opts.registerRoutes != nil {
			opts.registerRoutes(r)
		}
	}

	protectedUploadHandler := authMiddleware(uploadHandler)

	health := server.NewHealth(logger,
		server.ReadinessCheck{Name: "sqlite", Check: sqliteStore.Ready},
		server.ReadinessCheck{Name: "s3", Check: s3Store.Ready},
	)
	router := server.NewRouter(server.Options{
		Logger:         logger,
		Health:         health,
		UploadHandler:  protectedUploadHandler,
		RegisterRoutes: registerRoutes,
	})
	httpServer := server.NewHTTPServer(cfg.Server, logger, router)

	listener := opts.listener
	if listener == nil {
		listener, err = net.Listen("tcp", cfg.Server.Address())
		if err != nil {
			return fmt.Errorf("listen on %s: %w", cfg.Server.Address(), err)
		}
	}
	defer listener.Close()

	health.SetReady(true, "ready")
	logger.Info("http server listening",
		"addr", listener.Addr().String(),
		"temp_dir", cfg.Runtime.TempDir,
		"sqlite_path", cfg.Database.Path,
		"s3_bucket", cfg.Storage.Bucket,
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	select {
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown requested")
		health.SetReady(false, "draining")
		logger.Info("shutdown drain started",
			"readiness_drain_delay", cfg.Shutdown.ReadyzDrainDelay.String(),
			"shutdown_timeout", cfg.Shutdown.Timeout.String(),
		)

		if err := waitForDrainWindow(cfg.Shutdown.ReadyzDrainDelay, errCh); err != nil {
			return err
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown http server: %w", err)
		}
		if err := stopRetentionWorker(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown retention worker: %w", err)
		}

		err := <-errCh
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			logger.Info("shutdown complete")
			return nil
		}

		return fmt.Errorf("serve http after shutdown: %w", err)
	}
}

func waitForDrainWindow(delay time.Duration, errCh <-chan error) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server stopped during drain delay: %w", err)
	}
}
