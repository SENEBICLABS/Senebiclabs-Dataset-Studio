package auth

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTripAndFormat(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`).MatchString(h) {
		t.Fatalf("unexpected PHC format: %s", h)
	}
	if strings.Contains(h, "correct horse") {
		t.Fatal("hash contains the password")
	}
	if ok, err := VerifyPassword("correct horse battery", h); err != nil || !ok {
		t.Errorf("correct password rejected: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword("correct horse batterY", h); ok {
		t.Error("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Error("two hashes of one password must differ (fresh salt)")
	}
}

func TestVerifyRejectsMalformedAndAbusiveHashes(t *testing.T) {
	for _, bad := range []string{
		"", "plaintext", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=999999999,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA", // memory bomb
		"$argon2id$v=19$m=19456,t=1000,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",  // time bomb
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if ok, err := VerifyPassword("x", bad); ok || err == nil {
			t.Errorf("accepted malformed hash %q", bad)
		}
	}
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLength-1)); err == nil {
		t.Error("short password accepted")
	}
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLength+1)); err == nil {
		t.Error("over-long password accepted")
	}
}

func TestSessionTokenFormatAndHash(t *testing.T) {
	tok, hash, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 || len(hash) != 32 {
		t.Errorf("token len %d (want 43, 256 bits), hash len %d", len(tok), len(hash))
	}
	if hex.EncodeToString(hash) != hex.EncodeToString(HashSecret(tok)) {
		t.Error("returned hash is not SHA-256 of the token")
	}
	tok2, _, _ := NewSessionToken()
	if tok == tok2 {
		t.Error("tokens repeat")
	}
}

func TestAPIKeyFormatAndParsing(t *testing.T) {
	key, prefix, hash, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^dsk_[a-z2-7]{8}_[A-Za-z0-9_-]{43}$`).MatchString(key) {
		t.Fatalf("key format: %s", key)
	}
	if got, err := ParseAPIKeyPrefix(key); err != nil || got != prefix {
		t.Errorf("prefix %q (%v), want %q", got, err, prefix)
	}
	if !IsAPIKey(key) || len(hash) != 32 {
		t.Error("key not recognised or hash wrong size")
	}
	for _, bad := range []string{"dsk_", "dsk_short_x", "dsk_ABCDEFGH_x", "dsk_abcdefgh_", "xyz_abcdefgh_s", "dsk_abcdefg1_s"} {
		if _, err := ParseAPIKeyPrefix(bad); err == nil {
			t.Errorf("parsed malformed key %q", bad)
		}
	}
}

func TestIdentifierHMACNormalisesAndIsKeyed(t *testing.T) {
	k1, k2 := []byte("key-one-0123456789abcdef0123456789"), []byte("key-two-0123456789abcdef0123456789")
	a := IdentifierHMAC(k1, "  Alice@Example.TEST ")
	if a != IdentifierHMAC(k1, "alice@example.test") {
		t.Error("variants of one address must give one HMAC")
	}
	if a == IdentifierHMAC(k2, "alice@example.test") {
		t.Error("HMAC must depend on the key")
	}
	if strings.Contains(a, "alice") || len(a) != 64 {
		t.Errorf("unexpected HMAC %s", a)
	}
}

func TestKeyedLimiterLimitsPerKeyAndRefills(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newKeyedLimiter(5, func() time.Time { return now })
	for i := range 5 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("attempt %d refused within burst", i+1)
		}
	}
	ok, wait := l.allow("a")
	if ok || wait <= 0 || wait > 12*time.Second {
		t.Fatalf("6th attempt: ok=%v wait=%s", ok, wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Error("another key must have its own bucket")
	}
	now = now.Add(12 * time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Error("bucket did not refill after one interval")
	}
}
