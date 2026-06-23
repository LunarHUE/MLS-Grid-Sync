package processor

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// afterPassFake is a fakeProcessor that also implements AfterPasser, counting
// how many times the loop ran its finalize hook. Used to pin that
// RunPassNoFinalize drains records WITHOUT running AfterPass (the pipelined
// init consumer relies on this to avoid a quadratic per-page relink), while
// RunPass still runs it exactly once.
type afterPassFake struct {
	fakeProcessor
	afterCalls int
}

func (f *afterPassFake) AfterPass(_ context.Context, _ *sql.DB) error {
	f.afterCalls++
	return nil
}

func TestRunPassNoFinalize_DrainsButSkipsAfterPass(t *testing.T) {
	client, db := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	ids := seedRawOutputs(t, client, ctx, 3)

	fake := &afterPassFake{}
	p := New(client, db, fake)

	// Streaming drain: every row processed, but AfterPass must NOT fire.
	require.NoError(t, p.RunPassNoFinalize(ctx, rawoutput.ResourceLookup))
	assert.Equal(t, ids, fake.seen(), "no-finalize drain must process every row")
	assert.Equal(t, 0, fake.afterCalls, "no-finalize drain must NOT run AfterPass")

	// The finalize pass runs AfterPass exactly once and, because the cursor
	// already advanced, reprocesses nothing.
	require.NoError(t, p.RunPass(ctx, rawoutput.ResourceLookup))
	assert.Equal(t, 1, fake.afterCalls, "finalize pass runs AfterPass once")
	assert.Equal(t, ids, fake.seen(), "finalize pass must not reprocess drained rows")
}
