package config

import (
	"crypto/rand"
	"encoding/base64"
)

const apiKeyRandomBytes = 32

// GenerateAPIKey returns a URL-safe API key with 256 bits of randomness.
func GenerateAPIKey() (string, error) {
	var random [apiKeyRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "sk-local-" + base64.RawURLEncoding.EncodeToString(random[:]), nil
}
