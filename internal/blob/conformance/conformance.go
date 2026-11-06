// Package conformance is the blob store conformance suite (plan 16.2, M5).
// Every blob.Store implementation must pass Run; the optional S3 store (O3)
// would be added by passing the same suite.
package conformance

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
)

// Backend creates stores for the suite and can damage stored content, which
// no Store method may do.
type Backend struct {
	New func(t *testing.T) blob.Store
	// Corrupt changes the bytes of the final file for sum in place.
	Corrupt func(t *testing.T, s blob.Store, sum blob.Sum)
}

func stage(t *testing.T, s blob.Store, data []byte) (string, blob.Sum) {
	t.Helper()
	key := uuid.Must(uuid.NewV7()).String()
	w, err := s.CreateStaging(key)
	if err != nil {
		t.Fatalf("CreateStaging: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return key, sha256.Sum256(data)
}

func read(t *testing.T, s blob.Store, sum blob.Sum) []byte {
	t.Helper()
	r, err := s.Open(sum)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Run runs the suite.
func Run(t *testing.T, b Backend) {
	t.Run("StagingIsExclusive", func(t *testing.T) {
		s := b.New(t)
		key, _ := stage(t, s, []byte("one"))
		if _, err := s.CreateStaging(key); !errors.Is(err, blob.ErrExists) {
			t.Errorf("second CreateStaging = %v, want ErrExists", err)
		}
	})
	t.Run("StagingKeysMustBeUUIDs", func(t *testing.T) {
		s := b.New(t)
		for _, k := range []string{"", "../escape", "a/b", "not-a-uuid", "0190A6E4-0000-7000-8000-0000000000A1"} {
			if _, err := s.CreateStaging(k); !errors.Is(err, blob.ErrBadKey) {
				t.Errorf("CreateStaging(%q) = %v, want ErrBadKey", k, err)
			}
		}
	})
	t.Run("PromoteThenOpenAndVerify", func(t *testing.T) {
		s := b.New(t)
		data := []byte("hello, blob")
		key, sum := stage(t, s, data)
		if err := s.Promote(key, sum); err != nil {
			t.Fatal(err)
		}
		if got := read(t, s, sum); !bytes.Equal(got, data) {
			t.Errorf("read %q", got)
		}
		if h, err := s.Verify(sum); err != nil || h != blob.Healthy {
			t.Errorf("Verify = %s, %v", h, err)
		}
		st, _ := s.Staging()
		if len(st) != 0 {
			t.Errorf("staging file left after promote: %v", st)
		}
		fin, err := s.Finals()
		if err != nil || len(fin) != 1 || fin[0].Sum != sum {
			t.Errorf("Finals = %v, %v", fin, err)
		}
	})
	t.Run("PromoteRefusesAnExistingFinal", func(t *testing.T) {
		s := b.New(t)
		k1, sum := stage(t, s, []byte("same"))
		if err := s.Promote(k1, sum); err != nil {
			t.Fatal(err)
		}
		k2, _ := stage(t, s, []byte("same"))
		if err := s.Promote(k2, sum); !errors.Is(err, blob.ErrExists) {
			t.Errorf("second Promote = %v, want ErrExists", err)
		}
		st, _ := s.Staging()
		if len(st) != 1 || st[0].Key != k2 {
			t.Errorf("the refused staging file must remain: %v", st)
		}
	})
	t.Run("MissingAndCorrupt", func(t *testing.T) {
		s := b.New(t)
		absent := sha256.Sum256([]byte("never stored"))
		if h, err := s.Verify(absent); err != nil || h != blob.Missing {
			t.Errorf("Verify(absent) = %s, %v", h, err)
		}
		if _, err := s.Open(absent); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Open(absent) = %v", err)
		}
		key, sum := stage(t, s, []byte("will be damaged"))
		if err := s.Promote(key, sum); err != nil {
			t.Fatal(err)
		}
		b.Corrupt(t, s, sum)
		if h, err := s.Verify(sum); err != nil || h != blob.Corrupt {
			t.Errorf("Verify(corrupted) = %s, %v", h, err)
		}
	})
	t.Run("RemovesAreIdempotent", func(t *testing.T) {
		s := b.New(t)
		key, sum := stage(t, s, []byte("x"))
		for range 2 {
			if err := s.RemoveStaging(key); err != nil {
				t.Errorf("RemoveStaging: %v", err)
			}
			if err := s.RemoveFinal(sum); err != nil {
				t.Errorf("RemoveFinal: %v", err)
			}
		}
		if st, _ := s.Staging(); len(st) != 0 {
			t.Errorf("staging not removed: %v", st)
		}
	})
	t.Run("EmptyContent", func(t *testing.T) {
		s := b.New(t)
		key, sum := stage(t, s, nil)
		if err := s.Promote(key, sum); err != nil {
			t.Fatal(err)
		}
		if got := read(t, s, sum); len(got) != 0 {
			t.Errorf("read %d bytes", len(got))
		}
	})
}
