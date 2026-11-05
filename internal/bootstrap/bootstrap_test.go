package bootstrap

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// RFC 7677 section 3 worked example: password "pencil", salt and iteration
// count as below. Recomputing the published ClientProof and ServerSignature
// from our StoredKey/ServerKey proves the verifier derivation is correct.
func TestSCRAMVerifierMatchesRFC7677(t *testing.T) {
	salt, err := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	if err != nil {
		t.Fatal(err)
	}
	v, err := scramVerifierWithSalt("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^SCRAM-SHA-256\$4096:([^$]+)\$([^:]+):(.+)$`).FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("verifier has wrong shape: %s", v)
	}
	if m[1] != "W22ZaJ0SNY7soEsUEjb6gQ==" {
		t.Fatalf("salt not encoded as given: %s", m[1])
	}
	storedKey, _ := base64.StdEncoding.DecodeString(m[2])
	serverKey, _ := base64.StdEncoding.DecodeString(m[3])

	authMessage := strings.Join([]string{
		"n=user,r=rOprNGfwEbeRWgbNEkqO",
		"r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",
		"c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0",
	}, ",")

	serverSignature := hmacSHA256(serverKey, authMessage)
	if got := base64.StdEncoding.EncodeToString(serverSignature); got != "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Errorf("ServerSignature = %s", got)
	}

	// ClientProof = ClientKey XOR HMAC(StoredKey, AuthMessage); recover
	// ClientKey from the published proof and check H(ClientKey) == StoredKey.
	proof, _ := base64.StdEncoding.DecodeString("dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=")
	clientSignature := hmacSHA256(storedKey, authMessage)
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ clientSignature[i]
	}
	sum := sha256.Sum256(clientKey)
	if !hmac.Equal(sum[:], storedKey) {
		t.Error("StoredKey does not match the RFC 7677 client proof")
	}
}

func TestSCRAMVerifierUsesFreshSalt(t *testing.T) {
	a, err := scramVerifier("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := scramVerifier("correct-horse-battery")
	if a == b {
		t.Error("two verifiers for the same password must use different salts")
	}
	if strings.Contains(a, "correct-horse-battery") {
		t.Error("verifier contains the plaintext password")
	}
}

func TestPasswordsValidate(t *testing.T) {
	good := Passwords{
		Migrator: "migrator-pass-1", App: "app-password-1", Worker: "worker-pass-01",
		Backup: "backup-pass-001",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid passwords rejected: %v", err)
	}

	bad := good
	bad.App = "short"
	bad.Worker = "has a space 123"
	bad.Backup = "nön-ascii-pass"
	err := bad.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"studio_app", "studio_worker", "studio_backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	for _, secret := range []string{"short", "has a space", "nön"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks a password: %v", err)
		}
	}

	missing := good
	missing.Migrator = ""
	if err := missing.Validate(); err == nil {
		t.Error("an empty migrator password must be rejected")
	}
}
