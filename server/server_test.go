package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/health"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/server"
)

const queryBody = `{"query":"{ sourceSystems(first:1){ totalCount } }"}`

func okPing(context.Context) error  { return nil }
func badPing(context.Context) error { return errors.New("down") }

func testThresholds() health.Thresholds {
	return health.Thresholds{SyncMaxStaleness: 30 * time.Minute, MaxRawPending: 10000, MaxAttachmentFailures: 100}
}

// newHarness wires a real test DB into a NewMux-backed httptest server.
func newHarness(t *testing.T, opts server.Options, ping func(context.Context) error) *httptest.Server {
	t.Helper()
	client := testutil.NewTestDB(t)
	hsvc := health.NewService(client, ping, testThresholds(), time.Now)
	srv := httptest.NewServer(server.NewMux(client, hsvc, opts))
	t.Cleanup(srv.Close)
	return srv
}

func postQuery(t *testing.T, url string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/query", strings.NewReader(queryBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, string(body)
}

// ---- API key ----

func TestAPIKey_DisabledAllowsAnonymous(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{}, okPing)
	resp, body := postQuery(t, srv.URL, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
}

func TestAPIKey_MissingRejected401(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)
	resp, body := postQuery(t, srv.URL, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, body, "UNAUTHENTICATED")
}

func TestAPIKey_XAPIKeyHeaderAccepted(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)
	resp, body := postQuery(t, srv.URL, map[string]string{"X-API-Key": "secret"})
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
}

func TestAPIKey_BearerAccepted(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)

	resp, body := postQuery(t, srv.URL, map[string]string{"Authorization": "Bearer secret"})
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)

	// Prefix match is case-insensitive.
	resp2, body2 := postQuery(t, srv.URL, map[string]string{"Authorization": "bearer secret"})
	assert.Equal(t, http.StatusOK, resp2.StatusCode, body2)
}

func TestAPIKey_WrongKeyRejected(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)
	resp, _ := postQuery(t, srv.URL, map[string]string{"X-API-Key": "nope"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKey_GETTransportEnforced(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)
	resp, err := http.Get(srv.URL + "/query?query=" + "%7BsourceSystems(first%3A1)%7BtotalCount%7D%7D")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAPIKey_HealthzStaysOpen(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// /healthz is process-only: a down DB must NOT flip it to 503.
	srvBad := newHarness(t, server.Options{APIKey: "secret"}, badPing)
	respBad, err := http.Get(srvBad.URL + "/healthz")
	require.NoError(t, err)
	respBad.Body.Close()
	assert.Equal(t, http.StatusOK, respBad.StatusCode)
}

// ---- health endpoints ----

func getJSON(t *testing.T, url string) (*http.Response, health.HealthStatus) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	var hs health.HealthStatus
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&hs))
	resp.Body.Close()
	return resp, hs
}

func TestHealthz_ProcessOnly(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{}, badPing)
	resp, hs := getJSON(t, srv.URL+"/healthz")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, hs.Healthy)
	require.Len(t, hs.Checks, 1)
	assert.Equal(t, "process", hs.Checks[0].Name)
}

func TestReadyz_DBUpDown(t *testing.T) {
	t.Parallel()
	srvUp := newHarness(t, server.Options{}, okPing)
	respUp, hsUp := getJSON(t, srvUp.URL+"/readyz")
	assert.Equal(t, http.StatusOK, respUp.StatusCode)
	assert.True(t, hsUp.Healthy)

	srvDown := newHarness(t, server.Options{}, badPing)
	respDown, hsDown := getJSON(t, srvDown.URL+"/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, respDown.StatusCode)
	assert.False(t, hsDown.Healthy)
}

func TestSyncz_FreshDeployUnhealthy(t *testing.T) {
	t.Parallel()
	// A migrated-but-unsynced DB has no successful fetches → 503 with a stable
	// JSON body the operator can parse.
	srv := newHarness(t, server.Options{}, okPing)
	resp, hs := getJSON(t, srv.URL+"/syncz")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.False(t, hs.Healthy)
	assert.NotEmpty(t, hs.Checks)
}

func TestHealthEndpoints_StayOpenWithAPIKey(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)
	for _, path := range []string{"/healthz", "/readyz", "/syncz"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		// Open by construction (only /query is behind the API key), so never 401.
		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode, path)
	}
}

func TestAPIKey_PlaygroundStaysOpen(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)
	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, strings.ToLower(string(body)), "<html")
}

// ---- CORS ----

func TestCORS_DefaultWildcard(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{}, okPing)
	resp, _ := postQuery(t, srv.URL, map[string]string{"Origin": "https://example.com"})
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
}

func TestCORS_AllowlistedOriginEchoed(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{AllowedOrigins: []string{"https://app.example.com"}}, okPing)
	resp, _ := postQuery(t, srv.URL, map[string]string{"Origin": "https://app.example.com"})
	assert.Equal(t, "https://app.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, resp.Header.Values("Vary"), "Origin")
}

func TestCORS_DisallowedOriginNoHeader(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{AllowedOrigins: []string{"https://app.example.com"}}, okPing)
	resp, _ := postQuery(t, srv.URL, map[string]string{"Origin": "https://evil.example.com"})
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, resp.Header.Values("Vary"), "Origin")
}

func TestCORS_PreflightBypassesAPIKey(t *testing.T) {
	t.Parallel()
	srv := newHarness(t, server.Options{APIKey: "secret"}, okPing)

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/query", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "X-API-Key")
}

// ---- SplitOrigins ----

func TestSplitOrigins(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{"*", []string{"*"}},
		{"a, b", []string{"a", "b"}},
		{"", nil},
		{"a,,b,", []string{"a", "b"}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, server.SplitOrigins(tt.in), "SplitOrigins(%q)", tt.in)
	}
}
