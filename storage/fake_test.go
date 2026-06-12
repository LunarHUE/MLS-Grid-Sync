package storage

import (
	"strings"
	"testing"
)

func TestFakeStorer_Conformance(t *testing.T) {
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
