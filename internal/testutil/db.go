package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/geo"
	"github.com/LunarHUE/MLS-Grid-Sync/search"
)

// One PostgreSQL container is shared by every test in the process; each
// test gets its own database cloned from a pre-migrated template. This
// keeps per-test cost at one CREATE DATABASE (~100ms) instead of a
// container start + full migration (~15s), and keeps CI from drowning
// in dozens of concurrent containers. Advisory locks are scoped to the
// current database in PostgreSQL, so per-database isolation is as good
// as per-container for everything the suite exercises.
const templateDB = "mls_template"

var (
	pgOnce  sync.Once
	pgURL   *url.URL // admin connection URL; swap the path for other databases
	pgAdmin *sql.DB
	pgErr   error

	pgMu  sync.Mutex // serializes CREATE DATABASE — the template must have no other users
	pgSeq int
)

// initPostgres starts the shared container and migrates the ent schema
// into the template database. The container is not Terminated here:
// testcontainers' reaper (ryuk) removes it when the test process exits.
func initPostgres() {
	ctx := context.Background()

	// imresamu/postgis: multi-arch (amd64+arm64) PostGIS build on the
	// same alpine/musl base as the postgres:15-alpine it replaced —
	// required by the geo-search queries (geo.Migrate).
	ctr, err := postgres.Run(ctx, "imresamu/postgis:15-3.5-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			// initdb restarts postgres once during bootstrap, so a bare
			// port wait can hand out a socket that resets mid-restart
			// (the cause of "connection reset by peer" flakes on slow
			// CI runners). The second "ready" log line is the reliable
			// signal.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		pgErr = fmt.Errorf("start postgres container: %w", err)
		return
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		pgErr = fmt.Errorf("get connection string: %w", err)
		return
	}
	pgURL, err = url.Parse(dsn)
	if err != nil {
		pgErr = fmt.Errorf("parse connection string %q: %w", dsn, err)
		return
	}

	pgAdmin, err = sql.Open("postgres", dsn)
	if err != nil {
		pgErr = fmt.Errorf("open admin connection: %w", err)
		return
	}

	if _, err := pgAdmin.ExecContext(ctx, "CREATE DATABASE "+templateDB); err != nil {
		pgErr = fmt.Errorf("create template database: %w", err)
		return
	}

	tmplDB, err := sql.Open("postgres", dsnFor(templateDB))
	if err != nil {
		pgErr = fmt.Errorf("open template database: %w", err)
		return
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, tmplDB)))
	if err := client.Schema.Create(ctx); err != nil {
		pgErr = fmt.Errorf("migrate template schema: %w", err)
		return
	}
	// PostGIS column + indexes live in the template too, so every
	// cloned test database inherits them.
	if err := geo.Migrate(ctx, tmplDB); err != nil {
		pgErr = fmt.Errorf("postgis template migration: %w", err)
		return
	}
	// Trigram extension + address-search indexes live in the template
	// too, so every cloned test database inherits them.
	if err := search.Migrate(ctx, tmplDB); err != nil {
		pgErr = fmt.Errorf("trigram template migration: %w", err)
		return
	}
	// Close every connection to the template: CREATE DATABASE ...
	// TEMPLATE requires exclusive access to the source database.
	if err := client.Close(); err != nil {
		pgErr = fmt.Errorf("close template connection: %w", err)
	}
}

func dsnFor(database string) string {
	u := *pgURL
	u.Path = "/" + database
	return u.String()
}

// NewTestDB returns a ready *ent.Client on a fresh database with the schema
// already migrated. Client close is registered on t automatically; the
// backing container is shared process-wide and reaped at process exit.
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

	pgOnce.Do(initPostgres)
	require.NoError(t, pgErr, "shared postgres container")

	pgMu.Lock()
	pgSeq++
	name := fmt.Sprintf("mls_test_%d", pgSeq)
	_, err := pgAdmin.Exec(fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB))
	pgMu.Unlock()
	require.NoError(t, err, "clone template database")

	sqlDB, err := sql.Open("postgres", dsnFor(name))
	require.NoError(t, err, "open *sql.DB")

	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	client := ent.NewClient(ent.Driver(drv))
	t.Cleanup(func() { client.Close() })

	return client, sqlDB
}
