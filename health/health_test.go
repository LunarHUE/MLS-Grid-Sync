package health

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/processorcursor"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

// base is the fixed "now" the service clock returns so staleness math is
// deterministic; seeded timestamps are expressed relative to it.
var base = time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return base }

func okPing(context.Context) error { return nil }

func defaultThresholds() Thresholds {
	return Thresholds{SyncMaxStaleness: 30 * time.Minute, MaxRawPending: 10000, MaxAttachmentFailures: 100}
}

func byName(hs HealthStatus) map[string]HealthCheck {
	m := make(map[string]HealthCheck, len(hs.Checks))
	for _, c := range hs.Checks {
		m[c.Name] = c
	}
	return m
}

func seedSource(t *testing.T, ctx context.Context, c *ent.Client) string {
	t.Helper()
	return c.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx).ID
}

func seedBackfillSuccess(t *testing.T, ctx context.Context, c *ent.Client, src string, r rawoutput.Resource, endedAt time.Time) {
	t.Helper()
	c.SyncEvent.Create().
		SetSourceSystemID(src).
		SetResource(syncevent.Resource(r)).
		SetRunType(syncevent.RunTypeBackfill).
		SetStatus(syncevent.StatusSuccess).
		SetProcessorVersion("test").
		SetStartedAt(endedAt.Add(-time.Minute)).
		SetEndedAt(endedAt).
		SetHighWaterMark(endedAt).
		SaveX(ctx)
}

func seedEvent(t *testing.T, ctx context.Context, c *ent.Client, src string, r rawoutput.Resource, status syncevent.Status, startedAt time.Time) {
	t.Helper()
	c.SyncEvent.Create().
		SetSourceSystemID(src).
		SetResource(syncevent.Resource(r)).
		SetRunType(syncevent.RunTypeSync).
		SetStatus(status).
		SetProcessorVersion("test").
		SetStartedAt(startedAt).
		SaveX(ctx)
}

func seedCursor(t *testing.T, ctx context.Context, c *ent.Client, r rawoutput.Resource, last *uuid.UUID, modifiedAt time.Time) {
	t.Helper()
	b := c.ProcessorCursor.Create().
		SetResource(processorcursor.Resource(r)).
		SetProcessorVersion("test").
		SetModifiedAt(modifiedAt)
	if last != nil {
		b.SetLastRawOutputID(*last)
	}
	b.SaveX(ctx)
}

// seedRaw inserts n raw_output rows for r (uuidv7 ids are monotonic in
// creation order) and returns their ids in that order.
func seedRaw(t *testing.T, ctx context.Context, c *ent.Client, evID uuid.UUID, r rawoutput.Resource, n int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		row := c.RawOutput.Create().
			SetSyncEventID(evID).
			SetResource(r).
			SetSourceKey(fmt.Sprintf("%s-%d", r, i)).
			SetChangeType(rawoutput.ChangeTypeInsert).
			SetSourceModifiedAt(base).
			SetPayload([]byte(`{}`)).
			SaveX(ctx)
		ids = append(ids, row.ID)
	}
	return ids
}

// seedHealthy seeds the minimum state for a healthy /syncz: a backfill+success
// and a freshly-advanced cursor for every required resource.
func seedHealthy(t *testing.T, ctx context.Context, c *ent.Client) string {
	t.Helper()
	src := seedSource(t, ctx, c)
	for _, r := range processor.FetchableResources {
		seedBackfillSuccess(t, ctx, c, src, r, base.Add(-time.Minute))
		seedCursor(t, ctx, c, r, nil, base.Add(-time.Minute))
	}
	return src
}

func newSvc(c *ent.Client, th Thresholds) *Service {
	return NewService(c, okPing, th, fixedNow)
}

// --- Sync: fresh deploy / initial import gating ---

