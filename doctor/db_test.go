package doctor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// These tests exercise the real DB checks against the shared Testcontainers
// Postgres+PostGIS (internal/testutil). They run under the same `go test ./...`
// that the rest of the suite uses; the CI runner has Docker.

func TestRun_DBHealthy(t *testing.T) {
	_, sqlDB := testutil.NewTestDBWithSQL(t)

	results := Run(context.Background(), Deps{
		Config:      baseConfig(),
		DB:          sqlDB,
		BuildStorer: okStorer,
		Now:         time.Now,
	}, Options{SkipMLS: true, SkipStorage: true})

	m := byName(results)
	assert.Equal(t, StatusPass, m["postgres"].Status, m["postgres"].Message)
	assert.Equal(t, StatusPass, m["postgis"].Status, m["postgis"].Message)
	assert.Equal(t, StatusPass, m["tables"].Status, m["tables"].Message)
	assert.Equal(t, StatusPass, m["clock_skew"].Status, m["clock_skew"].Message)
}

func TestRun_PostGISMissing_Fail(t *testing.T) {
	_, sqlDB := testutil.NewTestDBWithSQL(t)
	// CASCADE also drops the generated geom column/indexes; fine on a
	// disposable per-test database.
	_, err := sqlDB.Exec("DROP EXTENSION IF EXISTS postgis CASCADE")
	require.NoError(t, err)

	results := Run(context.Background(), Deps{
		Config: baseConfig(), DB: sqlDB, BuildStorer: okStorer,
	}, Options{SkipMLS: true, SkipStorage: true})

	assert.Equal(t, StatusFail, byName(results)["postgis"].Status)
}

func TestRun_MissingTable_Fail(t *testing.T) {
	_, sqlDB := testutil.NewTestDBWithSQL(t)
	_, err := sqlDB.Exec("DROP TABLE IF EXISTS processor_cursor CASCADE")
	require.NoError(t, err)

	results := Run(context.Background(), Deps{
		Config: baseConfig(), DB: sqlDB, BuildStorer: okStorer,
	}, Options{SkipMLS: true, SkipStorage: true})

	tbl := byName(results)["tables"]
	assert.Equal(t, StatusFail, tbl.Status)
	assert.Contains(t, tbl.Message, "processor_cursor")
	// Remediation must read as "fresh DB", not corruption.
	assert.Contains(t, tbl.Remediation, "brand-new database")
}
