package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/testimony-dev/testimony/internal/server"
)

func TestHealthAndReadinessEndpoints(t *testing.T) {
	logger, buf := newBufferedLogger(t)
	health := server.NewHealth(logger)
	router := server.NewRouter(server.Options{Logger: logger, Health: health})

	t.Run("healthz is always live", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		res := httptest.NewRecorder()

		router.ServeHTTP(res, req)

		if got, want := res.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}

		var body map[string]string
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		if got, want := body["status"], "ok"; got != want {
			t.Fatalf("status body = %q, want %q", got, want)
		}
	})

	t.Run("readyz reflects state transitions", func(t *testing.T) {
		health.SetReady(false, "waiting for dependencies")

		notReadyReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		notReadyRes := httptest.NewRecorder()
		router.ServeHTTP(notReadyRes, notReadyReq)

		if got, want := notReadyRes.Code, http.StatusServiceUnavailable; got != want {
			t.Fatalf("not ready status = %d, want %d", got, want)
		}

		var notReadyBody map[string]string
		if err := json.Unmarshal(notReadyRes.Body.Bytes(), &notReadyBody); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		if got, want := notReadyBody["reason"], "waiting for dependencies"; got != want {
			t.Fatalf("reason = %q, want %q", got, want)
		}

		health.SetReady(true, "dependencies healthy")
		readyReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		readyRes := httptest.NewRecorder()
		router.ServeHTTP(readyRes, readyReq)

		if got, want := readyRes.Code, http.StatusOK; got != want {
			t.Fatalf("ready status = %d, want %d", got, want)
		}

		var readyBody map[string]string
		if err := json.Unmarshal(readyRes.Body.Bytes(), &readyBody); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		if got, want := readyBody["status"], "ready"; got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
	})

	if !strings.Contains(buf.String(), "readiness changed") {
		t.Fatalf("expected readiness transition log, got %q", buf.String())
	}
}

func TestRequestLoggerEmitsStructuredFields(t *testing.T) {
	logger, buf := newBufferedLogger(t)
	health := server.NewHealth(logger)
	health.SetReady(true, "ready for logging")
	router := server.NewRouter(server.Options{Logger: logger, Health: health})
	buf.Reset()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("expected at least one log line")
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	for _, field := range []string{"request_id", "method", "path", "status", "duration", "duration_ms"} {
		if _, ok := entry[field]; !ok {
			t.Fatalf("expected log field %q in %#v", field, entry)
		}
	}
	if got, want := entry["method"], "GET"; got != want {
		t.Fatalf("method = %v, want %v", got, want)
	}
	if got, want := entry["path"], "/healthz"; got != want {
		t.Fatalf("path = %v, want %v", got, want)
	}
	if got, want := entry["status"], float64(http.StatusOK); got != want {
		t.Fatalf("status = %v, want %v", got, want)
	}
	if got, ok := entry["request_id"].(string); !ok || got == "" {
		t.Fatalf("request_id = %v, want non-empty string", entry["request_id"])
	}
}

func TestReadyzDependsOnRuntimeChecks(t *testing.T) {
	logger, _ := newBufferedLogger(t)
	dependencyHealthy := false
	health := server.NewHealth(logger, server.ReadinessCheck{
		Name: "sqlite",
		Check: func(_ context.Context) error {
			if dependencyHealthy {
				return nil
			}
			return errors.New("database unavailable")
		},
	})
	health.SetReady(true, "ready")
	router := server.NewRouter(server.Options{Logger: logger, Health: health})

	notReadyReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	notReadyRes := httptest.NewRecorder()
	router.ServeHTTP(notReadyRes, notReadyReq)

	if got, want := notReadyRes.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("not ready status = %d, want %d", got, want)
	}

	var notReadyBody map[string]string
	if err := json.Unmarshal(notReadyRes.Body.Bytes(), &notReadyBody); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got, want := notReadyBody["reason"], "sqlite_unavailable"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}

	dependencyHealthy = true
	readyReq := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	readyRes := httptest.NewRecorder()
	router.ServeHTTP(readyRes, readyReq)

	if got, want := readyRes.Code, http.StatusOK; got != want {
		t.Fatalf("ready status = %d, want %d", got, want)
	}
}

func newBufferedLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger, err := server.NewLogger("debug", buf)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	return logger, buf
}
