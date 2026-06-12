package testutil

import (
	"context"
	"database/sql"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
)

// NewTestDB spins up a PostgreSQL 15 container, auto-migrates the schema, and
// returns a ready *ent.Client. Cleanup (container teardown + client close) is
// registered on t automatically.
func NewTestDB(t *testing.T) *ent.Client {
	client, _ := NewTestDBWithSQL(t)
	return client
}

// NewTestDBWithSQL is like NewTestDB but also returns the underlying *sql.DB.
// Use this when code under test needs direct access to *sql.DB — for example
// the processor's per-resource advisory lock helper, which must pin a
// dedicated *sql.Conn (see sync/processor/lock.go).
func NewTestDBWithSQL(t *testing.T) (*ent.Client, *sql.DB) {
	t.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:15-alpine",
		postgres.WithDatabase("mls_test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp"),
		),
	)
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "get connection string")

	sqlDB, err := sql.Open("postgres", dsn)
	require.NoError(t, err, "open *sql.DB")

	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	client := ent.NewClient(ent.Driver(drv))
	t.Cleanup(func() { client.Close() })

	require.NoError(t, client.Schema.Create(ctx), "migrate schema")
	return client, sqlDB
}
