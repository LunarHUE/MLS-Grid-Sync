package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startMinio spins up a MinIO container and returns the
// http://host:port endpoint pointed at the mapped port. Cleanup is
// registered on t.
func startMinio(t *testing.T) string {
	t.Helper()
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
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			t.Logf("terminate minio: %v", err)
		}
	})

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("minio host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("minio port: %v", err)
	}
	return "http://" + host + ":" + port.Port()
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
	_, err := NewS3(context.Background(), "", "", "us-east-1", "", "", false)
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestNewS3_MissingRegion(t *testing.T) {
	_, err := NewS3(context.Background(), "", "some-bucket", "", "", "", false)
	if err == nil {
		t.Fatal("expected error for missing region")
	}
}

func TestNewS3_IdempotentCreate(t *testing.T) {
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
