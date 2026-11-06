package blob

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/blobdb"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
)

// Advisory locks, in PostgreSQL's two-key space so they never collide with
// single-key locks such as River's.
const (
	// LockNamespaceBackupGC with key 0 is BACKUP_GC (plan 11, 13): a backup
	// holds it shared for its whole run; blob.sweep try-locks it exclusively
	// and skips its run if it cannot get it.
	LockNamespaceBackupGC int32 = 0x44530001
	// LockNamespaceBlob with a key derived from the SHA-256 is blob_key(sha)
	// (plan 11). Hash collisions in the key only serialise unrelated
	// uploads; they never break correctness.
	LockNamespaceBlob int32 = 0x44530002
)

// BlobKey is the advisory-lock key of sum: its first four bytes.
func BlobKey(sum Sum) int32 {
	return int32(binary.BigEndian.Uint32(sum[:4])) //nolint:gosec // G115: deliberate wrap-around; any 32 bits of the hash are a valid lock key.
}

// Errors from the service.
var (
	ErrTooLarge   = errors.New("blob: upload exceeds the size limit")
	ErrNotStaging = errors.New("blob: upload is not staging")
	ErrUnhealthy  = errors.New("blob: the stored copy of this content is missing or corrupt")
)

// Service runs the upload protocol over a Store.
type Service struct {
	store    Store
	maxBytes int64
}

// NewService returns a Service. maxBytes is the per-upload cap (plan 14).
func NewService(store Store, maxBytes int64) (*Service, error) {
	if store == nil || maxBytes <= 0 {
		return nil, errors.New("blob: service needs a store and a positive size limit")
	}
	return &Service{store: store, maxBytes: maxBytes}, nil
}

// Staged is a fully written, fsynced upload awaiting transaction 2.
type Staged struct {
	UploadID  uuid.UUID
	Sum       Sum
	Size      int64
	MediaType string // sniffed from the content (plan 14)
}

// Object is a committed blob.
type Object struct {
	ID           uuid.UUID
	Sum          Sum
	Size         int64
	MediaType    string
	Deduplicated bool // the content was already stored
}

// Stage performs transaction 1 and streams r to staging (plan 11, steps 1
// and 2). The upload row is committed before any byte is written, so a crash
// at any later point leaves something blob.sweep can find. On any failure
// the staging file is removed and the upload marked abandoned.
func (s *Service) Stage(ctx context.Context, conn db.Beginner, r io.Reader, createdBy uuid.UUID) (Staged, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Staged{}, fmt.Errorf("blob: upload id: %w", err)
	}
	if err := db.WithTx(ctx, conn, func(tx pgx.Tx) error {
		return blobdb.New(tx).InsertUpload(ctx, blobdb.InsertUploadParams{ID: id, StagingKey: id.String(), CreatedBy: createdBy})
	}); err != nil {
		return Staged{}, fmt.Errorf("blob: record upload: %w", err)
	}
	st, err := s.write(id, r)
	if err != nil {
		_ = s.store.RemoveStaging(id.String())
		if aerr := db.WithTx(context.WithoutCancel(ctx), conn, func(tx pgx.Tx) error {
			_, e := blobdb.New(tx).AbandonUpload(ctx, id)
			return e
		}); aerr != nil {
			// blob.sweep abandons it after expiry instead.
			return Staged{}, errors.Join(err, fmt.Errorf("blob: mark upload abandoned: %w", aerr))
		}
		return Staged{}, err
	}
	return st, nil
}

// sniffer keeps the first 512 bytes for http.DetectContentType.
type sniffer struct{ head []byte }

