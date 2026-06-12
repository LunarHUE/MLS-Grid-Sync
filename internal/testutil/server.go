package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/graph"
)

// NewTestServer creates a real PostgreSQL test DB and an httptest.Server wrapping
// the GraphQL handler. Both are torn down via t.Cleanup automatically.
func NewTestServer(t *testing.T) (*httptest.Server, *ent.Client) {
	t.Helper()
	client := NewTestDB(t)
	srv := httptest.NewServer(graph.NewHandler(client))
	t.Cleanup(srv.Close)
	return srv, client
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   json.RawMessage  `json:"data"`
	Errors []map[string]any `json:"errors,omitempty"`
}

// GQL posts a GraphQL query to srv and unmarshals the "data" field into out.
// It fails the test on HTTP errors, non-200 status, or GraphQL errors.
func GQL(t *testing.T, srv *httptest.Server, query string, variables map[string]any, out any) {
	t.Helper()

	body, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	require.NoError(t, err)

	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "unexpected HTTP status: %s", raw)

	var gqlResp gqlResponse
	require.NoError(t, json.Unmarshal(raw, &gqlResp))
	require.Empty(t, gqlResp.Errors, "graphql errors: %v", gqlResp.Errors)

	if out != nil {
		require.NoError(t, json.Unmarshal(gqlResp.Data, out))
	}
}

// GQLCtx is like GQL but uses a provided context for the HTTP request.
func GQLCtx(t *testing.T, ctx context.Context, srv *httptest.Server, query string, variables map[string]any, out any) {
	t.Helper()

	body, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "unexpected HTTP status: %s", raw)

	var gqlResp gqlResponse
	require.NoError(t, json.Unmarshal(raw, &gqlResp))
	require.Empty(t, gqlResp.Errors, "graphql errors: %v", gqlResp.Errors)

	if out != nil {
		require.NoError(t, json.Unmarshal(gqlResp.Data, out))
	}
}
