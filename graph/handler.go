package graph

import (
	"context"
	"net/http"

	"entgo.io/contrib/entgql"
	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
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

// NewHandler returns an HTTP handler for the GraphQL API.
func NewHandler(client *ent.Client) http.Handler {
	srv := handler.New(NewSchema(client))
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.GET{})
	srv.Use(extension.Introspection{})
	srv.Use(guardedTransactioner{entgql.Transactioner{TxOpener: client}})
	return srv
}

// NewPlaygroundHandler returns the GraphQL Playground UI at the given endpoint.
func NewPlaygroundHandler(endpoint string) http.Handler {
	return playground.Handler("MLS Grid GraphQL", endpoint)
}