func TestSync_FreshDeploy_OnlyInitialImportFails(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	assert.False(t, hs.Healthy)

	m := byName(hs)
	assert.Equal(t, StatusFail, m["initial_import"].Status)
	// Every downstream check is skipped — one honest reason on a fresh deploy.
	for _, name := range []string{"processing_freshness", "raw_backlog", "attachment_failures", "resource_state"} {
		assert.Equal(t, StatusSkipped, m[name].Status, name)
	}
	for _, r := range processor.FetchableResources {
		assert.Equal(t, StatusSkipped, m[fetchFreshnessName(r)].Status, r)
	}
}

func TestSync_EmptyFeedStillCompletesInitialImport(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	src := seedSource(t, ctx, c)
	// All required resources backfill-succeed; open_house carries zero records
	// but still stamps success (record_count default 0, ended_at set).
	for _, r := range processor.FetchableResources {
		seedBackfillSuccess(t, ctx, c, src, r, base.Add(-time.Minute))
		seedCursor(t, ctx, c, r, nil, base.Add(-time.Minute))
	}

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	assert.True(t, hs.Healthy, "%+v", hs.Checks)
	assert.Equal(t, StatusPass, byName(hs)["initial_import"].Status)
}

// --- Sync: healthy + per-check failures ---

func TestSync_Healthy(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	seedHealthy(t, ctx, c)

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	assert.True(t, hs.Healthy, "%+v", hs.Checks)
	m := byName(hs)
	assert.Equal(t, StatusPass, m["initial_import"].Status)
	assert.Equal(t, StatusPass, m["property_fetch_freshness"].Status)
	assert.Equal(t, StatusPass, m["raw_backlog"].Status)
	assert.Equal(t, StatusPass, m["attachment_failures"].Status)
	assert.Equal(t, StatusPass, m["resource_state"].Status)
}

func TestSync_StaleFetchFails(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	src := seedSource(t, ctx, c)
	for _, r := range processor.FetchableResources {
		ended := base.Add(-time.Minute)
		if r == rawoutput.ResourceProperty {
			ended = base.Add(-2 * time.Hour) // beyond the 30m threshold
		}
		seedBackfillSuccess(t, ctx, c, src, r, ended)
		seedCursor(t, ctx, c, r, nil, base.Add(-time.Minute))
	}

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	assert.False(t, hs.Healthy)
	assert.Equal(t, StatusFail, byName(hs)["property_fetch_freshness"].Status)
}

func TestSync_RawBacklog_CountsOnlyAfterCursor(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	src := seedSource(t, ctx, c)
	evID := c.SyncEvent.Create().SetSourceSystemID(src).SetResource(syncevent.ResourceProperty).
		SetRunType(syncevent.RunTypeBackfill).SetStatus(syncevent.StatusSuccess).
		SetProcessorVersion("test").SetStartedAt(base).SetEndedAt(base).SetHighWaterMark(base).SaveX(ctx).ID
	// initial import complete for all required resources.
	for _, r := range processor.FetchableResources {
		if r != rawoutput.ResourceProperty {
			seedBackfillSuccess(t, ctx, c, src, r, base.Add(-time.Minute))
		}
		seedCursor(t, ctx, c, r, nil, base.Add(-time.Minute))
	}

	// 5 property raw rows; cursor sits at the 3rd → only rows 4 and 5 pending.
	ids := seedRaw(t, ctx, c, evID, rawoutput.ResourceProperty, 5)
	// Reset the property cursor to point at ids[2].
	c.ProcessorCursor.Update().Where(processorcursor.ResourceEQ(processorcursor.ResourceProperty)).
		SetLastRawOutputID(ids[2]).SetModifiedAt(base.Add(-time.Minute)).SaveX(ctx)

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	rb := byName(hs)["raw_backlog"]
	assert.Equal(t, StatusPass, rb.Status, rb.Message)
	assert.Contains(t, rb.Message, "2 raw records pending")
}

