package storage

import (
	"context"
	"io"
)

// Storer uploads a file and returns the public host URL.
type Storer interface {
	Upload(ctx context.Context, key string, body io.Reader, contentType string) (hostURL string, err error)
}

// Cleaner is an optional capability implemented by storers that support
// bulk prefix removal. The worker-storage-cleanup subcommand type-asserts
// for it; backends without it cannot be cleaned via that path.
type Cleaner interface {
	CleanupPrefix(ctx context.Context, prefix string) error
}
