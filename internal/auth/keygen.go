package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidAPIKey = errors.New("invalid api key")

const generatedAPIKeyBytes = 32

func GenerateAPIKey() (string, error) {
	buf := make([]byte, generatedAPIKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func HashAPIKey(rawKey string) (string, error) {
	normalized := strings.TrimSpace(rawKey)
	if normalized == "" {
		return "", fmt.Errorf("hash api key: %w", ErrInvalidAPIKey)
	}

	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:]), nil
}
