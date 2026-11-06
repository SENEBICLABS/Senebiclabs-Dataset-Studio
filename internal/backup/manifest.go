// Package backup implements studioctl backup, restore and verify (plan 13).
//
// A backup directory holds:
//
//	database.dump   pg_dump -Fc of one exported snapshot
//	blobs/          the committed blobs listed in that snapshot, re-hashed
//	manifest.json   what the snapshot contained, with every file's hash
//	COMPLETE        written last: the SHA-256 of manifest.json
//
// A directory without a valid COMPLETE is not a backup, and restore refuses
// it. Every hash is checked before restore touches anything.
package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File names inside a backup directory.
const (
	DumpFile     = "database.dump"
	BlobsDir     = "blobs"
	ManifestFile = "manifest.json"
	CompleteFile = "COMPLETE"
)

// FormatVersion is the manifest format this code writes and reads.
const FormatVersion = 1

// Manifest records what one backup snapshot contained (plan 13, step 2).
type Manifest struct {
	FormatVersion      int              `json:"format_version"`
	CreatedAt          time.Time        `json:"created_at"`
	Database           string           `json:"database"`
	GooseVersion       int64            `json:"goose_version"`
	InstallationID     string           `json:"installation_id"`
	NewestAuditEventID string           `json:"newest_audit_event_id,omitempty"`
	RowCounts          map[string]int64 `json:"row_counts"`
	Blobs              []BlobEntry      `json:"blobs"`
	Dump               FileEntry        `json:"dump"`
	PgDumpVersion      string           `json:"pg_dump_version"`
	// RegistryDigest and ReleaseManifestHashes are part of the plan's
	// manifest (13, step 2). Their tables arrive in M8 and M13; until then
	// the digest is null and the list empty, and verify-restore has nothing
	// to compare for them.
	RegistryDigest        *string  `json:"registry_digest"`
	ReleaseManifestHashes []string `json:"release_manifest_hashes"`
}

// BlobEntry is one committed blob in the snapshot.
type BlobEntry struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// FileEntry is one file and its hash.
type FileEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// writeManifest writes manifest.json and then COMPLETE, each fsynced, and
// the directory fsynced after each, so COMPLETE can exist only after a
// durable manifest (plan 13, step 5).
func writeManifest(dir string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encode manifest: %w", err)
	}
	b = append(b, '\n')
	if err := writeDurable(filepath.Join(dir, ManifestFile), b); err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	return writeDurable(filepath.Join(dir, CompleteFile), []byte(hex.EncodeToString(sum[:])+"\n"))
}

// ReadManifest checks COMPLETE against manifest.json and decodes it. It
// fails for an incomplete or tampered backup.
func ReadManifest(dir string) (Manifest, error) {
	complete, err := os.ReadFile(filepath.Join(dir, CompleteFile)) //nolint:gosec // G304: dir is the operator-supplied backup directory.
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, errors.New("backup: no COMPLETE marker: the backup is incomplete")
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: read COMPLETE: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // G304: as above.
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: read manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	if strings.TrimSpace(string(complete)) != hex.EncodeToString(sum[:]) {
		return Manifest{}, errors.New("backup: manifest does not match COMPLETE: the backup was altered")
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("backup: decode manifest: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return Manifest{}, fmt.Errorf("backup: manifest format %d is not supported (want %d)", m.FormatVersion, FormatVersion)
	}
	if m.Dump.Name != DumpFile {
		return Manifest{}, fmt.Errorf("backup: unexpected dump file name %q", m.Dump.Name)
	}
	return m, nil
}

func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path is inside the new backup directory.
	if err != nil {
		return fmt.Errorf("backup: create %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: fsync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: a directory inside the backup or blob root.
	if err != nil {
		return fmt.Errorf("backup: open directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("backup: fsync directory: %w", err)
	}
	return nil
}

// fileSHA256 hashes a file.
func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file inside the backup directory.
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := copyN(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
