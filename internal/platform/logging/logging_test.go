package logging

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestJSONLoggerIncludesComponentAndRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "warn", "json", "studio-test")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown", "k", "v")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line at warn level, got %d: %s", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(lines[0], &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["component"] != "studio-test" || rec["msg"] != "shown" || rec["k"] != "v" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestInvalidSettings(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "verbose", "json", "c"); err == nil {
		t.Error("expected error for unknown level")
	}
	if _, err := New(&bytes.Buffer{}, "info", "xml", "c"); err == nil {
		t.Error("expected error for unknown format")
	}
}
