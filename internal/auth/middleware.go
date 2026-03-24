package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

var (
	errMissingAuthorizationHeader = errors.New("missing authorization header")
	errMalformedAuthorization     = errors.New("malformed authorization header")
)

type APIKeyValidator interface {
	ValidateAPIKey(ctx context.Context, rawKey string) error
}

type MiddlewareOptions struct {
	Enabled   bool
	Validator APIKeyValidator
	Logger    *slog.Logger
}

func NewMiddleware(opts MiddlewareOptions) (func(http.Handler) http.Handler, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	if !opts.Enabled {
		return func(next http.Handler) http.Handler { return next }, nil
	}
	if opts.Validator == nil {
		return nil, errors.New("new auth middleware: nil validator")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := parseBearerToken(r.Header.Get("Authorization"))
			if err != nil {
				logRejection(logger, r, rejectionReason(err), nil)
				writeUnauthorized(w)
				return
			}

			if err := opts.Validator.ValidateAPIKey(r.Context(), token); err != nil {
				reason := "invalid_api_key"
				if !errors.Is(err, ErrInvalidAPIKey) {
					reason = "api_key_validation_failed"
				}
				logRejection(logger, r, reason, err)
				writeUnauthorized(w)
				return
			}

			next.ServeHTTP(w, r)
		})
	}, nil
}

func parseBearerToken(header string) (string, error) {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return "", errMissingAuthorizationHeader
	}

	parts := strings.Fields(trimmed)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", errMalformedAuthorization
	}

	return parts[1], nil
}

func rejectionReason(err error) string {
	switch {
	case errors.Is(err, errMissingAuthorizationHeader):
		return "missing_authorization_header"
	default:
		return "malformed_authorization_header"
	}
}

func logRejection(logger *slog.Logger, r *http.Request, reason string, err error) {
	attrs := []any{
		"reason", reason,
		"method", r.Method,
		"path", r.URL.Path,
	}
	if projectSlug := strings.TrimSpace(chi.URLParam(r, "slug")); projectSlug != "" {
		attrs = append(attrs, "project_slug", projectSlug)
	}
	if err != nil && !errors.Is(err, ErrInvalidAPIKey) {
		attrs = append(attrs, "error", err)
	}
	logger.Warn("auth rejected", attrs...)
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}
