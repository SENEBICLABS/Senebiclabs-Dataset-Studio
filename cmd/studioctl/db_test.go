package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDBSubcommandUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"db"}, {"db", "drop"}, {"db", "bootstrap", "extra"},
		{"migrate", "down"}, {"migrate", "up", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := run(t.Context(), args, lookup(nil), &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestDBBootstrapRequiresSuperuserURL(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"db", "bootstrap"}, lookup(nil), &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), envBootstrapURL) {
		t.Errorf("error does not name %s: %s", envBootstrapURL, errb.String())
	}
}

func TestDBBootstrapRejectsWeakPasswordsWithoutEchoingThem(t *testing.T) {
	var out, errb bytes.Buffer
	//nolint:gosec // G101: synthetic credentials (host h, no server); the test asserts they are never printed.
	env := map[string]string{
		envBootstrapURL:     "postgres://admin:adminSecretX@h/postgres",
		envMigratorPassword: "tiny",
		envAppPassword:      "app-password-ok",
		envWorkerPassword:   "worker password",
		envBackupPassword:   "backup-password-ok",
	}
	if code := run(t.Context(), []string{"db", "bootstrap"}, lookup(env), &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	msg := errb.String()
	for _, secret := range []string{"tiny", "worker password", "adminSecretX", "app-password-ok"} {
		if strings.Contains(msg, secret) {
			t.Errorf("output leaks %q: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "studio_migrator") || !strings.Contains(msg, "studio_worker") {
		t.Errorf("output does not name the failing roles: %s", msg)
	}
}

func TestMigrateRequiresMigratorURL(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, lookup(nil), &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), envMigratorURL) {
		t.Errorf("error does not name %s: %s", envMigratorURL, errb.String())
	}
	errb.Reset()
	if code := run(t.Context(), []string{"migrate", "status"},
		lookup(map[string]string{envMigratorURL: "mysql://h/db"}), &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}
