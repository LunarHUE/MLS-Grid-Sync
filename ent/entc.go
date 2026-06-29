//go:build ignore

package main

import (
	"os"

	"github.com/lunarhue/libs-go/log"

	"entgo.io/contrib/entgql"
	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
)

// entGraphQLSchema is the entgql-generated GraphQL schema, relative to this
// file's directory (ent/). It is regenerated from scratch on every run — see
// the os.Remove below.
const entGraphQLSchema = "../graph/ent.graphql"

func main() {
	// entgql's schema generator MERGES into an existing ent.graphql, and that
	// merge is non-idempotent for the builtin Time/Map scalars: it toggles their
	// declarations in and out on alternating runs (present → absent → present),
	// which breaks gqlgen with either "Cannot redeclare type Time" or "Undefined
	// type Time" depending on the run's parity. Removing the file first forces a
	// from-scratch generation, which is deterministic: Time/Map are always
	// declared here, so scalars.graphql must NOT re-declare them. This must run
	// before NewExtension, which reads the existing schema at construction time.
	if err := os.Remove(entGraphQLSchema); err != nil && !os.IsNotExist(err) {
		log.Panicf("removing stale ent.graphql: %v", err)
	}

	// Initialize the entgql extension
	ex, err := entgql.NewExtension(
		// This tells the plugin to generate a raw GraphQL schema file
		entgql.WithSchemaGenerator(),
		entgql.WithSchemaPath(entGraphQLSchema),
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
