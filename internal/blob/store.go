// Package blob is the content-addressed blob store (plan 5.3 and 11).
//
// An upload is two transactions (plan 11): transaction 1 records the upload
// in blob_uploads and commits; the bytes are streamed to staging, hashed and
// fsynced; transaction 2, which is the caller's command transaction, takes
// the per-sha advisory lock and either deduplicates against an existing
// committed blob or renames staging into its final path and inserts the
// blob_objects row. Committed blobs are immutable and never deleted in
// Phase 1. blob.sweep and blob.audit (jobs.go) clean up after crashes and
// detect missing or corrupt blobs.
package blob

import (
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// Sum is a SHA-256 content hash.
type Sum [32]byte

// String is the lowercase hex form.
func (s Sum) String() string { return hex.EncodeToString(s[:]) }

// ParseSum parses a lowercase or uppercase hex SHA-256.
func ParseSum(h string) (Sum, error) {
	var s Sum
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != len(s) {
		return s, errors.New("blob: not a hex SHA-256")
	}
	copy(s[:], b)
	return s, nil
}

// SumFromBytes converts a 32-byte database value.
func SumFromBytes(b []byte) (Sum, error) {
	var s Sum
	if len(b) != len(s) {
		return s, errors.New("blob: sha256 must be 32 bytes")
	}
	copy(s[:], b)
	return s, nil
}

// Health of a committed blob's file.
type Health string

// Health values; Missing and Corrupt are the blob_health_events kinds.
const (
	Healthy Health = "healthy"
	Missing Health = "missing"
	Corrupt Health = "corrupt"
)

// Errors returned by stores and the service.
var (
	ErrExists   = errors.New("blob: already exists")
	ErrNotFound = errors.New("blob: not found")
	ErrBadKey   = errors.New("blob: invalid staging key")
)

// Entry is one file a store lists, with its modification time (the sweep's
// grace periods are measured from it).
type Entry struct {
	Key     string // staging entries
	Sum     Sum    // final entries
	ModTime time.Time
}

// Store holds blob bytes. Staging keys are upload ids (UUIDs); final files are
// addressed by their SHA-256. Implementations must make every completed
// write durable (fsync of the file and its directory) before returning.
// The conformance suite in package conformance defines the contract.
type Store interface {
	// CreateStaging creates a new staging file. It fails if the key exists.
	// Close makes the written bytes durable.
	CreateStaging(key string) (io.WriteCloser, error)
	// RemoveStaging deletes a staging file; a missing file is not an error.
	RemoveStaging(key string) error
	// Staging lists staging files.
	Staging() ([]Entry, error)
	// Promote moves a staging file to the final path of sum durably. It
	// returns ErrExists, leaving both files, if the final file exists. The
	// caller must hold the per-sha lock (plan 11).
	Promote(key string, sum Sum) error
	// Open reads a final file; ErrNotFound if it does not exist.
	Open(sum Sum) (io.ReadCloser, error)
	// Verify re-hashes a final file.
	Verify(sum Sum) (Health, error)
	// RemoveFinal deletes a final file; a missing file is not an error. Only
	// the sweep calls it, for files with no blob_objects row, under the
	// per-sha lock.
	RemoveFinal(sum Sum) error
	// Finals lists final files.
	Finals() ([]Entry, error)
}
