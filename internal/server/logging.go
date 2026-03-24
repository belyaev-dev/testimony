package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

func NewLogger(level string, output io.Writer) (*slog.Logger, error) {
	parsedLevel, err := parseLogLevel(level)
	if err != nil {
		return nil, err
	}
	if output == nil {
		output = io.Discard
	}

	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: parsedLevel})
	return slog.New(handler), nil
}

func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startedAt := time.Now()
			wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(wrapped, r)

			duration := time.Since(startedAt)
			logger.Info("http request completed",
				"request_id", middleware.GetReqID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", wrapped.Status(),
				"bytes", wrapped.BytesWritten(),
				"duration", duration.String(),
				"duration_ms", duration.Milliseconds(),
			)
		})
	}
}

func parseLogLevel(level string) (slog.Level, error) {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		return 0, err
	}
	return parsed, nil
}
