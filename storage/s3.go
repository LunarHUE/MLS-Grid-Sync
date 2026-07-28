package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Storer writes attachments to an S3-API bucket. Auth precedence
// (decision 2):
//  1. Endpoint non-empty → custom endpoint + UsePathStyle + static
//     creds from AccessKeyID/SecretAccessKey. The MinIO test path.
//  2. Else → default AWS config chain (env/instance role). Prod.
//
// The bucket is created idempotently at construction so a fresh
// emulator or a misconfigured prod account fails loudly at start.
// Credentials scoped to objects rather than buckets cannot create one;
// construction then falls back to proving the bucket already resolves.
type S3Storer struct {
	client   *s3.Client
	uploader *transfermanager.Client
	bucket   string
	region   string
	endpoint string
}

// NewS3 constructs the storer per the auth-precedence rules above.
// Region and bucket are required regardless of branch.
func NewS3(ctx context.Context, endpoint, bucket, region, accessKeyID, secretAccessKey string, usePathStyle bool) (*S3Storer, error) {
	if bucket == "" {
		return nil, fmt.Errorf("s3 backend requires storage.s3.bucket")
	}
	if region == "" {
		return nil, fmt.Errorf("s3 backend requires storage.s3.region")
	}

	var awsCfg aws.Config // zero value; populated below
	switch {
	case endpoint != "":
		if accessKeyID == "" || secretAccessKey == "" {
			return nil, fmt.Errorf("s3 custom endpoint requires access_key_id and secret_access_key")
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(region),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
		)
		if err != nil {
			return nil, fmt.Errorf("s3 load custom config: %w", err)
		}
		awsCfg = cfg
	default:
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return nil, fmt.Errorf("s3 load default config: %w", err)
		}
		awsCfg = cfg
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
		}
		o.UsePathStyle = usePathStyle
	})

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		// Tolerate already-exists. MinIO emits BucketAlreadyOwnedByYou
		// on second create when the requesting key already owns it,
		// matches real AWS behavior. BucketAlreadyExists fires when the
		// name collides with a different owner — also a "don't fail
		// construction" case for our purposes (we'll discover write
		// failure if creds can't actually write).
		var owned *s3types.BucketAlreadyOwnedByYou
		var exists *s3types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) {
			// Object-scoped credentials refuse CreateBucket outright rather
			// than reporting already-exists: Cloudflare R2 "Object Read &
			// Write" API tokens and AWS policies granting only
			// s3:GetObject/s3:PutObject both answer AccessDenied even when the
			// bucket is present and writable. Widening the tolerated-error set
			// to include AccessDenied would also swallow a wrong endpoint, a
			// bad key, or a typo'd bucket name — exactly the misconfiguration
			// this call exists to surface.
			//
			// So ask the authoritative question instead: does the bucket
			// resolve for these credentials? A HeadBucket that succeeds means
			// the only thing missing was permission to create a bucket that
			// already exists, which is not an error. Anything else fails
			// loudly, carrying both errors so the log says which wall we hit.
			if _, headErr := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); headErr != nil {
				return nil, fmt.Errorf("create bucket %q: %w (bucket also unreachable: %v)", bucket, err, headErr)
			}
		}
	}

	return &S3Storer{
		client:   client,
		uploader: transfermanager.New(client),
		bucket:   bucket,
		region:   region,
		endpoint: endpoint,
	}, nil
}

// Upload writes body to <bucket>/<key>. PLAIN PutObject via the
// transfer manager — no access conditions. DO NOT add If-None-Match
// or If-Match: the Storer contract is last-write-wins per
// storage/conformance_test.go:57-81. A conditional header would break
// the double-upload case silently.
func (s *S3Storer) Upload(ctx context.Context, key string, body io.Reader, contentType string) (string, error) {
	input := &transfermanager.UploadObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
		Body:   body,
	}
	if contentType != "" {
		input.ContentType = &contentType
	}
	if _, err := s.uploader.UploadObject(ctx, input); err != nil {
		return "", fmt.Errorf("upload %q: %w", key, err)
	}
	return s.objectURL(key), nil
}

// CleanupPrefix lists every object under the prefix and deletes them
// in batches of up to 1000 (the DeleteObjects API max).
func (s *S3Storer) CleanupPrefix(ctx context.Context, prefix string) error {
	var continuation *string
	for {
		input := &s3.ListObjectsV2Input{
			Bucket:            &s.bucket,
			ContinuationToken: continuation,
		}
		if prefix != "" {
			input.Prefix = &prefix
		}
		page, err := s.client.ListObjectsV2(ctx, input)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		if len(page.Contents) > 0 {
			ids := make([]s3types.ObjectIdentifier, 0, len(page.Contents))
			for _, obj := range page.Contents {
				if obj.Key == nil {
					continue
				}
				ids = append(ids, s3types.ObjectIdentifier{Key: obj.Key})
			}
			if len(ids) > 0 {
				if _, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
					Bucket: &s.bucket,
					Delete: &s3types.Delete{Objects: ids},
				}); err != nil {
					return fmt.Errorf("delete batch: %w", err)
				}
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		continuation = page.NextContinuationToken
	}
	return nil
}

// Download streams the object back out, satisfying Fetcher. NoSuchKey (and
// the bare 404 some S3-compatible servers return instead) maps to
// storage.ErrObjectNotFound so callers can treat a miss uniformly.
func (s *S3Storer) Download(ctx context.Context, key string) (io.ReadCloser, string, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		var noKey *s3types.NoSuchKey
		var notFound *s3types.NotFound
		if errors.As(err, &noKey) || errors.As(err, &notFound) {
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
func (s *S3Storer) downloadForTest(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// objectURL returns the canonical URL for an object. Endpoint set
// (MinIO) → path-style; empty (real AWS) → virtual-hosted.
func (s *S3Storer) objectURL(key string) string {
	if s.endpoint != "" {
		return strings.TrimRight(s.endpoint, "/") + "/" + s.bucket + "/" + key
	}
	return "https://" + s.bucket + ".s3." + s.region + ".amazonaws.com/" + key
}
