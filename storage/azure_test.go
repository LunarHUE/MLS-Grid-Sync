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

// Azurite's well-known devstoreaccount1 + key. Public; not a secret.
// The {{HOST}} / {{PORT}} placeholders are substituted at fixture
// build-time once the testcontainer reports its mapped port.
const azuriteConnStrTemplate = "DefaultEndpointsProtocol=http;" +
	"AccountName=devstoreaccount1;" +
	"AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;" +
	"BlobEndpoint=http://{{HOST}}:{{PORT}}/devstoreaccount1;"

// One Azurite container is shared by every test in the process — tests
// already isolate via randContainerName, so per-test containers buy
// nothing. The container is reaped by testcontainers' ryuk at process
// exit, so no Terminate is registered.
var (
	azuriteOnce sync.Once
	azuriteConn string
	azuriteErr  error
)

// startAzurite returns the connection string of the shared Azurite
// blob-only container, starting it on first use.
func startAzurite(t *testing.T) string {
	t.Helper()
	azuriteOnce.Do(func() {
		ctx := context.Background()

		// EMULATOR DIVERGENCE: the published `azurite:latest` tag trails the
		// azblob SDK's default API version (e.g. SDK sends 2026-04-06; the
		// image returns InvalidHeaderValue). `--skipApiVersionCheck` makes
		// Azurite accept any API version. Real Azure validates the version
		// for real, so REAL-CLOUD VALIDATION must pin the SDK to a version
		// the prod account supports — flagged here so this isn't forgotten
		// at deploy time. See plan watch items.
		ctr, err := testcontainers.Run(ctx,
			"mcr.microsoft.com/azure-storage/azurite:latest",
			testcontainers.WithExposedPorts("10000/tcp"),
			testcontainers.WithCmd("azurite-blob", "--blobHost", "0.0.0.0", "--skipApiVersionCheck"),
			testcontainers.WithWaitStrategy(
				wait.ForListeningPort("10000/tcp"),
			),
		)
		if err != nil {
			azuriteErr = fmt.Errorf("start azurite: %w", err)
			return
		}

		host, err := ctr.Host(ctx)
		if err != nil {
			azuriteErr = fmt.Errorf("azurite host: %w", err)
			return
		}
		port, err := ctr.MappedPort(ctx, "10000/tcp")
		if err != nil {
			azuriteErr = fmt.Errorf("azurite port: %w", err)
			return
		}
		conn := strings.ReplaceAll(azuriteConnStrTemplate, "{{HOST}}", host)
		azuriteConn = strings.ReplaceAll(conn, "{{PORT}}", port.Port())
	})
	if azuriteErr != nil {
		t.Fatalf("shared azurite container: %v", azuriteErr)
	}
	return azuriteConn
}

// randContainerName returns a fresh per-test-call container name.
// The conformance suite calls makeFixture per subtest, so each subtest
// gets its own container — matching LocalStorer's t.TempDir() semantic.
func randContainerName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "media-" + hex.EncodeToString(buf[:])
}

func TestAzureBlobStorer_Conformance(t *testing.T) {
	t.Parallel()
	conn := startAzurite(t)

	testStorerConformance(t, func(t *testing.T) conformanceFixture {
		container := randContainerName(t)
		s, err := NewAzureBlob(context.Background(), conn, "", container)
		if err != nil {
			t.Fatalf("NewAzureBlob: %v", err)
		}
		return conformanceFixture{
			name:   "azure",
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
				if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
					t.Errorf("AzureBlob URL %q missing http(s) prefix", url)
				}
				if !strings.Contains(url, container) {
					t.Errorf("AzureBlob URL %q missing container %q", url, container)
				}
				if !strings.Contains(url, key) {
					t.Errorf("AzureBlob URL %q missing key %q", url, key)
				}
			},
		}
	})
}

func TestNewAzureBlob_ConnectionStringBranch(t *testing.T) {
	t.Parallel()
	conn := startAzurite(t)
	s, err := NewAzureBlob(context.Background(), conn, "", randContainerName(t))
	if err != nil {
		t.Fatalf("expected conn-string branch to construct, got %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil storer")
	}
}

func TestNewAzureBlob_ConnectionStringTakesPrecedence(t *testing.T) {
	t.Parallel()
	// Both fields set: connection_string wins. If precedence reversed,
	// we'd try to use DefaultAzureCredential against the bogus
	// account_url and fail before container create.
	conn := startAzurite(t)
	bogusAccountURL := "https://this-account-does-not-exist.blob.core.windows.net"
	s, err := NewAzureBlob(context.Background(), conn, bogusAccountURL, randContainerName(t))
	if err != nil {
		t.Fatalf("expected conn-string to win, got %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil storer")
	}
}

func TestNewAzureBlob_BothEmpty(t *testing.T) {
	t.Parallel()
	_, err := NewAzureBlob(context.Background(), "", "", "container")
	if err == nil {
		t.Fatal("expected error when both connection_string and account_url empty")
	}
	if !strings.Contains(err.Error(), "connection_string") || !strings.Contains(err.Error(), "account_url") {
		t.Errorf("error should name both fields, got: %v", err)
	}
}

func TestNewAzureBlob_MissingContainer(t *testing.T) {
	t.Parallel()
	conn := startAzurite(t)
	_, err := NewAzureBlob(context.Background(), conn, "", "")
	if err == nil {
		t.Fatal("expected error for missing container")
	}
}

func TestNewAzureBlob_IdempotentCreate(t *testing.T) {
	t.Parallel()
	conn := startAzurite(t)
	container := randContainerName(t)
	if _, err := NewAzureBlob(context.Background(), conn, "", container); err != nil {
		t.Fatalf("first construct: %v", err)
	}
	// Second construct against the same already-existing container must
	// succeed — confirms the ContainerAlreadyExists tolerance.
	if _, err := NewAzureBlob(context.Background(), conn, "", container); err != nil {
		t.Fatalf("second construct (idempotent): %v", err)
	}
}

// Compile-time guard: AzureBlobStorer must implement both Storer and
// Cleaner. The matrix-wide cleanup subcommand depends on this.
var (
	_ Storer  = (*AzureBlobStorer)(nil)
	_ Cleaner = (*AzureBlobStorer)(nil)
)