func TestSync_RawBacklog_MissingCursorCountsAll(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	src := seedSource(t, ctx, c)
	evID := c.SyncEvent.Create().SetSourceSystemID(src).SetResource(syncevent.ResourceProperty).
		SetRunType(syncevent.RunTypeBackfill).SetStatus(syncevent.StatusSuccess).
		SetProcessorVersion("test").SetStartedAt(base).SetEndedAt(base).SetHighWaterMark(base).SaveX(ctx).ID
	for _, r := range processor.FetchableResources {
		if r != rawoutput.ResourceProperty {
			seedBackfillSuccess(t, ctx, c, src, r, base.Add(-time.Minute))
		}
		// Cursor for every resource EXCEPT property — property has raw rows but
		// no cursor, so all its rows must count as pending.
		if r != rawoutput.ResourceProperty {
			seedCursor(t, ctx, c, r, nil, base.Add(-time.Minute))
		}
	}
	seedRaw(t, ctx, c, evID, rawoutput.ResourceProperty, 3)

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	rb := byName(hs)["raw_backlog"]
	assert.Contains(t, rb.Message, "3 raw records pending")
	assert.Contains(t, rb.Message, "no processor cursor yet for: property")
}

func TestSync_RawBacklog_OverThresholdFails(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	src := seedSource(t, ctx, c)
	evID := c.SyncEvent.Create().SetSourceSystemID(src).SetResource(syncevent.ResourceProperty).
		SetRunType(syncevent.RunTypeBackfill).SetStatus(syncevent.StatusSuccess).
		SetProcessorVersion("test").SetStartedAt(base).SetEndedAt(base).SetHighWaterMark(base).SaveX(ctx).ID
	for _, r := range processor.FetchableResources {
		if r != rawoutput.ResourceProperty {
			seedBackfillSuccess(t, ctx, c, src, r, base.Add(-time.Minute))
		}
		seedCursor(t, ctx, c, r, nil, base.Add(-time.Minute))
	}
	seedRaw(t, ctx, c, evID, rawoutput.ResourceProperty, 4)

	th := defaultThresholds()
	th.MaxRawPending = 1
	hs := newSvc(c, th).Sync(ctx)
	assert.False(t, hs.Healthy)
	assert.Equal(t, StatusFail, byName(hs)["raw_backlog"].Status)
}

func TestSync_ProcessingFreshness_StaysAdvisory(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	src := seedSource(t, ctx, c)
	for _, r := range processor.FetchableResources {
		seedBackfillSuccess(t, ctx, c, src, r, base.Add(-time.Minute))
		// Cursor advanced long ago (2h) — stale.
		seedCursor(t, ctx, c, r, nil, base.Add(-2*time.Hour))
	}

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	m := byName(hs)
	// Stale cursor alone is a warn, not a fail: still healthy (200).
	assert.True(t, hs.Healthy, "%+v", hs.Checks)
	assert.Equal(t, StatusWarn, m["processing_freshness"].Status)
	assert.Contains(t, m["processing_freshness"].Message, "has not advanced")
}

func TestSync_AttachmentFailures_OverThreshold(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	seedHealthy(t, ctx, c)
	evID := c.SyncEvent.Query().FirstX(ctx).ID
	c.Media.Create().
		SetID("M-1").
		SetSourceModifiedAt(base).
		SetResourceType(entmedia.ResourceTypeProperty).
		SetResourceRecordKey("LK-1").
		SaveX(ctx)
	for i := 0; i < 3; i++ {
		c.AttachmentJob.Create().
			SetID(uuid.New()).
			SetMediaKey("M-1").
			SetSyncEventID(evID).
			SetStatus(attachmentjob.StatusPermanentlyFailed).
			SaveX(ctx)
	}

	th := defaultThresholds()
	th.MaxAttachmentFailures = 2
	hs := newSvc(c, th).Sync(ctx)
	assert.False(t, hs.Healthy)
	assert.Equal(t, StatusFail, byName(hs)["attachment_failures"].Status)
}