func (s *sniffer) Write(p []byte) (int, error) {
	if room := 512 - len(s.head); room > 0 {
		s.head = append(s.head, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (s *Service) write(id uuid.UUID, r io.Reader) (Staged, error) {
	w, err := s.store.CreateStaging(id.String())
	if err != nil {
		return Staged{}, fmt.Errorf("blob: create staging file: %w", err)
	}
	h, sn := sha256.New(), &sniffer{}
	n, err := io.Copy(io.MultiWriter(w, h, sn), io.LimitReader(r, s.maxBytes+1))
	if err == nil && n > s.maxBytes {
		err = fmt.Errorf("%w (%d bytes)", ErrTooLarge, s.maxBytes)
	}
	if err != nil {
		_ = w.Close()
		return Staged{}, fmt.Errorf("blob: write staging file: %w", err)
	}
	if err := w.Close(); err != nil {
		return Staged{}, err
	}
	st := Staged{UploadID: id, Size: n, MediaType: http.DetectContentType(sn.head)}
	copy(st.Sum[:], h.Sum(nil))
	return st, nil
}

// Commit performs transaction 2's blob part inside tx, the caller's command
// transaction (plan 11, step 3): the caller then inserts its referencing rows
// and its audit event in the same tx. It locks the upload row and then the
// per-sha key, in that order (blob.sweep takes them in the same order).
// If tx rolls back after the rename, the final file has no row and blob.sweep
// removes it after the grace period.
func (s *Service) Commit(ctx context.Context, tx pgx.Tx, st Staged, createdBy uuid.UUID) (Object, error) {
	q := blobdb.New(tx)
	up, err := q.GetUpload(ctx, st.UploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Object{}, fmt.Errorf("%w: unknown upload %s", ErrNotStaging, st.UploadID)
	}
	if err != nil {
		return Object{}, fmt.Errorf("blob: read upload: %w", err)
	}
	if up.State != "staging" {
		return Object{}, fmt.Errorf("%w: upload %s is %s", ErrNotStaging, st.UploadID, up.State)
	}
	if err := q.LockBlobKey(ctx, blobdb.LockBlobKeyParams{Namespace: LockNamespaceBlob, Key: BlobKey(st.Sum)}); err != nil {
		return Object{}, fmt.Errorf("blob: lock: %w", err)
	}
	size := st.Size
	existing, err := q.GetBlobBySHA256(ctx, st.Sum[:])
	switch {
	case err == nil:
		if err := s.requireHealthy(st.Sum); err != nil {
			return Object{}, err
		}
		if err := s.store.RemoveStaging(up.StagingKey); err != nil {
			return Object{}, err
		}
		if n, err := q.MarkDeduplicated(ctx, blobdb.MarkDeduplicatedParams{ID: st.UploadID, Sha256: st.Sum[:], Size: &size}); err != nil || n != 1 {
			return Object{}, fmt.Errorf("blob: mark deduplicated (%d rows): %w", n, err)
		}
		return Object{ID: existing.ID, Sum: st.Sum, Size: existing.SizeBytes, MediaType: existing.MediaType, Deduplicated: true}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Object{}, fmt.Errorf("blob: look up content: %w", err)
	}

	switch err := s.store.Promote(up.StagingKey, st.Sum); {
	case errors.Is(err, ErrExists):
		// A final file with no row: an earlier commit of this content rolled
		// back after its rename. Within the grace period the file is reused
		// (plan 11) once it is verified.
		if err := s.requireHealthy(st.Sum); err != nil {
			return Object{}, err
		}
		if err := s.store.RemoveStaging(up.StagingKey); err != nil {
			return Object{}, err
		}
	case err != nil:
		return Object{}, err
	}
	objID, err := uuid.NewV7()
	if err != nil {
		return Object{}, fmt.Errorf("blob: object id: %w", err)
	}
	if n, err := q.InsertBlob(ctx, blobdb.InsertBlobParams{
		ID: objID, Sha256: st.Sum[:], SizeBytes: st.Size, MediaType: st.MediaType, CreatedBy: createdBy,
	}); err != nil || n != 1 {
		// Impossible under the per-sha lock unless the lock was not taken.
		return Object{}, fmt.Errorf("blob: insert blob (%d rows): %w", n, err)
	}
	if n, err := q.MarkPromoted(ctx, blobdb.MarkPromotedParams{ID: st.UploadID, Sha256: st.Sum[:], Size: &size}); err != nil || n != 1 {
		return Object{}, fmt.Errorf("blob: mark promoted (%d rows): %w", n, err)
	}
	return Object{ID: objID, Sum: st.Sum, Size: st.Size, MediaType: st.MediaType}, nil
}

func (s *Service) requireHealthy(sum Sum) error {
	h, err := s.store.Verify(sum)
	if err != nil {
		return err
	}
	if h != Healthy {
		return fmt.Errorf("%w (%s: %s)", ErrUnhealthy, sum, h)
	}
	return nil
}

// Open reads a committed blob's bytes.
func (s *Service) Open(sum Sum) (io.ReadCloser, error) { return s.store.Open(sum) }
