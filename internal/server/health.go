package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
)

type ReadinessCheck struct {
	Name  string
	Check func(context.Context) error
}

type Health struct {
	logger *slog.Logger
	ready  atomic.Bool
	reason atomic.Value
	checks []ReadinessCheck
}

type healthResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func NewHealth(logger *slog.Logger, checks ...ReadinessCheck) *Health {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	h := &Health{
		logger: logger,
		checks: append([]ReadinessCheck(nil), checks...),
	}
	h.ready.Store(false)
	h.reason.Store("starting")
	return h
}

func (h *Health) Ready() bool {
	return h.ready.Load()
}

func (h *Health) Reason() string {
	reason, _ := h.reason.Load().(string)
	return reason
}

func (h *Health) SetReady(ready bool, reason string) {
	if reason == "" {
		if ready {
			reason = "ready"
		} else {
			reason = "not_ready"
		}
	}

	previousReady := h.ready.Load()
	previousReason := h.Reason()
	h.ready.Store(ready)
	h.reason.Store(reason)

	if previousReady == ready && previousReason == reason {
		return
	}

	h.logger.Info("readiness changed",
		"ready", ready,
		"reason", reason,
	)
}

func (h *Health) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (h *Health) Readyz(w http.ResponseWriter, r *http.Request) {
	ready, reason := h.readinessState(r.Context())
	if ready {
		writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
		return
	}

	writeJSON(w, http.StatusServiceUnavailable, healthResponse{
		Status: "not_ready",
		Reason: reason,
	})
}

func (h *Health) readinessState(ctx context.Context) (bool, string) {
	if !h.Ready() {
		return false, h.Reason()
	}

	for _, check := range h.checks {
		if check.Check == nil {
			continue
		}
		if err := check.Check(ctx); err != nil {
			reason := readinessFailureReason(check.Name)
			h.logger.Error("readiness probe failed",
				"dependency", normalizeReadinessName(check.Name),
				"reason", reason,
				"error", err,
			)
			return false, reason
		}
	}

	return true, "ready"
}

func readinessFailureReason(name string) string {
	normalized := normalizeReadinessName(name)
	if normalized == "" {
		normalized = "dependency"
	}
	return fmt.Sprintf("%s_unavailable", normalized)
}

func normalizeReadinessName(name string) string {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	if trimmed == "" {
		return ""
	}

	replacer := strings.NewReplacer(" ", "_", "-", "_", "/", "_")
	return replacer.Replace(trimmed)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
