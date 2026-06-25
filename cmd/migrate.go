package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/geo"
	"github.com/LunarHUE/MLS-Grid-Sync/search"
)

// migrateCmd applies every database migration the deployment needs — the ent
// schema, the PostGIS column/indexes, and the pg_trgm extension + fuzzy-search
// indexes — without an MLS token, a storage backend, or a sync run.
//
// It exists because serve deliberately runs no migrations ("sync/worker own
// the schema"), so a database populated by a binary that predated the
// fuzzy-search feature has no pg_trgm and serve cannot install it. Running
// `mls-cli migrate` once repairs that: every statement is idempotent
// (Schema.Create is additive; the geo/trigram migrations use IF NOT EXISTS).
var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database schema and extension migrations (ent schema, PostGIS, pg_trgm)",
	Long: `migrate applies all database migrations the deployment requires:

  - the ent schema (tables, columns, indexes)
  - the PostGIS extension + geometry column/indexes (geo search)
  - the pg_trgm extension + trigram/btree indexes (fuzzy address search)

Unlike a full sync it needs no MLS token and no storage backend, so it is the
safe way to provision or repair a database independently of serve (which never
migrates) and the sync loop. Every migration is idempotent, so it is safe to
re-run.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		sqlDB, err := sql.Open("postgres", appConfig.Database.DSN)
		if err != nil {
			return fmt.Errorf("failed opening connection to postgres: %w", err)
		}
		defer sqlDB.Close()

		drv := entsql.OpenDB(dialect.Postgres, sqlDB)
		db := ent.NewClient(ent.Driver(drv))
		defer db.Close()

		// Schema.Create is the first call that actually dials Postgres (sql.Open
		// is lazy); reuse the bounded retry so a still-booting DB doesn't fail
		// the command on the first attempt.
		if err := createSchemaWithRetry(ctx, db); err != nil {
			return err
		}
		log.Infof("migrate: ent schema applied")

		if err := geo.Migrate(ctx, sqlDB); err != nil {
			return fmt.Errorf("failed applying postgis migrations: %w", err)
		}
		log.Infof("migrate: PostGIS migrations applied")

		if err := search.Migrate(ctx, sqlDB); err != nil {
			return fmt.Errorf("failed applying trigram migrations: %w", err)
		}
		log.Infof("migrate: pg_trgm extension and fuzzy-search indexes applied")

		log.Infof("migrate: all migrations applied successfully")
		return nil
	},
}

// createSchemaWithRetry runs db.Schema.Create with the same bounded linear
// backoff setupComponents uses, so a DB that is just coming up (compose start,
// restart) doesn't fail migrate on the first try.
func createSchemaWithRetry(ctx context.Context, db *ent.Client) error {
	var schemaErr error
	for attempt := 1; attempt <= schemaCreateAttempts; attempt++ {
		if schemaErr = db.Schema.Create(ctx); schemaErr == nil {
			return nil
		}
		if attempt < schemaCreateAttempts {
			backoff := time.Duration(attempt) * schemaCreateBackoff
			log.Warnf("schema create attempt %d/%d failed (%v); retrying in %s",
				attempt, schemaCreateAttempts, schemaErr, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("failed creating schema resources after %d attempts: %w", schemaCreateAttempts, schemaErr)
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}
