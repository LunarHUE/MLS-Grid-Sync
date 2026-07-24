package storage

import (
	"context"
	"errors"
	"io"
)

// ErrObjectNotFound is returned by Fetcher.Download when nothing is stored
// under the key. It is the signal that separates a cache miss — recoverable,
// the caller re-fetches from the upstream origin — from a backend fault,
// which is not. Backends must map their own not-found representation
// (Azure BlobNotFound, S3 NoSuchKey, os.ErrNotExist) onto this so callers
// never have to type-switch per backend.
var ErrObjectNotFound = errors.New("storage: object not found")

// Storer uploads a file and returns the public host URL.
type Storer interface {
	Upload(ctx context.Context, key string, body io.Reader, contentType string) (hostURL string, err error)
}

// Fetcher is an optional capability implemented by storers that can read an
// object back out. It exists for the read-through media endpoint, which
// serves cached bytes on a hit and populates them on a miss.
//
// It is deliberately a separate interface rather than a method on Storer,
// matching the Cleaner pattern: an upload-only backend stays valid, and the
// handler type-asserts and reports a clear configuration error instead of
// every backend growing a stub that panics.
//
// The returned body is the caller's to close.
type Fetcher interface {
	Download(ctx context.Context, key string) (body io.ReadCloser, contentType string, err error)
}

// Cleaner is an optional capability implemented by storers that support
// bulk prefix removal. The worker-storage-cleanup subcommand type-asserts
// for it; backends without it cannot be cleaned via that path.
type Cleaner interface {
	CleanupPrefix(ctx context.Context, prefix string) error
}
