//go:build ignore

package main

import (
	"github.com/lunarhue/libs-go/log"

	"entgo.io/contrib/entgql"
	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
)

func main() {
	// Initialize the entgql extension
	ex, err := entgql.NewExtension(
		// This tells the plugin to generate a raw GraphQL schema file
		entgql.WithSchemaGenerator(),
		entgql.WithSchemaPath("../graph/ent.graphql"),
		entgql.WithConfigPath("../gqlgen.yml"),
		entgql.WithWhereInputs(true),
		// By default, entgql forces you to have mutations.
		// Since your DB is read-only, we skip mutation generation.
		// entgql.WithMutations(), <- Do not include this
	)
	if err != nil {
		log.Panicf("creating entgql extension: %v", err)
	}

	opts := []entc.Option{
		entc.Extensions(ex),
	}

	// Run the Ent compiler on your schema directory
	if err := entc.Generate("./schema", &gen.Config{}, opts...); err != nil {
		log.Panicf("running ent codegen: %v", err)
	}
}
