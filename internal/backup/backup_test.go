package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManifestRoundTripAndTamperDetection(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		FormatVersion: FormatVersion, CreatedAt: time.Unix(0, 0).UTC(), Database: "studio", GooseVersion: 6,
		InstallationID: "i", RowCounts: map[string]int64{"t": 1}, Blobs: []BlobEntry{},
		Dump: FileEntry{Name: DumpFile, SHA256: "x", Size: 1}, ReleaseManifestHashes: []string{},
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(dir)
	if err != nil || got.GooseVersion != 6 || got.RowCounts["t"] != 1 {
		t.Fatalf("ReadManifest = %+v, %v", got, err)
	}
	// COMPLETE is written only once: a second manifest cannot overwrite it.
	if err := writeManifest(dir, m); err == nil {
		t.Error("manifest overwritten")
	}
	p := filepath.Join(dir, ManifestFile)
	raw, _ := os.ReadFile(p)
	tampered := []byte(strings.Replace(string(raw), `"goose_version": 6`, `"goose_version": 7`, 1))
	if err := os.WriteFile(p, tampered, 0o600); err != nil { //nolint:gosec // G703: the test tampers with its own file in t.TempDir().
		t.Fatal(err)
	}
	if _, err := ReadManifest(dir); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Errorf("tampered manifest = %v", err)
	}
	if err := os.Remove(filepath.Join(dir, CompleteFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(dir); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("missing COMPLETE = %v", err)
	}
}

func TestConnFromURL(t *testing.T) {
	c, err := ConnFromURL("postgres://studio_backup:pw@127.0.0.1:55432/studio?sslmode=disable")
	if err != nil || c.User != "studio_backup" || c.Password != "pw" || c.Database != "studio" {
		t.Errorf("ConnFromURL = %+v, %v", c, err)
	}
	if _, err := ConnFromURL("postgres://127.0.0.1/"); err == nil {
		t.Error("URL without user and database accepted")
	}
}

func TestDockerToolsRejectBadContainerNames(t *testing.T) {
	for _, name := range []string{"", "-it", "--privileged", "a b", "x;rm"} {
		if _, err := (DockerTools{Container: name}).Version(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "container") {
			t.Errorf("container %q accepted: %v", name, err)
		}
	}
}

func TestPrepareEmptyDir(t *testing.T) {
	base := t.TempDir()
	if err := prepareEmptyDir(filepath.Join(base, "new")); err != nil {
		t.Errorf("new dir: %v", err)
	}
	if err := prepareEmptyDir("relative"); err == nil {
		t.Error("relative dir accepted")
	}
	full := filepath.Join(base, "full")
	_ = os.MkdirAll(full, 0o750)
	_ = os.WriteFile(filepath.Join(full, "x"), nil, 0o600)
	if err := prepareEmptyDir(full); err == nil {
		t.Error("non-empty dir accepted")
	}
}

func TestCompareCounts(t *testing.T) {
	if d := compareCounts(map[string]int64{"a": 1}, map[string]int64{"a": 1}); d != "" {
		t.Errorf("equal counts differ: %s", d)
	}
	d := compareCounts(map[string]int64{"a": 1, "b": 2}, map[string]int64{"a": 2, "c": 0})
	for _, want := range []string{"a 2≠1", "b missing", "c unexpected"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff %q lacks %q", d, want)
		}
	}
}
