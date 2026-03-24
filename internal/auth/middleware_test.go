package auth_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	internalauth "github.com/testimony-dev/testimony/internal/auth"
	"github.com/testimony-dev/testimony/internal/db"
)

func TestMiddlewareRejectsMissingMalformedAndInvalidBearerTokens(t *testing.T) {
	ctx := context.Background()
	store := openAuthStore(t, ctx)
	defer store.Close()

	if _, err := store.EnsureAPIKey(ctx, db.APIKeyBootstrapName, "bootstrap-key-test"); err != nil {
		t.Fatalf("EnsureAPIKey() error = %v", err)
	}

	tests := []struct {
		name          string
		authorization string
		wantReason    string
	}{
		{name: "missing header", wantReason: "missing_authorization_header"},
		{name: "malformed header", authorization: "Basic abc123", wantReason: "malformed_authorization_header"},
		{name: "invalid key", authorization: "Bearer wrong-key", wantReason: "invalid_api_key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			router := newAuthRouter(t, internalauth.MiddlewareOptions{
				Enabled:   true,
				Validator: store,
				Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
			})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/backend-tests/upload", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			res := httptest.NewRecorder()

			router.ServeHTTP(res, req)

			if got, want := res.Code, http.StatusUnauthorized; got != want {
				t.Fatalf("status = %d, want %d", got, want)
			}
			if got, want := res.Header().Get("WWW-Authenticate"), "Bearer"; got != want {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
			}
			if !strings.Contains(logs.String(), tt.wantReason) {
				t.Fatalf("logs = %q, want reason %q", logs.String(), tt.wantReason)
			}
			if !strings.Contains(logs.String(), "backend-tests") {
				t.Fatalf("logs = %q, want project slug", logs.String())
			}
			if strings.Contains(logs.String(), "wrong-key") {
				t.Fatalf("logs leaked raw token: %q", logs.String())
			}
		})
	}
}

func TestMiddlewareAllowsValidBearerToken(t *testing.T) {
	ctx := context.Background()
	store := openAuthStore(t, ctx)
	defer store.Close()

	if _, err := store.EnsureAPIKey(ctx, db.APIKeyBootstrapName, "bootstrap-key-test"); err != nil {
		t.Fatalf("EnsureAPIKey() error = %v", err)
	}

	router := newAuthRouter(t, internalauth.MiddlewareOptions{
		Enabled:   true,
		Validator: store,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/backend-tests/upload", nil)
	req.Header.Set("Authorization", "Bearer bootstrap-key-test")
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if got, want := res.Code, http.StatusNoContent; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestMiddlewareBypassesValidationWhenDisabled(t *testing.T) {
	router := newAuthRouter(t, internalauth.MiddlewareOptions{Enabled: false})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/backend-tests/upload", nil)
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if got, want := res.Code, http.StatusNoContent; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestMiddlewareRequiresValidatorWhenEnabled(t *testing.T) {
	if _, err := internalauth.NewMiddleware(internalauth.MiddlewareOptions{Enabled: true}); err == nil {
		t.Fatal("NewMiddleware(enabled) error = nil, want validation error")
	}
}

func newAuthRouter(t *testing.T, opts internalauth.MiddlewareOptions) http.Handler {
	t.Helper()

	mw, err := internalauth.NewMiddleware(opts)
	if err != nil {
		t.Fatalf("NewMiddleware() error = %v", err)
	}

	r := chi.NewRouter()
	r.With(mw).Post("/api/v1/projects/{slug}/upload", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return r
}

func openAuthStore(t *testing.T, ctx context.Context) *db.SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "testimony.sqlite")
	store, err := db.OpenSQLiteStore(ctx, path, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("OpenSQLiteStore() error = %v", err)
	}
	return store
}
