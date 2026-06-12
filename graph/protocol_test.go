package graph_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Wire-protocol tests: the HTTP/JSON contract documented in
// docs/graphql-api.md ("Making requests"). These pin status codes and
// error envelope shapes, not query semantics.

type rawEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

func rawPost(t *testing.T, url, body string) (int, rawEnvelope) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var env rawEnvelope
	require.NoError(t, json.Unmarshal(raw, &env), "non-JSON response: %s", raw)
	return resp.StatusCode, env
}

// Undecodable JSON → 400 with a JSON error envelope. Also guards the
// transactioner fix: without guardedTransactioner this path panics
// inside entgql and surfaces as "internal system error".
func TestProtocol_UndecodableBody(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	status, env := rawPost(t, srv.URL, `{not json`)
	assert.Equal(t, http.StatusBadRequest, status)
	require.NotEmpty(t, env.Errors)
	assert.NotEqual(t, "internal system error", env.Errors[0].Message,
		"transactioner panicked on a request with no operation context")
}

// Unparseable GraphQL → 422 + GRAPHQL_PARSE_FAILED.
func TestProtocol_ParseError(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	status, env := rawPost(t, srv.URL, `{"query":"{ nope"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	require.NotEmpty(t, env.Errors)
	assert.Equal(t, "GRAPHQL_PARSE_FAILED", env.Errors[0].Extensions.Code)
}

// Valid syntax, unknown field → 422 + GRAPHQL_VALIDATION_FAILED.
func TestProtocol_ValidationError(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	status, env := rawPost(t, srv.URL, `{"query":"{ doesNotExist { id } }"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	require.NotEmpty(t, env.Errors)
	assert.Equal(t, "GRAPHQL_VALIDATION_FAILED", env.Errors[0].Extensions.Code)
}

// Resolver-level failure → HTTP 200; the error lives in the envelope.
func TestProtocol_ResolverErrorIs200(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	status, env := rawPost(t, srv.URL,
		`{"query":"{ propertiesNear(center:{latitude:99,longitude:0}, radiusMeters:10, first:1){totalCount} }"}`)
	assert.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, env.Errors)
	assert.Contains(t, env.Errors[0].Message, "out of range")
}

// GET transport: query in the URL query string.
func TestProtocol_GETTransport(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	resp, err := http.Get(srv.URL + "?query=" + url.QueryEscape(`{ sourceSystems(first:1){ totalCount } }`))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"data":{"sourceSystems":{"totalCount":0}}}`, string(raw))
}

// operationName selects between multiple operations in one document.
func TestProtocol_OperationName(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedLookup(t, client, "lkp-op", true)

	status, env := rawPost(t, srv.URL, `{
		"query": "query A($n: Int){ lookups(first:$n){totalCount} } query B { offices(first:1){totalCount} }",
		"operationName": "A",
		"variables": {"n": 1}
	}`)
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, env.Errors)
	assert.JSONEq(t, `{"lookups":{"totalCount":1}}`, string(env.Data))
}
