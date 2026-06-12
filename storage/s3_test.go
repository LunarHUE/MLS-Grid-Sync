package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// One MinIO container is shared by every test in the process — tests
// already isolate via randBucketName, so per-test containers buy
// nothing. The container is reaped by testcontainers' ryuk at process
// exit, so no Terminate is registered.
var (
	minioOnce     sync.Once
	minioEndpoint string
	minioErr      error
)

// startMinio returns the http://host:port endpoint of the shared MinIO
// container, starting it on first use.
func startMinio(t *testing.T) string {
	t.Helper()
	minioOnce.Do(func() {
		ctx := context.Background()

		ctr, err := testcontainers.Run(ctx,
			"minio/minio:latest",
			testcontainers.WithExposedPorts("9000/tcp"),
			testcontainers.WithCmd("server", "/data"),
			testcontainers.WithEnv(map[string]string{
				"MINIO_ROOT_USER":     "minioadmin",
				"MINIO_ROOT_PASSWORD": "minioadmin",
			}),
			testcontainers.WithWaitStrategy(
				wait.ForListeningPort("9000/tcp"),
			),
		)
		if err != nil {
			minioErr = fmt.Errorf("start minio: %w", err)
			return
		}

		host, err := ctr.Host(ctx)
		if err != nil {
			minioErr = fmt.Errorf("minio host: %w", err)
			return
		}
		port, err := ctr.MappedPort(ctx, "9000/tcp")
		if err != nil {
			minioErr = fmt.Errorf("minio port: %w", err)
			return
		}
		minioEndpoint = "http://" + host + ":" + port.Port()
	})
	if minioErr != nil {
		t.Fatalf("shared minio container: %v", minioErr)
	}
	return minioEndpoint
}

// randBucketName returns a fresh per-test-call bucket name. The
// conformance suite calls makeFixture per subtest; each subtest gets
// its own bucket — matching LocalStorer's t.TempDir() semantic and
// the Azure storer's randContainerName.
func randBucketName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// S3 bucket names must start with a lowercase letter or digit and
	// be 3–63 chars. "media-<hex>" is 18 chars and valid.
	return "media-" + hex.EncodeToString(buf[:])
}

func TestS3Storer_Conformance(t *testing.T) {
	t.Parallel()
	endpoint := startMinio(t)

	testStorerConformance(t, func(t *testing.T) conformanceFixture {
		bucket := randBucketName(t)
		s, err := NewS3(context.Background(), endpoint, bucket, "us-east-1",
			"minioadmin", "minioadmin", true)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		return conformanceFixture{
			name:   "s3",
			storer: s,
			readback: func(t *testing.T, key string) []byte {
				t.Helper()
				data, err := s.downloadForTest(context.Background(), key)
				if err != nil {
					t.Fatalf("downloadForTest %q: %v", key, err)
				}
				return data
			},
			urlAssertion: func(t *testing.T, key, url string) {
				if !strings.HasPrefix(url, endpoint) {
					t.Errorf("S3 URL %q missing endpoint %q", url, endpoint)
				}
				if !strings.Contains(url, bucket) {
					t.Errorf("S3 URL %q missing bucket %q", url, bucket)
				}
				if !strings.Contains(url, key) {
					t.Errorf("S3 URL %q missing key %q", url, key)
				}
			},
			// S3 PUTs are atomic at the object level. The conformance
			// suite has no atomicity case wired in.
			supportsAtomicity: false,
		}
	})
}

func TestNewS3_EndpointBranch(t *testing.T) {
	t.Parallel()
	endpoint := startMinio(t)
	s, err := NewS3(context.Background(), endpoint, randBucketName(t), "us-east-1",
		"minioadmin", "minioadmin", true)
	if err != nil {
		t.Fatalf("expected endpoint branch to construct, got %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil storer")
	}
}

func TestNewS3_EndpointRequiresCreds(t *testing.T) {
	t.Parallel()
	// Endpoint set but creds empty → must error before any network call.
	// (The default-chain branch is for prod, where creds come from env
	// or IAM role. With an endpoint set, we KNOW we mean static creds.)
	_, err := NewS3(context.Background(), "http://example.invalid:9000",
		"some-bucket", "us-east-1", "", "", true)
	if err == nil {
		t.Fatal("expected error when endpoint set but creds empty")
	}
	if !strings.Contains(err.Error(), "access_key_id") {
		t.Errorf("error should name credentials, got: %v", err)
	}
}

func TestNewS3_MissingBucket(t *testing.T) {
	t.Parallel()
	_, err := NewS3(context.Background(), "", "", "us-east-1", "", "", false)
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestNewS3_MissingRegion(t *testing.T) {
	t.Parallel()
	_, err := NewS3(context.Background(), "", "some-bucket", "", "", "", false)
	if err == nil {
		t.Fatal("expected error for missing region")
	}
}

func TestNewS3_IdempotentCreate(t *testing.T) {
	t.Parallel()
	endpoint := startMinio(t)
	bucket := randBucketName(t)
	if _, err := NewS3(context.Background(), endpoint, bucket, "us-east-1",
		"minioadmin", "minioadmin", true); err != nil {
		t.Fatalf("first construct: %v", err)
	}
	// Second construct against the same already-existing bucket must
	// succeed — confirms the BucketAlreadyOwnedByYou tolerance.
	if _, err := NewS3(context.Background(), endpoint, bucket, "us-east-1",
		"minioadmin", "minioadmin", true); err != nil {
		t.Fatalf("second construct (idempotent): %v", err)
	}
}

// Compile-time guard: S3Storer must implement both Storer and Cleaner.
var (
	_ Storer  = (*S3Storer)(nil)
	_ Cleaner = (*S3Storer)(nil)
)
