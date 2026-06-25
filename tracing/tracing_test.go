package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureHandler records the trace ID it sees in the request context.
func captureHandler(seen *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = TraceID(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})
}

func TestMiddleware_GeneratesTraceIDWhenAbsent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(Middleware(captureHandler(&seen)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything")
	require.NoError(t, err)
	resp.Body.Close()

	// Generated, propagated into context, and echoed on both headers.
	require.Len(t, seen, 32)
	assert.Equal(t, seen, resp.Header.Get(HeaderTraceID))
	tp, ok := parseTraceparent(resp.Header.Get(HeaderTraceparent))
	require.True(t, ok, "echoed traceparent must be well-formed")
	assert.Equal(t, seen, tp.TraceID)
}

func TestMiddleware_ReusesInboundTraceparent(t *testing.T) {
	var seen string
	srv := httptest.NewServer(Middleware(captureHandler(&seen)))
	defer srv.Close()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const parentSpan = "00f067aa0ba902b7"
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set(HeaderTraceparent, "00-"+traceID+"-"+parentSpan+"-01")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// Inbound trace ID is reused; a fresh span ID is minted for this hop.
	assert.Equal(t, traceID, seen)
	assert.Equal(t, traceID, resp.Header.Get(HeaderTraceID))
	tp, ok := parseTraceparent(resp.Header.Get(HeaderTraceparent))
	require.True(t, ok)
	assert.Equal(t, traceID, tp.TraceID)
	assert.NotEqual(t, parentSpan, tp.SpanID, "this hop must mint its own span ID")
}

func TestMiddleware_AcceptsBareXTraceID(t *testing.T) {
	var seen string
	srv := httptest.NewServer(Middleware(captureHandler(&seen)))
	defer srv.Close()

	const traceID = "abcdef0123456789abcdef0123456789"
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set(HeaderTraceID, traceID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, traceID, seen)
}

func TestMiddleware_MalformedTraceparentIsReplaced(t *testing.T) {
	var seen string
	srv := httptest.NewServer(Middleware(captureHandler(&seen)))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set(HeaderTraceparent, "garbage-not-a-traceparent")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// A bad header must not poison the trace; a fresh valid ID is generated.
	require.Len(t, seen, 32)
	assert.True(t, isHex(seen))
}

func TestParseTraceparent(t *testing.T) {
	good, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	require.True(t, ok)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", good.TraceID)
	assert.Equal(t, "00f067aa0ba902b7", good.SpanID)
	assert.True(t, good.Sampled)

	notSampled, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	require.True(t, ok)
	assert.False(t, notSampled.Sampled)

	bad := []string{
		"",
		"not-a-header",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // unsupported version
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // all-zero trace
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", // all-zero span
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",    // missing flags
		"00-XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX-00f067aa0ba902b7-01", // non-hex trace
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz", // non-hex flags
	}
	for _, v := range bad {
		_, ok := parseTraceparent(v)
		assert.False(t, ok, v)
	}
}

func TestTraceID_EmptyWhenUntraced(t *testing.T) {
	assert.Equal(t, "", TraceID(context.Background()))
}

func TestSpan_Traceparent(t *testing.T) {
	s := Span{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Sampled: true}
	assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", s.Traceparent())
	s.Sampled = false
	assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", s.Traceparent())
}