func TestSync_ResourceState_FailedLatestEvent(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	src := seedHealthy(t, ctx, c)
	// A newer failed event for property becomes the latest by started_at.
	seedEvent(t, ctx, c, src, rawoutput.ResourceProperty, syncevent.StatusFailed, base)

	hs := newSvc(c, defaultThresholds()).Sync(ctx)
	rs := byName(hs)["resource_state"]
	assert.Equal(t, StatusFail, rs.Status)
	assert.Contains(t, rs.Message, "property")
}

// --- Ready ---

func TestReady_Healthy(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	hs := newSvc(c, defaultThresholds()).Ready(ctx)
	assert.True(t, hs.Healthy, "%+v", hs.Checks)
	m := byName(hs)
	assert.Equal(t, StatusPass, m["database"].Status)
	assert.Equal(t, StatusPass, m["schema"].Status)
	assert.Equal(t, StatusPass, m["health_config"].Status)
}

func TestReady_BadPingFails(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	svc := NewService(c, func(context.Context) error { return errors.New("down") }, defaultThresholds(), fixedNow)
	hs := svc.Ready(ctx)
	assert.False(t, hs.Healthy)
	assert.Equal(t, StatusFail, byName(hs)["database"].Status)
}

func TestReady_InvalidThresholdsFail(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	hs := NewService(c, okPing, Thresholds{SyncMaxStaleness: 0, MaxRawPending: -1, MaxAttachmentFailures: -1}, fixedNow).Ready(ctx)
	assert.False(t, hs.Healthy)
	assert.Equal(t, StatusFail, byName(hs)["health_config"].Status)
}

// --- Live & nil-dependency guards ---

func TestLive_AlwaysHealthyNoDeps(t *testing.T) {
	svc := NewService(nil, nil, Thresholds{}, nil)
	hs := svc.Live(context.Background())
	assert.True(t, hs.Healthy)
	require.Len(t, hs.Checks, 1)
	assert.Equal(t, "process", hs.Checks[0].Name)
	assert.Equal(t, StatusPass, hs.Checks[0].Status)
}

func TestNilDeps_ReadyAndSyncFailCleanly(t *testing.T) {
	svc := NewService(nil, nil, defaultThresholds(), fixedNow)

	ready := svc.Ready(context.Background())
	assert.False(t, ready.Healthy)
	rm := byName(ready)
	assert.Equal(t, StatusFail, rm["database"].Status) // ping nil
	assert.Equal(t, StatusFail, rm["schema"].Status)   // db nil

	sync := svc.Sync(context.Background())
	assert.False(t, sync.Healthy)
	assert.Equal(t, StatusFail, byName(sync)["initial_import"].Status)
}

// --- isolation & redaction (internal) ---

func TestSafeCheck_RecoversPanic(t *testing.T) {
	res := safeCheck("boom", func() HealthCheck { panic("kaboom") })
	assert.Equal(t, "boom", res.Name)
	assert.Equal(t, StatusFail, res.Status)
	assert.Contains(t, res.Message, "panicked")
}

func TestSanitizeError_MasksSecrets(t *testing.T) {
	cases := []string{
		"dial tcp: host=db user=app password=SUPERSECRET dbname=mls",
		"connect postgres://app:SUPERSECRET@db:5432/mls failed",
		"auth failed: Authorization: Bearer SUPERSECRET",
	}
	for _, in := range cases {
		out := sanitizeError(errors.New(in))
		assert.NotContains(t, out, "SUPERSECRET", in)
	}
}

func TestReady_DatabaseErrorRedacted(t *testing.T) {
	ctx := context.Background()
	c := testutil.NewTestDB(t)
	// A ping error that echoes a DSN password must never reach the output.
	svc := NewService(c, func(context.Context) error {
		return errors.New("pq: auth failed for host=db password=SUPERSECRET dbname=mls")
	}, defaultThresholds(), fixedNow)
	hs := svc.Ready(ctx)
	for _, ch := range hs.Checks {
		assert.NotContains(t, ch.Message, "SUPERSECRET", ch.Name)
	}
	assert.Contains(t, strings.ToLower(byName(hs)["database"].Message), "password=***")
}
