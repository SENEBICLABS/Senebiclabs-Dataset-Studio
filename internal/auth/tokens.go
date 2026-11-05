package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Session tokens are opaque 256-bit values (plan 14), sent as Bearer tokens
// and stored only as SHA-256.
const sessionTokenBytes = 32

// API keys have the form dsk_<prefix>_<secret> (plan 14). The prefix is a
// public lookup handle; the whole key is stored only as SHA-256.
const (
	apiKeyScheme      = "dsk_"
	apiKeyPrefixLen   = 8 // base32 characters [a-z2-7]
	apiKeySecretBytes = 32
)

var (
	b64      = base64.RawURLEncoding
	prefixEn = base32.StdEncoding.WithPadding(base32.NoPadding)
)

// ErrMalformedAPIKey is returned for strings that are not dsk_ keys.
var ErrMalformedAPIKey = errors.New("auth: malformed API key")

// NewSessionToken returns a new token and its SHA-256 hash.
func NewSessionToken() (token string, hash []byte, err error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("auth: session token: %w", err)
	}
	token = b64.EncodeToString(b)
	return token, HashSecret(token), nil
}

// HashSecret is the stored form of a session token or API key.
func HashSecret(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// NewAPIKey returns a new key, its public prefix and its SHA-256 hash.
func NewAPIKey() (key, prefix string, hash []byte, err error) {
	p := make([]byte, 5) // 5 bytes -> 8 base32 characters
	s := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(p); err != nil {
		return "", "", nil, fmt.Errorf("auth: api key: %w", err)
	}
	if _, err := rand.Read(s); err != nil {
		return "", "", nil, fmt.Errorf("auth: api key: %w", err)
	}
	prefix = strings.ToLower(prefixEn.EncodeToString(p))
	key = apiKeyScheme + prefix + "_" + b64.EncodeToString(s)
	return key, prefix, HashSecret(key), nil
}

// IsAPIKey reports whether a bearer credential looks like an API key.
func IsAPIKey(s string) bool { return strings.HasPrefix(s, apiKeyScheme) }

// ParseAPIKeyPrefix extracts the lookup prefix from a dsk_ key.
func ParseAPIKeyPrefix(key string) (string, error) {
	rest, ok := strings.CutPrefix(key, apiKeyScheme)
	if !ok {
		return "", ErrMalformedAPIKey
	}
	prefix, secret, ok := strings.Cut(rest, "_")
	if !ok || len(prefix) != apiKeyPrefixLen || secret == "" {
		return "", ErrMalformedAPIKey
	}
	for _, c := range prefix {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return "", ErrMalformedAPIKey
		}
	}
	return prefix, nil
}

// IdentifierHMAC returns the keyed hash of a login identifier recorded for
// anonymous failed logins (plan 8.1). The identifier is normalised (trimmed,
// lower-cased) so variants of one address aggregate together; the raw
// identifier is never stored.
func IdentifierHMAC(key []byte, identifier string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.ToLower(strings.TrimSpace(identifier))))
	return hex.EncodeToString(m.Sum(nil))
}
