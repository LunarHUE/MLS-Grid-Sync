package cmd

import (
	"strings"
	"testing"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/config"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/storage"
)

func TestNewStorer_FakeDefault(t *testing.T) {
	// Empty backend → "fake" default (defends the existing test wiring).
	s, err := newStorer(config.StorageConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := s.(*storage.FakeStorer); !ok {
		t.Errorf("expected *FakeStorer, got %T", s)
	}
}

func TestNewStorer_FakeExplicit(t *testing.T) {
	s, err := newStorer(config.StorageConfig{Backend: "fake"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := s.(*storage.FakeStorer); !ok {
		t.Errorf("expected *FakeStorer, got %T", s)
	}
}

func TestNewStorer_Local(t *testing.T) {
	root := t.TempDir()
	s, err := newStorer(config.StorageConfig{
		Backend: "local",
		Local:   config.LocalStorageConfig{RootDir: root, CapBytes: 1 << 20},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := s.(*storage.LocalStorer); !ok {
		t.Errorf("expected *LocalStorer, got %T", s)
	}
}

func TestNewStorer_LocalRequiresRootDir(t *testing.T) {
	_, err := newStorer(config.StorageConfig{
		Backend: "local",
		Local:   config.LocalStorageConfig{CapBytes: 1 << 20},
	})
	if err == nil {
		t.Fatal("expected error for missing root_dir")
	}
}

func TestNewStorer_LocalRequiresCap(t *testing.T) {
	_, err := newStorer(config.StorageConfig{
		Backend: "local",
		Local:   config.LocalStorageConfig{RootDir: t.TempDir()},
	})
	if err == nil {
		t.Fatal("expected error for missing cap_bytes")
	}
}

// TestNewStorer_AzureRequiresAuth structurally asserts that the azure
// arm of newStorer hits the constructor and the constructor's
// validation fires — no container required. Both connection_string
// and account_url empty → clear error from NewAzureBlob.
func TestNewStorer_AzureRequiresAuth(t *testing.T) {
	_, err := newStorer(config.StorageConfig{
		Backend: "azure",
		Azure:   config.AzureStorageConfig{Container: "media"},
	})
	if err == nil {
		t.Fatal("expected error from azure constructor (no auth fields)")
	}
	if !strings.Contains(err.Error(), "connection_string") && !strings.Contains(err.Error(), "account_url") {
		t.Errorf("error should name azure auth fields, got: %v", err)
	}
}

// TestNewStorer_AzureRequiresContainer structurally asserts the
// constructor's container-required validation.
func TestNewStorer_AzureRequiresContainer(t *testing.T) {
	_, err := newStorer(config.StorageConfig{
		Backend: "azure",
		Azure:   config.AzureStorageConfig{ConnectionString: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "container") {
		t.Errorf("expected container-required error, got %v", err)
	}
}

// TestNewStorer_S3RequiresBucket structurally asserts the s3 arm
// hits its constructor.
func TestNewStorer_S3RequiresBucket(t *testing.T) {
	_, err := newStorer(config.StorageConfig{Backend: "s3"})
	if err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Errorf("expected bucket-required error, got %v", err)
	}
}

// TestNewStorer_S3EndpointRequiresCreds structurally asserts the
// endpoint branch's creds validation. Real endpoint not contacted.
func TestNewStorer_S3EndpointRequiresCreds(t *testing.T) {
	_, err := newStorer(config.StorageConfig{
		Backend: "s3",
		S3: config.S3StorageConfig{
			Endpoint: "http://example.invalid:9000",
			Bucket:   "media",
			Region:   "us-east-1",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "access_key_id") {
		t.Errorf("expected creds-required error, got %v", err)
	}
}

func TestNewStorer_UnknownBackend(t *testing.T) {
	_, err := newStorer(config.StorageConfig{Backend: "gluster"})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected unknown-backend error, got %v", err)
	}
}
