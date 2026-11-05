package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestVersionPrintsJSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"version"}, lookup(nil), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var v map[string]string
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if v["go_version"] == "" || v["version"] == "" {
		t.Errorf("missing fields: %v", v)
	}
}

func TestConfigCheck(t *testing.T) {
	var out, errb bytes.Buffer
	//nolint:gosec // G101: synthetic credential (host h); the test asserts config check never prints this password.
	code := run(t.Context(), []string{"config", "check"},
		lookup(map[string]string{"STUDIO_DATABASE_URL": "postgres://u:pw123@h/db",
			"STUDIO_AUDIT_HMAC_KEY": "test-only-hmac-key-0123456789abcdef"}), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "pw123") {
		t.Error("config check printed a password")
	}

	out.Reset()
	errb.Reset()
	if code := run(t.Context(), []string{"config", "check"}, lookup(map[string]string{"STUDIO_LOG_LEVEL": "x"}), &out, &errb); code != 1 {
		t.Errorf("invalid config exit = %d", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"frobnicate"}, lookup(nil), &out, &errb); code != 2 {
		t.Errorf("exit = %d", code)
	}
	if code := run(t.Context(), nil, lookup(nil), &out, &errb); code != 2 {
		t.Errorf("no-args exit = %d", code)
	}
}
