package storage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// AzureBlobStorer writes attachments to an Azure Blob Storage container.
// Auth precedence (decision 1):
//  1. ConnectionString non-empty → connection-string client (the Azurite
//     test path with the well-known devstoreaccount1 string).
//  2. Else AccountURL → DefaultAzureCredential (the eventual
//     managed-identity prod path).
//
// The container is created idempotently at construction so a fresh
// emulator or a misconfigured prod account fails loudly at start, not
// on the first upload.
type AzureBlobStorer struct {
	client    *azblob.Client
	container string
}

// NewAzureBlob constructs the storer per the auth-precedence rules
// above. Container is required regardless of branch.
func NewAzureBlob(ctx context.Context, connectionString, accountURL, container string) (*AzureBlobStorer, error) {
	if container == "" {
		return nil, fmt.Errorf("azure backend requires storage.azure.container")
	}

	var (
		client *azblob.Client
		err    error
	)
	switch {
	case connectionString != "":
		client, err = azblob.NewClientFromConnectionString(connectionString, nil)
		if err != nil {
			return nil, fmt.Errorf("azure connection-string client: %w", err)
		}
	case accountURL != "":
		cred, credErr := azidentity.NewDefaultAzureCredential(nil)
		if credErr != nil {
			return nil, fmt.Errorf("azure default credential: %w", credErr)
		}
		client, err = azblob.NewClient(accountURL, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure client: %w", err)
		}
	default:
		return nil, fmt.Errorf("azure backend requires storage.azure.connection_string OR storage.azure.account_url")
	}

	if _, err := client.CreateContainer(ctx, container, nil); err != nil {
		if !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			return nil, fmt.Errorf("create container %q: %w", container, err)
		}
	}

	return &AzureBlobStorer{client: client, container: container}, nil
}

// Upload writes body to <container>/<key>. PLAIN UploadStream — no
// access conditions. DO NOT add If-None-Match: the Storer contract is
// last-write-wins per storage/conformance_test.go:57-81. A conditional
// header would break the double-upload case silently.
func (s *AzureBlobStorer) Upload(ctx context.Context, key string, body io.Reader, contentType string) (string, error) {
	var headers *blob.HTTPHeaders
	if contentType != "" {
		headers = &blob.HTTPHeaders{BlobContentType: to.Ptr(contentType)}
	}
	if _, err := s.client.UploadStream(ctx, s.container, key, body, &azblob.UploadStreamOptions{
		HTTPHeaders: headers,
	}); err != nil {
		return "", fmt.Errorf("upload %q: %w", key, err)
	}
	// client.URL() returns the service URL ending in the account name
	// (e.g. http://127.0.0.1:10000/devstoreaccount1 for Azurite, or
	// https://<acct>.blob.core.windows.net for real Azure). Compose
	// the container + key directly — URL escaping is unnecessary for
	// our sha256-hashed key shape.
	return joinAzureURL(s.client.URL(), s.container, key), nil
}

// CleanupPrefix lists every blob under the prefix and deletes each.
// Tolerates already-deleted blobs (a concurrent run or retry).
// An empty prefix matches every blob in the container, which is the
// explicit "wipe the whole container" semantic the subcommand exposes.
func (s *AzureBlobStorer) CleanupPrefix(ctx context.Context, prefix string) error {
	opts := &azblob.ListBlobsFlatOptions{}
	if prefix != "" {
		opts.Prefix = to.Ptr(prefix)
	}
	pager := s.client.NewListBlobsFlatPager(s.container, opts)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list blobs: %w", err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			if _, err := s.client.DeleteBlob(ctx, s.container, *item.Name, nil); err != nil {
				if bloberror.HasCode(err, bloberror.BlobNotFound) {
					continue
				}
				return fmt.Errorf("delete %q: %w", *item.Name, err)
			}
		}
	}
	return nil
}

// Download streams <container>/<key> back out, satisfying Fetcher. A missing
// blob maps to storage.ErrObjectNotFound so the read-through handler can tell
// a cache miss from a real backend fault without knowing about azblob.
//
// The body is streamed, not buffered — a large image must not cost the serve
// process its own copy in memory per in-flight request.
func (s *AzureBlobStorer) Download(ctx context.Context, key string) (io.ReadCloser, string, error) {
	resp, err := s.client.DownloadStream(ctx, s.container, key, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
			return nil, "", ErrObjectNotFound
		}
		return nil, "", fmt.Errorf("download %q: %w", key, err)
	}
	var contentType string
	if resp.ContentType != nil {
		contentType = *resp.ContentType
	}
	return resp.Body, contentType, nil
}

// downloadForTest is a package-internal test helper for the
// conformance suite's readback case. Kept here so it participates in
// the package build but stays out of the production Storer interface.
func (s *AzureBlobStorer) downloadForTest(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.client.DownloadStream(ctx, s.container, key, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// joinAzureURL composes the blob URL from the service URL, container,
// and key. Service URLs may or may not carry a trailing slash; both
// shapes are handled.
func joinAzureURL(serviceURL, container, key string) string {
	base := strings.TrimRight(serviceURL, "/")
	return base + "/" + container + "/" + key
}
