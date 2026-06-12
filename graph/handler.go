package graph

import (
	"net/http"

	"entgo.io/contrib/entgql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
)

// NewHandler returns an HTTP handler for the GraphQL API.
func NewHandler(client *ent.Client) http.Handler {
	srv := handler.New(NewSchema(client))
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.GET{})
	srv.Use(extension.Introspection{})
	srv.Use(entgql.Transactioner{TxOpener: client})
	return srv
}

// NewPlaygroundHandler returns the GraphQL Playground UI at the given endpoint.
func NewPlaygroundHandler(endpoint string) http.Handler {
	return playground.Handler("MLS Grid GraphQL", endpoint)
}
