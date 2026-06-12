package storage

import (
	"context"
	"io"
)

// FakeStorer discards uploads and returns deterministic fake URLs.
type FakeStorer struct{}

func (f *FakeStorer) Upload(_ context.Context, key string, body io.Reader, _ string) (string, error) {
	io.Copy(io.Discard, body) //nolint:errcheck
	return "https://fake-s3.local/" + key, nil
}

// CleanupPrefix is a no-op: FakeStorer never persisted anything.
func (f *FakeStorer) CleanupPrefix(_ context.Context, _ string) error {
	return nil
}
