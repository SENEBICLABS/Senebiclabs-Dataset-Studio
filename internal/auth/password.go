// Package auth implements authentication (plan 14, M3): local accounts with
// argon2id passwords, opaque session tokens and API keys stored only as
// hashes, rate-limited login with uniform errors, and audit events for every
// authentication change.
//
// Authorization (who may do what) is M4 and is not decided here.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: the OWASP minimum recommendation (m=19 MiB, t=2, p=1).
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16

	// Bounds accepted when verifying a stored hash, so a tampered or legacy
	// hash cannot force an expensive computation.
	maxArgonMemoryKiB = 256 * 1024
	maxArgonTime      = 10
	maxArgonThreads   = 8
)

// Password length limits. The maximum bounds hashing cost per request.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 1024
)

var errMalformedHash = errors.New("auth: malformed password hash")

// ValidatePassword checks a new password against the length policy.
func ValidatePassword(pw string) error {
	if len(pw) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if len(pw) > MaxPasswordLength {
		return fmt.Errorf("password must be at most %d characters", MaxPasswordLength)
	}
	return nil
}

// HashPassword returns a PHC-format argon2id hash with a fresh salt.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads, b64(salt), b64(key)), nil
}

// VerifyPassword reports whether pw matches the PHC-format hash, comparing in
// constant time.
func VerifyPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errMalformedHash
	}
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false, errMalformedHash
	}
	if mem == 0 || mem > maxArgonMemoryKiB || t == 0 || t > maxArgonTime || p == 0 || p > maxArgonThreads {
		return false, errMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, errMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errMalformedHash
	}
	got := argon2.IDKey([]byte(pw), salt, t, mem, p, uint32(len(want))) //nolint:gosec // G115: len(want) is bounded to 16..64 above.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when the login identifier is unknown, so an
// unknown account costs the same time as a wrong password (no user
// enumeration by timing).
var dummyHash = func() string {
	h, err := HashPassword("dummy-password-for-timing-only")
	if err != nil {
		panic(err)
	}
	return h
}()
