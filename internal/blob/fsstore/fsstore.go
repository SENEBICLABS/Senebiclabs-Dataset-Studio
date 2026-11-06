// Package fsstore is the local filesystem blob store (plan 4 and 11).
//
// Layout under the root:
//
//	staging/<upload id>         uploads in progress
//	sha256/ab/cd/<sha256 hex>   committed content, by hash
//
// Every completed write is made durable: the file is fsynced before it is
// closed, and each directory whose entries change is fsynced after the
// change. Staging keys must be canonical UUIDs, so no key can name a path
// outside the staging directory.
package fsstore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
)

const (
	dirPerm  = 0o750
	filePerm = 0o600
)

// Store is a filesystem blob store.
type Store struct {
	root    string
	staging string
	final   string
}

var _ blob.Store = (*Store)(nil)

// New opens (creating if needed) a store at root, which must be absolute.
func New(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("fsstore: root must be an absolute path")
	}
	s := &Store{root: root, staging: filepath.Join(root, "staging"), final: filepath.Join(root, "sha256")}
	for _, d := range []string{root, s.staging, s.final} {
		if err := os.MkdirAll(d, dirPerm); err != nil {
			return nil, fmt.Errorf("fsstore: create %s: %w", d, err)
		}
	}
	if err := syncDir(filepath.Dir(root)); err != nil {
		return nil, err
	}
	if err := syncDir(root); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) stagingPath(key string) (string, error) {
	u, err := uuid.Parse(key)
	if err != nil || u.String() != key {
		return "", blob.ErrBadKey
	}
	return filepath.Join(s.staging, key), nil
}

func (s *Store) finalPath(sum blob.Sum) string {
	h := sum.String()
	return filepath.Join(s.final, h[0:2], h[2:4], h)
}

type stagingFile struct {
	f   *os.File
	dir string
}

func (w *stagingFile) Write(p []byte) (int, error) { return w.f.Write(p) }

// Close fsyncs the file and its directory, then closes it.
func (w *stagingFile) Close() error {
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return fmt.Errorf("fsstore: fsync staging file: %w", err)
	}
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("fsstore: close staging file: %w", err)
	}
	return syncDir(w.dir)
}

// CreateStaging implements blob.Store.
func (s *Store) CreateStaging(key string) (io.WriteCloser, error) {
	p, err := s.stagingPath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm) //nolint:gosec // G304: p is staging/<canonical UUID>, checked in stagingPath.
	if errors.Is(err, fs.ErrExist) {
		return nil, blob.ErrExists
	}
	if err != nil {
		return nil, fmt.Errorf("fsstore: create staging file: %w", err)
	}
	return &stagingFile{f: f, dir: s.staging}, nil
}

// RemoveStaging implements blob.Store.
func (s *Store) RemoveStaging(key string) error {
	p, err := s.stagingPath(key)
	if err != nil {
		return err
	}
	return removeDurably(p)
}

// Staging implements blob.Store. Entries that are not upload ids are not
// ours and are ignored.
func (s *Store) Staging() ([]blob.Entry, error) {
	des, err := os.ReadDir(s.staging)
	if err != nil {
		return nil, fmt.Errorf("fsstore: list staging: %w", err)
	}
	var out []blob.Entry
	for _, de := range des {
		if _, err := s.stagingPath(de.Name()); err != nil || !de.Type().IsRegular() {
			continue
		}
		info, err := de.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("fsstore: stat staging file: %w", err)
		}
		out = append(out, blob.Entry{Key: de.Name(), ModTime: info.ModTime()})
	}
	return out, nil
}

// Promote implements blob.Store.
func (s *Store) Promote(key string, sum blob.Sum) error {
	src, err := s.stagingPath(key)
	if err != nil {
		return err
	}
	dst := s.finalPath(sum)
	if err := s.mkdirDurably(filepath.Dir(dst)); err != nil {
		return err
	}
	// The caller holds the per-sha lock, so nothing else creates dst between
	// this check and the rename.
	if _, err := os.Lstat(dst); err == nil {
		return blob.ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fsstore: stat final file: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("fsstore: promote: %w", err)
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	return syncDir(s.staging)
}

// mkdirDurably creates dir and any missing parents below the final root,
// fsyncing each parent whose entries changed.
func (s *Store) mkdirDurably(dir string) error {
	rel, err := filepath.Rel(s.final, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return errors.New("fsstore: directory outside the store")
	}
	cur := s.final
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		next := filepath.Join(cur, part)
		err := os.Mkdir(next, dirPerm)
		switch {
		case err == nil:
			if err := syncDir(cur); err != nil {
				return err
			}
		case errors.Is(err, fs.ErrExist):
		default:
			return fmt.Errorf("fsstore: create %s: %w", next, err)
		}
		cur = next
	}
	return nil
}

// Open implements blob.Store.
func (s *Store) Open(sum blob.Sum) (io.ReadCloser, error) {
	f, err := os.Open(s.finalPath(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, blob.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("fsstore: open: %w", err)
	}
	return f, nil
}

// Verify implements blob.Store.
func (s *Store) Verify(sum blob.Sum) (blob.Health, error) {
	f, err := s.Open(sum)
	if errors.Is(err, blob.ErrNotFound) {
		return blob.Missing, nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("fsstore: read %s: %w", sum, err)
	}
	var got blob.Sum
	copy(got[:], h.Sum(nil))
	if got != sum {
		return blob.Corrupt, nil
	}
	return blob.Healthy, nil
}

// RemoveFinal implements blob.Store.
func (s *Store) RemoveFinal(sum blob.Sum) error { return removeDurably(s.finalPath(sum)) }

// Finals implements blob.Store. Files whose names are not a SHA-256 in the
// right directory are not ours and are ignored.
func (s *Store) Finals() ([]blob.Entry, error) {
	var out []blob.Entry
	err := filepath.WalkDir(s.final, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if de.IsDir() || !de.Type().IsRegular() {
			return nil
		}
		sum, perr := blob.ParseSum(de.Name())
		if perr != nil || p != s.finalPath(sum) {
			return nil
		}
		info, ierr := de.Info()
		if errors.Is(ierr, fs.ErrNotExist) {
			return nil
		}
		if ierr != nil {
			return ierr
		}
		out = append(out, blob.Entry{Sum: sum, ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fsstore: list final files: %w", err)
	}
	return out, nil
}

func removeDurably(p string) error {
	err := os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fsstore: remove: %w", err)
	}
	return syncDir(filepath.Dir(p))
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is a directory inside, or the parent of, the configured store root.
	if err != nil {
		return fmt.Errorf("fsstore: open directory for fsync: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsstore: fsync directory: %w", err)
	}
	return nil
}
