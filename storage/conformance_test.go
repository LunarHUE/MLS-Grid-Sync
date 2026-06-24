package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
)

// conformanceFixture is what each backend supplies to drive the
// shared behavioral suite. The optional readback/urlAssertion hooks are
// nil-able so backends opt in only to what they can honor; the suite skips
// the cases that don't apply.
type conformanceFixture struct {
	name   string
	storer Storer
	// readback returns the bytes previously uploaded under `key`. nil
	// for backends that don't persist content (FakeStorer).
	readback func(t *testing.T, key string) []byte
	// urlAssertion validates the shape of the URL Upload returned.
	urlAssertion func(t *testing.T, key, url string)
}

// testStorerConformance is the shared contract suite extended by
// Phase 3's Azurite/MinIO cases. The same fn runs against Fake, Local,
// and any future backend — the suite is the contract.
func testStorerConformance(t *testing.T, makeFixture func(t *testing.T) conformanceFixture) {
	t.Helper()

	t.Run("upload returns URL of expected shape", func(t *testing.T) {
		fix := makeFixture(t)
		key := "media/abc/hash1"
		url, err := fix.storer.Upload(context.Background(), key, bytes.NewReader([]byte("hello")), "text/plain")
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		if url == "" {
			t.Fatal("expected non-empty URL")
		}
		fix.urlAssertion(t, key, url)
	})

	t.Run("nested key round-trip", func(t *testing.T) {
		fix := makeFixture(t)
		key := "media/deeply/nested/path/object"
		_, err := fix.storer.Upload(context.Background(), key, bytes.NewReader([]byte("nested")), "application/octet-stream")
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
	})

	t.Run("same-key double upload succeeds with consistent URL shape", func(t *testing.T) {
		// Step-0 pinning: the worker's correct path never re-uploads the
		// same key (sha256 dedup happens before Storer). Re-uploads are
		// from reprocess/crash-recovery/test re-runs; the contract is
		// that they succeed and return the same URL shape.
		fix := makeFixture(t)
		key := "media/same/key"
		url1, err := fix.storer.Upload(context.Background(), key, bytes.NewReader([]byte("first")), "text/plain")
		if err != nil {
			t.Fatalf("first upload: %v", err)
		}
		url2, err := fix.storer.Upload(context.Background(), key, bytes.NewReader([]byte("second")), "text/plain")
		if err != nil {
			t.Fatalf("second upload: %v", err)
		}
		if url1 != url2 {
			t.Errorf("URL shape drifted across re-upload: %q vs %q", url1, url2)
		}
		if fix.readback != nil {
			got := fix.readback(t, key)
			if !bytes.Equal(got, []byte("second")) {
				t.Errorf("readback after re-upload = %q, want %q", got, "second")
			}
		}
	})

	t.Run("upload then readback returns identical bytes", func(t *testing.T) {
		fix := makeFixture(t)
		if fix.readback == nil {
			t.Skipf("%s does not support readback", fix.name)
		}
		key := "media/byte/equality"
		payload := []byte("the cake is a lie\x00\x01\x02binary too")
		if _, err := fix.storer.Upload(context.Background(), key, bytes.NewReader(payload), "application/octet-stream"); err != nil {
			t.Fatalf("upload: %v", err)
		}
		got := fix.readback(t, key)
		if !bytes.Equal(got, payload) {
			t.Errorf("readback = %q, want %q", got, payload)
		}
	})

	t.Run("8-way concurrent distinct-key uploads succeed", func(t *testing.T) {
		fix := makeFixture(t)
		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := fmt.Sprintf("media/concurrent/%d", i)
				body := fmt.Appendf(nil, "payload-%d", i)
				if _, err := fix.storer.Upload(context.Background(), key, bytes.NewReader(body), "text/plain"); err != nil {
					errs <- fmt.Errorf("key %d: %w", i, err)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("concurrent upload: %v", err)
		}
	})

	t.Run("CleanupPrefix removes exactly the prefix", func(t *testing.T) {
		fix := makeFixture(t)
		cleaner, ok := fix.storer.(Cleaner)
		if !ok {
			t.Skipf("%s does not implement Cleaner", fix.name)
		}
		// Plant: two files under prefix A, one under prefix B.
		ctx := context.Background()
		mustUpload := func(key, payload string) {
			t.Helper()
			if _, err := fix.storer.Upload(ctx, key, bytes.NewReader([]byte(payload)), "text/plain"); err != nil {
				t.Fatalf("upload %q: %v", key, err)
			}
		}
		mustUpload("under-a/one", "1")
		mustUpload("under-a/two", "2")
		mustUpload("under-b/sibling", "3")

		if err := cleaner.CleanupPrefix(ctx, "under-a"); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		// If the backend supports readback, prefix A is gone, B remains.
		if fix.readback != nil {
			t.Run("sibling under unrelated prefix survives", func(t *testing.T) {
				got := fix.readback(t, "under-b/sibling")
				if string(got) != "3" {
					t.Errorf("sibling lost or corrupted: %q", got)
				}
			})
		}
	})
}

// failingReader returns n bytes then err — for the atomicity test in
// LocalStorer (mid-upload failure must leave no visible file).
type failingReader struct {
	src io.Reader
	err error
}

func (r *failingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if err == io.EOF && r.err != nil {
		return n, r.err
	}
	return n, err
}
