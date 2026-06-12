package mls_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/mls"
)

func odata(t *testing.T, records []any, nextLink string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"value": records, "@odata.nextLink": nextLink})
	require.NoError(t, err)
	return b
}

func TestFetchPage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(odata(t, []any{
			map[string]any{"ListingKey": "abc123", "ListPrice": 500000},
			map[string]any{"ListingKey": "def456", "ListPrice": 750000},
		}, ""))
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("test-token", srv.URL)
	resp, err := client.FetchPage(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Len(t, resp.Value, 2)
	assert.Empty(t, resp.NextLink)
}

func TestFetchPage_SendsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(odata(t, nil, ""))
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("my-secret-token", srv.URL)
	_, err := client.FetchPage(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "Bearer my-secret-token", gotAuth)
}

func TestFetchPage_RetryOnTransientError(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(odata(t, []any{map[string]any{"ListingKey": "x1"}}, ""))
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	resp, err := client.FetchPage(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Len(t, resp.Value, 1)
	assert.Equal(t, int32(3), attempts.Load(), "expected exactly 3 attempts")
}

func TestFetchPage_FailsAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	_, err := client.FetchPage(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Equal(t, int32(3), attempts.Load())
}

func TestFetchPage_NextLink(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write(odata(t, []any{map[string]any{"ListingKey": "p1"}}, "NEXT_PAGE_URL"))
			return
		}
		_, _ = w.Write(odata(t, []any{map[string]any{"ListingKey": "p2"}}, ""))
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	resp, err := client.FetchPage(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "NEXT_PAGE_URL", resp.NextLink, "nextLink should be passed through")
}

// TestFetchPage_429_RetryAfterInteger pins the courtesy-retry behavior:
// a 429 with Retry-After is followed by a successful response without
// consuming the transient-error budget.
func TestFetchPage_429_RetryAfterInteger(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(odata(t, []any{map[string]any{"ListingKey": "ok"}}, ""))
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	resp, err := client.FetchPage(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Len(t, resp.Value, 1)
	assert.Equal(t, int32(2), attempts.Load(), "429 + success = 2 attempts")
}

// TestFetchPage_429_DoesNotConsumeBudget is the load-bearing regression:
// the pre-fix client treated 429 like any other failure, so 3 errors in
// a row (some 429, some 5xx) gave up. With 429 carved out of the
// transient budget, 2x 503 + 1x 429 + 200 must succeed.
func TestFetchPage_429_DoesNotConsumeBudget(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		switch n {
		case 1, 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 3:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(odata(t, []any{map[string]any{"ListingKey": "ok"}}, ""))
		}
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	resp, err := client.FetchPage(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Len(t, resp.Value, 1)
	assert.Equal(t, int32(4), attempts.Load(), "2x 503 + 429 + 200 = 4 attempts (429 is a courtesy retry, not a budget hit)")
}

// TestFetchPage_429_ConsecutiveCapGivesUp ensures we don't loop forever
// if the server is stuck returning 429. The final error surfaces the 429.
func TestFetchPage_429_ConsecutiveCapGivesUp(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	_, err := client.FetchPage(context.Background(), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429", "final error should surface the 429 status")
	// 5 courtesy retries + 1 final attempt that breaks the loop = 6 total.
	assert.Equal(t, int32(6), attempts.Load(), "consecutive-429 cap should bound retries")
}

func TestFetchPage_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(odata(t, nil, ""))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	client := mls.NewClientWithURL("token", srv.URL)
	_, err := client.FetchPage(ctx, srv.URL)
	require.Error(t, err)
}
