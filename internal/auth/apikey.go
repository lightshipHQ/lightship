package auth

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

// KeyPrefix marks a LightShip API key in logs, environment dumps and repository scanners. Secret
// scanning works on recognisable shapes, so the prefix is part of the credential rather than
// decoration.
const KeyPrefix = "lsk_"

// NewKey returns a bearer token. It is shown to the caller once, at creation; only its sha256 is
// stored, so a leaked database yields no usable credential and a lost key is replaced rather than
// recovered.
func NewKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// LooksLikeKey reports whether a bearer credential is shaped like an API key. It decides which
// lookup a request gets, not whether the credential is valid.
func LooksLikeKey(token string) bool { return strings.HasPrefix(token, KeyPrefix) }
