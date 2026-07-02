package securetoken

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

func New(bytes int) (string, error) {
	if bytes < 16 {
		return "", fmt.Errorf("token entropy must be at least 16 bytes")
	}
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
