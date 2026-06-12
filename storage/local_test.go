package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const oneGiB = int64(1) << 30

func newTestLocal(t *testing.T, cap int64) *LocalStorer {
	t.Helper()
	root := t.TempDir()
	s, err := NewLocal(root, cap)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return s
}

func TestLocalStorer_Conformance(t *testing.T) {
	t.Parallel()
	testStorerConformance(t, func(t *testing.T) conformanceFixture {
		s := newTestLocal(t, oneGiB)
		return conformanceFixture{
			name:   "local",
			storer: s,
			readback: func(t *testing.T, key string) []byte {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(s.RootDir(), key))
				if err != nil {
					t.Fatalf("readback %q: %v", key, err)
				}
				return data
			},
			urlAssertion: func(t *testing.T, key, url string) {
				if !strings.HasPrefix(url, "file://") {
					t.Errorf("LocalStorer URL %q missing file:// prefix", url)
				}
				if !strings.Contains(url, key) {
					t.Errorf("LocalStorer URL %q missing key %q", url, key)
				}
				if !strings.Contains(url, s.RootDir()) {
					t.Errorf("LocalStorer URL %q missing root %q", url, s.RootDir())
				}
			},
			supportsAtomicity: true,
		}
	})
}

func TestLocalStorer_CapEnforcedAtBoundary(t *testing.T) {
	t.Parallel()
	const cap = 100
	s := newTestLocal(t, cap)

	// Fill to exactly the cap.
	if _, err := s.Upload(context.Background(), "fill", bytes.NewReader(make([]byte, cap)), ""); err != nil {
		t.Fatalf("filling to cap: %v", err)
	}
	// Next byte must be rejected.
	_, err := s.Upload(context.Background(), "overflow", bytes.NewReader([]byte("x")), "")
	if !errors.Is(err, ErrLocalStorerFull) {
		t.Errorf("expected ErrLocalStorerFull at cap boundary, got %v", err)
	}
}

func TestLocalStorer_CapEnforcedOnOversize(t *testing.T) {
	t.Parallel()
	// A single upload larger than the cap must be rejected, and the
	// budget must roll back so subsequent small uploads still work.
	const cap = 10
	s := newTestLocal(t, cap)

	_, err := s.Upload(context.Background(), "huge", bytes.NewReader(make([]byte, 100)), "")
	if !errors.Is(err, ErrLocalStorerFull) {
		t.Errorf("expected ErrLocalStorerFull on oversize upload, got %v", err)
	}
	// A small subsequent upload must succeed — budget rolled back.
	if _, err := s.Upload(context.Background(), "small", bytes.NewReader([]byte("ok")), ""); err != nil {
		t.Errorf("expected post-rollback upload to succeed, got %v", err)
	}
}

func TestLocalStorer_PathTraversalRejected(t *testing.T) {
	t.Parallel()
	s := newTestLocal(t, oneGiB)
	cases := []string{
		"../escape",
		"a/../../escape",
		"/absolute",
		"",
	}
	for _, key := range cases {
		if _, err := s.Upload(context.Background(), key, bytes.NewReader([]byte("x")), ""); err == nil {
			t.Errorf("expected rejection for key %q", key)
		}
	}
}

func TestLocalStorer_AtomicWriteNoHalfFile(t *testing.T) {
	t.Parallel()
	// Mid-stream error: io.Copy returns the failingReader's error after
	// some bytes have been written to the temp file. The final path must
	// not exist; the temp file must be cleaned up.
	s := newTestLocal(t, oneGiB)
	body := &failingReader{
		src: bytes.NewReader([]byte("partial-")),
		err: errors.New("simulated network drop"),
	}
	_, err := s.Upload(context.Background(), "key-that-fails", body, "")
	if err == nil {
		t.Fatal("expected upload to fail")
	}

	finalPath := filepath.Join(s.RootDir(), "key-that-fails")
	if _, statErr := os.Stat(finalPath); statErr == nil {
		t.Errorf("final path %q exists after failed upload", finalPath)
	}

	// Sweep for orphaned temp files under root.
	var found []string
	_ = filepath.WalkDir(s.RootDir(), func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.Base(path), ".upload-") {
			found = append(found, path)
		}
		return nil
	})
	if len(found) > 0 {
		t.Errorf("temp files left behind: %v", found)
	}
}

func TestLocalStorer_CleanupPrefixScopeExact(t *testing.T) {
	t.Parallel()
	s := newTestLocal(t, oneGiB)
	ctx := context.Background()
	mustUpload := func(key string) {
		t.Helper()
		if _, err := s.Upload(ctx, key, bytes.NewReader([]byte("x")), ""); err != nil {
			t.Fatalf("upload %q: %v", key, err)
		}
	}
	mustUpload("alpha/one")
	mustUpload("alpha/two")
	mustUpload("beta/sibling")

	if err := s.CleanupPrefix(ctx, "alpha"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// alpha/* gone, beta/sibling intact.
	if _, err := os.Stat(filepath.Join(s.RootDir(), "alpha")); !os.IsNotExist(err) {
		t.Errorf("expected alpha removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.RootDir(), "beta", "sibling")); err != nil {
		t.Errorf("expected beta/sibling intact, stat err = %v", err)
	}
}

func TestLocalStorer_RestartRecountsUsed(t *testing.T) {
	t.Parallel()
	// A restart against a partially-filled rootDir must NOT
	// double-budget existing files (otherwise the cap drifts).
	root := t.TempDir()
	first, err := NewLocal(root, oneGiB)
	if err != nil {
		t.Fatalf("first NewLocal: %v", err)
	}
	if _, err := first.Upload(context.Background(), "preexisting", bytes.NewReader(make([]byte, 50)), ""); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	second, err := NewLocal(root, oneGiB)
	if err != nil {
		t.Fatalf("second NewLocal: %v", err)
	}
	if got := second.used.Load(); got != 50 {
		t.Errorf("recount after restart = %d, want 50", got)
	}
}

// readbackHelper is used by the conformance suite. Kept here so it
// participates in the package's test build.
var _ = io.Reader(nil) // pin import
