package storage

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestFakeStorer_Conformance(t *testing.T) {
	t.Parallel()
	testStorerConformance(t, func(t *testing.T) conformanceFixture {
		return conformanceFixture{
			name:   "fake",
			storer: &FakeStorer{},
			urlAssertion: func(t *testing.T, key, url string) {
				const want = "https://fake-s3.local/"
				if !strings.HasPrefix(url, want) {
					t.Errorf("FakeStorer URL %q missing prefix %q", url, want)
				}
				if !strings.Contains(url, key) {
					t.Errorf("FakeStorer URL %q missing key %q", url, key)
				}
			},
			// Fake discards bytes — no readback, no atomicity guarantee.
		}
	})
}

// TestFakeStorer_RejectsBadKeys pins the parity fix: the fake enforces the
// same key contract as LocalStorer (see TestLocalStorer_PathTraversalRejected)
// so swapping it in for a real backend doesn't silently accept keys a real
// filesystem backend would reject.
func TestFakeStorer_RejectsBadKeys(t *testing.T) {
	t.Parallel()
	bad := []string{"", "/abs/key", "../escape", "media/../../etc/passwd"}
	for _, key := range bad {
		t.Run(key, func(t *testing.T) {
			_, err := (&FakeStorer{}).Upload(context.Background(), key, bytes.NewReader([]byte("x")), "text/plain")
			if err == nil {
				t.Errorf("FakeStorer.Upload(%q) = nil error, want rejection", key)
			}
		})
	}
}
