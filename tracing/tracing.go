// Package tracing gives every HTTP request a stable, propagated trace ID so a
// single line in the console can be followed from the HTTP layer through the
// GraphQL operation that handled it.
//
// It speaks W3C Trace Context (the `traceparent` header) so a trace ID supplied
// by an upstream caller (gateway, browser instrumentation, another service) is
// reused; when none is supplied a fresh one is generated. Either way the chosen
// trace ID is echoed back on the response (both `traceparent` and the
// human-friendly `X-Trace-Id`) so the caller can correlate, and is stashed in
// the request context for downstream handlers via FromContext / TraceID.
//
// This is deliberately a lightweight, dependency-free implementation rather than
// the full OpenTelemetry SDK: it produces OTel-compatible IDs and propagation
// without requiring a collector, and can be swapped for the SDK later without
// changing call sites that only read TraceID(ctx).
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/lunarhue/libs-go/log"
)

// Header names. HeaderTraceparent is the W3C standard; HeaderTraceID is a
// convenience echo of just the 32-hex trace ID for humans and `curl`.
const (
	HeaderTraceparent = "traceparent"
	HeaderTraceID     = "X-Trace-Id"
)

// Span carries the trace identifiers for the current request. SpanID is this
// server's span; ParentID is the inbound caller's span (empty when this
// process started the trace).
type Span struct {
	TraceID  string // 32 lowercase hex
	SpanID   string // 16 lowercase hex
	ParentID string // 16 lowercase hex, or "" if this process is the root
	Sampled  bool
}

// Traceparent renders the span as a W3C traceparent header value.
func (s Span) Traceparent() string {
	flags := "00"
	if s.Sampled {
		flags = "01"
	}
	return "00-" + s.TraceID + "-" + s.SpanID + "-" + flags
}

type ctxKey struct{}

// FromContext returns the Span attached to ctx, if any.
func FromContext(ctx context.Context) (Span, bool) {
	s, ok := ctx.Value(ctxKey{}).(Span)
	return s, ok
}

// TraceID returns the trace ID attached to ctx, or "" when untraced. Handlers
// and resolvers use it to tag their own log lines with the request's trace.
func TraceID(ctx context.Context) string {
	if s, ok := FromContext(ctx); ok {
		return s.TraceID
	}
	return ""
}

// WithSpan returns a copy of ctx carrying span. Exported so non-HTTP entry
// points (CLI, tests) can seed a trace.
func WithSpan(ctx context.Context, span Span) context.Context {
	return context.WithValue(ctx, ctxKey{}, span)
}

// statusRecorder captures the response status code (default 200 if the handler
// never calls WriteHeader) so the completion log can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Middleware assigns/propagates a trace ID for each request, echoes it on the
// response, stores it in the request context, and logs request completion
// (method, path, status, latency, trace ID) to the console.
//
// Health-probe paths (/healthz, /readyz, /syncz) are logged at debug level to
// keep frequent load-balancer polling from drowning out real traffic; every
// other path is logged at request level.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := spanFromRequest(r)

		// Echo before invoking next so the headers are present even if the
		// handler streams or writes early.
		w.Header().Set(HeaderTraceparent, span.Traceparent())
		w.Header().Set(HeaderTraceID, span.TraceID)

		ctx := WithSpan(r.Context(), span)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		elapsed := time.Since(start)

		logf := log.Requestf
		if isHealthPath(r.URL.Path) {
			logf = log.Debugf
		}
		logf("trace=%s %d %s %s (%s)", span.TraceID, rec.status, r.Method, r.URL.Path, elapsed.Round(time.Microsecond))
	})
}

// spanFromRequest derives this request's span: it reuses an inbound trace ID
// from `traceparent` (or, failing that, a bare 32-hex `X-Trace-Id`), generating
// one when neither is present, and always mints a fresh span ID for this hop.
func spanFromRequest(r *http.Request) Span {
	if parent, ok := parseTraceparent(r.Header.Get(HeaderTraceparent)); ok {
		return Span{
			TraceID:  parent.TraceID,
			SpanID:   newID(8),
			ParentID: parent.SpanID,
			Sampled:  parent.Sampled,
		}
	}
	if tid, ok := normalizeTraceID(r.Header.Get(HeaderTraceID)); ok {
		return Span{TraceID: tid, SpanID: newID(8), Sampled: true}
	}
	return Span{TraceID: newID(16), SpanID: newID(8), Sampled: true}
}

// parseTraceparent parses a W3C `traceparent` value
// (version-traceid-spanid-flags) and reports whether it was well formed. Only
// the "00" version is recognized; an all-zero trace or span ID is rejected per
// the spec.
func parseTraceparent(v string) (Span, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 {
		return Span{}, false
	}
	version, traceID, spanID, flags := parts[0], strings.ToLower(parts[1]), strings.ToLower(parts[2]), parts[3]
	if version != "00" {
		return Span{}, false
	}
	if len(traceID) != 32 || !isHex(traceID) || traceID == strings.Repeat("0", 32) {
		return Span{}, false
	}
	if len(spanID) != 16 || !isHex(spanID) || spanID == strings.Repeat("0", 16) {
		return Span{}, false
	}
	if len(flags) != 2 || !isHex(flags) {
		return Span{}, false
	}
	sampled := flags[1]%2 == 1 // low bit of the flags byte
	return Span{TraceID: traceID, SpanID: spanID, Sampled: sampled}, true
}

// normalizeTraceID accepts a bare 32-hex trace ID (the X-Trace-Id fallback),
// lower-casing it; anything else is rejected so a malformed header can't poison
// the trace.
func normalizeTraceID(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) != 32 || !isHex(v) || v == strings.Repeat("0", 32) {
		return "", false
	}
	return v, true
}

// newID returns n random bytes as lowercase hex (16 hex chars per 8 bytes).
// crypto/rand.Read never returns an error on the platforms we target.
func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}

func isHealthPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/syncz":
		return true
	default:
		return false
	}
}
