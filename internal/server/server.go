package server

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/testimony-dev/testimony/internal/config"
)

type Options struct {
	Logger         *slog.Logger
	Health         *Health
	UploadHandler  http.Handler
	RegisterRoutes func(r chi.Router)
}

func NewRouter(opts Options) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	health := opts.Health
	if health == nil {
		health = NewHealth(logger)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(RequestLogger(logger))

	r.Get("/healthz", health.Healthz)
	r.Get("/readyz", health.Readyz)

	if opts.UploadHandler != nil {
		r.Route("/api/v1", func(api chi.Router) {
			api.Post("/projects/{slug}/upload", opts.UploadHandler.ServeHTTP)
		})
	}

	if opts.RegisterRoutes != nil {
		opts.RegisterRoutes(r)
	}

	return r
}

func NewHTTPServer(cfg config.ServerConfig, logger *slog.Logger, handler http.Handler) *http.Server {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	errorLogger := logger.With("component", "http_server")

	return &http.Server{
		Addr:         cfg.Address(),
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		ErrorLog:     slog.NewLogLogger(errorLogger.Handler(), slog.LevelError),
	}
}
