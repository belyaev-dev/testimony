package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/testimony-dev/testimony/internal/auth"
)

func TestKeygenGenerateAPIKey(t *testing.T) {
	first, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() first error = %v", err)
	}
	second, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() second error = %v", err)
	}

	if len(first) != 64 {
		t.Fatalf("len(first) = %d, want 64", len(first))
	}
	if len(second) != 64 {
		t.Fatalf("len(second) = %d, want 64", len(second))
	}
	if first == second {
		t.Fatal("GenerateAPIKey() produced duplicate keys")
	}
}

func TestKeygenHashAPIKeyDeterministic(t *testing.T) {
	got, err := auth.HashAPIKey("deterministic-api-key")
	if err != nil {
		t.Fatalf("HashAPIKey() error = %v", err)
	}

	const want = "0783c4e1e95b15fbd0f82e520883534d9e08bec4870e355781ab5379ce7a47ec"
	if got != want {
		t.Fatalf("HashAPIKey() = %q, want %q", got, want)
	}
}

func TestKeygenHashAPIKeyRejectsEmptyInput(t *testing.T) {
	_, err := auth.HashAPIKey("   ")
	if !errors.Is(err, auth.ErrInvalidAPIKey) {
		t.Fatalf("HashAPIKey(empty) error = %v, want ErrInvalidAPIKey", err)
	}
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("HashAPIKey(empty) error = %v, want invalid api key message", err)
	}
}
