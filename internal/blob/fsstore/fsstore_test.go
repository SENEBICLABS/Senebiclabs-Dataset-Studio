package fsstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Backend{
		New: func(t *testing.T) blob.Store {
			s, err := New(filepath.Join(t.TempDir(), "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		Corrupt: func(t *testing.T, s blob.Store, sum blob.Sum) {
			p := s.(*Store).finalPath(sum)
			if err := os.WriteFile(p, []byte("tampered"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	})
}

func TestRootMustBeAbsolute(t *testing.T) {
	if _, err := New("relative/blobs"); err == nil {
		t.Error("relative root accepted")
	}
}

// Files that are not ours are never listed, so the sweep can never delete
// them.
func TestListingsIgnoreForeignFiles(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(s.staging, "notes.txt"),
		filepath.Join(s.final, "README"),
		filepath.Join(s.final, "ab", "cd", "not-a-hash"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A valid hash in the wrong directory is also foreign.
	misplaced := filepath.Join(s.final, "00", "00", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	if err := os.MkdirAll(filepath.Dir(misplaced), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(misplaced, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Staging(); err != nil || len(st) != 0 {
		t.Errorf("Staging = %v, %v", st, err)
	}
	if fin, err := s.Finals(); err != nil || len(fin) != 0 {
		t.Errorf("Finals = %v, %v", fin, err)
	}
}
