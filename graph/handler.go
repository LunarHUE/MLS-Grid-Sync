package graph

import (
	"context"
	"net/http"
	"time"

	"entgo.io/contrib/entgql"
	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/lunarhue/libs-go/log"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/tracing"
)

// guardedTransactioner wraps entgql.Transactioner to skip its response
// interceptor when no operation context exists — i.e. requests that die
// before parsing (undecodable JSON bodies). The raw Transactioner calls
// graphql.GetOperationContext unconditionally there, which panics;
// gqlgen recovers it, but the client gets an opaque "internal system
// error" and the log gets a stack trace.
type guardedTransactioner struct{ entgql.Transactioner }

func (g guardedTransactioner) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	if !graphql.HasOperationContext(ctx) {
		return next(ctx)
	}
	return g.Transactioner.InterceptResponse(ctx, next)
}

// traceLogger logs one line per GraphQL operation — the request's trace ID,
// the operation name, an ok/errors verdict, and the duration — so a console
// reader can follow a request from the HTTP completion log (same trace ID) into
// the operation that handled it. The trace ID comes from the context the
// tracing.Middleware populated; it is "" when the handler is used without that
// middleware (e.g. unit tests hitting the handler directly).
type traceLogger struct{}

func (traceLogger) ExtensionName() string                   { return "TraceLogging" }
func (traceLogger) Validate(graphql.ExecutableSchema) error { return nil }

func (traceLogger) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	// Requests that die before parsing (undecodable body) have no operation
	// context; nothing to log, and GetOperationContext would panic.
	if !graphql.HasOperationContext(ctx) {
		return next(ctx)
	}
	oc := graphql.GetOperationContext(ctx)
	resp := next(ctx)

	dur := time.Since(oc.Stats.OperationStart).Round(time.Microsecond)
	opName := oc.OperationName
	if opName == "" {
		opName = "(anonymous)"
	}
	tid := tracing.TraceID(ctx)
	if n := len(resp.Errors); n > 0 {
		log.Errorf("trace=%s graphql op=%s errors=%d (%s)", tid, opName, n, dur)
	} else {
		log.Requestf("trace=%s graphql op=%s ok (%s)", tid, opName, dur)
	}
	return resp
}

// NewHandler returns an HTTP handler for the GraphQL API.
func NewHandler(client *ent.Client) http.Handler {
	srv := handler.New(NewSchema(client))
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.GET{})
	srv.Use(extension.Introspection{})
	srv.Use(extension.FixedComplexityLimit(MaxComplexity))
	// Registered before the Transactioner so the logged duration encloses the
	// transaction; both guard HasOperationContext for pre-parse failures.
	srv.Use(traceLogger{})
	srv.Use(guardedTransactioner{entgql.Transactioner{TxOpener: client}})
	return srv
}

// NewPlaygroundHandler returns the GraphQL Playground UI at the given endpoint.
func NewPlaygroundHandler(endpoint string) http.Handler {
	return playground.Handler("MLS Grid GraphQL", endpoint)
}
