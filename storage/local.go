package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ErrLocalStorerFull is returned by LocalStorer.Upload when the byte
// budget is exhausted. The worker's failure classifier maps this to
// permanently_failed — a config-bound cap that retry cannot resolve.
var ErrLocalStorerFull = errors.New("local storer at capacity")

// LocalStorer writes uploaded bytes under rootDir using a temp+rename
// atomic write so a crashed test cannot leave half-written files that
// pass file(1). It enforces a total-bytes ceiling as a runaway-test
// safety bound. Not for production.
type LocalStorer struct {
	rootDir  string
	capBytes int64
	used     atomic.Int64
}

// NewLocal opens (creating if needed) rootDir as the storage root and
// counts existing bytes against the cap so a restart does not
// double-budget a partially-filled directory.
func NewLocal(rootDir string, capBytes int64) (*LocalStorer, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve abs path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create root: %w", err)
	}
	s := &LocalStorer{rootDir: abs, capBytes: capBytes}
	if err := s.recountUsed(); err != nil {
		return nil, fmt.Errorf("count existing: %w", err)
	}
	return s, nil
}

// RootDir reports the absolute root directory — useful for the cleanup
// subcommand and for the smoke test's "where did files land?" question.
func (s *LocalStorer) RootDir() string { return s.rootDir }

func (s *LocalStorer) recountUsed() error {
	var total int64
	err := filepath.WalkDir(s.rootDir, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	s.used.Store(total)
	return nil
}

// validateKey rejects absolute paths and any traversal that would
// escape rootDir. Keys come from our own code, but enforcing here is
// cheap and makes the file:// URL a contract rather than a hope.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty key")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("absolute key not allowed: %q", key)
	}
	clean := filepath.Clean(key)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return fmt.Errorf("path traversal in key: %q", key)
	}
	return nil
}

func (s *LocalStorer) Upload(_ context.Context, key string, body io.Reader, _ string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	if s.used.Load() >= s.capBytes {
		return "", ErrLocalStorerFull
	}

	finalPath := filepath.Join(s.rootDir, key)
	parentDir := filepath.Dir(finalPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent: %w", err)
	}

	tmp, err := os.CreateTemp(parentDir, ".upload-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}
	defer cleanup()

	n, copyErr := io.Copy(tmp, body)
	if copyErr != nil {
		tmp.Close()
		return "", fmt.Errorf("write body: %w", copyErr)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close: %w", err)
	}

	// Post-write cap check: the body may exceed remaining budget by
	// more than zero. Roll back budget on overflow; defer cleans tmp.
	if s.used.Add(n) > s.capBytes {
		s.used.Add(-n)
		return "", ErrLocalStorerFull
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		s.used.Add(-n)
		return "", fmt.Errorf("rename: %w", err)
	}
	tmpPath = "" // disarm cleanup — file is now at finalPath
	return "file://" + finalPath, nil
}

// Download opens <rootDir>/<key> for reading, satisfying Fetcher. The key is
// validated exactly as on the upload path so a traversal key cannot read back
// outside the root. Content type is returned empty: the filesystem does not
// carry one, and the caller falls back to what it recorded at upload time.
func (s *LocalStorer) Download(_ context.Context, key string) (io.ReadCloser, string, error) {
	if err := validateKey(key); err != nil {
		return nil, "", err
	}
	f, err := os.Open(filepath.Join(s.rootDir, key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", ErrObjectNotFound
		}
		return nil, "", fmt.Errorf("open %q: %w", key, err)
	}
	return f, "", nil
}

// CleanupPrefix recursively removes <rootDir>/<prefix> and refreshes
// the byte counter. Empty prefix removes the whole root (subcommand
// gates this behind explicit confirmation).
func (s *LocalStorer) CleanupPrefix(_ context.Context, prefix string) error {
	if prefix != "" {
		if err := validateKey(prefix); err != nil {
			return err
		}
	}
	target := filepath.Join(s.rootDir, prefix)
	// Defense-in-depth: refuse to delete anything outside rootDir.
	rel, err := filepath.Rel(s.rootDir, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("cleanup target escapes root: %q", prefix)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove %q: %w", target, err)
	}
	// If the root itself was removed, recreate it so the storer stays
	// usable. NewLocal's initial MkdirAll won't have helped here.
	if err := os.MkdirAll(s.rootDir, 0o755); err != nil {
		return fmt.Errorf("recreate root: %w", err)
	}
	return s.recountUsed()
}
