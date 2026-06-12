package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/mls"
)

func odataPayload(t *testing.T, records ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"value":           records,
		"@odata.nextLink": "",
	})
	require.NoError(t, err)
	return b
}

func TestProbeOriginatingSystems_ExtractsDistinctNames(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		// Sanity: the probe must NOT carry an OriginatingSystemName
		// filter — that's the entire point of discovery.
		assert.NotContains(t, r.URL.RawQuery, "OriginatingSystemName")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(odataPayload(t,
			map[string]any{"LookupKey": "k1", "OriginatingSystemName": "actris"},
			map[string]any{"LookupKey": "k2", "OriginatingSystemName": "flinthills"},
			map[string]any{"LookupKey": "k3", "OriginatingSystemName": "actris"}, // dup
			map[string]any{"LookupKey": "k4"},                                    // missing field
		))
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("my-token", srv.URL)
	got, err := probeOriginatingSystems(context.Background(), client, srv.URL)
	require.NoError(t, err)

	// Distinct, sorted.
	assert.Equal(t, []string{"actris", "flinthills"}, got)
	// Probe MUST go through mls.Client — auth header confirms.
	assert.Equal(t, "Bearer my-token", gotAuth, "regression: probe must ride mls.Client (auth, rate limit, retries)")
}

func TestProbeOriginatingSystems_UpstreamRejectsSurfacesError(t *testing.T) {
	rejection := `{"error":{"code":"OriginatingSystemName required","message":"this token cannot enumerate systems"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(rejection))
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	_, err := probeOriginatingSystems(context.Background(), client, srv.URL)
	require.Error(t, err)
	// The body must surface verbatim so the operator can read what MLS
	// actually said.
	assert.Contains(t, err.Error(), "OriginatingSystemName required")
	assert.Contains(t, err.Error(), "this token cannot enumerate")
}

func TestProbeOriginatingSystems_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(odataPayload(t)) // no records
	}))
	defer srv.Close()

	client := mls.NewClientWithURL("token", srv.URL)
	got, err := probeOriginatingSystems(context.Background(), client, srv.URL)
	require.NoError(t, err, "an empty response is a valid 'probe worked, no names' outcome")
	assert.Empty(t, got)
}
