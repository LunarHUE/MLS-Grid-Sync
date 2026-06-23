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

	// FeatureUpsert generates OnConflict / UpdateNewValues APIs on the entity
	// builders. The typed processor's bulk-projection path (sync/processor) uses
	// them for batched entity upserts; without this feature ent only emits
	// conflict-free CreateBulk. Additive — existing query/create APIs are
	// unchanged, and mutations stay off (DB is read-through-projector).
	cfg := &gen.Config{
		Features: []gen.Feature{gen.FeatureUpsert},
	}

	// Run the Ent compiler on your schema directory
	if err := entc.Generate("./schema", cfg, opts...); err != nil {
		log.Panicf("running ent codegen: %v", err)
	}
}
