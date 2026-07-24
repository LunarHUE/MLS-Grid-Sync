package storage

import (
	"context"
	"io"
)

// FakeStorer discards uploads and returns deterministic fake URLs.
type FakeStorer struct{}

func (f *FakeStorer) Upload(_ context.Context, key string, body io.Reader, _ string) (string, error) {
	// Enforce the same key contract as the filesystem-backed LocalStorer so the
	// fake is a faithful stand-in: empty/absolute/traversal keys are rejected
	// before any bytes are accepted.
	if err := validateKey(key); err != nil {
		return "", err
	}
	io.Copy(io.Discard, body) //nolint:errcheck
	return "https://fake-s3.local/" + key, nil
}

// Download always reports a miss. FakeStorer discards on upload, so there is
// never anything to read back — returning ErrObjectNotFound is the honest
// answer and keeps the read-through handler on its origin-fetch path rather
// than serving zero bytes as if they were a cache hit.
func (f *FakeStorer) Download(_ context.Context, key string) (io.ReadCloser, string, error) {
	if err := validateKey(key); err != nil {
		return nil, "", err
	}
	return nil, "", ErrObjectNotFound
}

// CleanupPrefix is a no-op: FakeStorer never persisted anything.
func (f *FakeStorer) CleanupPrefix(_ context.Context, _ string) error {
	return nil
}
