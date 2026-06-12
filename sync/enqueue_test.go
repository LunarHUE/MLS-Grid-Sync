package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
)

// seedMedia creates the parent Media row the AttachmentJob.media FK points
// at. Phase 1's schema requires the parent to exist before any job rows.
// Idempotent — repeated calls for the same key are no-ops, which lets
// helper functions seed defensively without coordinating with callers.
func seedMedia(t *testing.T, client *ent.Client, ctx context.Context, mediaKey string, ts time.Time) {
	t.Helper()
	exists, err := client.Media.Query().Where(entmedia.IDEQ(mediaKey)).Exist(ctx)
	require.NoError(t, err)
	if exists {
		return
	}
	require.NoError(t, client.Media.Create().
		SetID(mediaKey).
		SetSourceModifiedAt(ts).
		SetResourceType(entmedia.ResourceTypeProperty).
		SetResourceRecordKey("LK-stub").
		Exec(ctx))
}

// mediaRecord builds a media payload in the EXPANDED shape — what
// splitExpandedChildren actually writes to raw_output, NOT the
// standalone /v2/Media fantasy. Per the 2026-06-11 audit:
//
//   - ModificationTimestamp is absent (0%) — the splitter owns
//     source_modified_at on the row; parseEnqueueTimestamp falls back
//     to ModificationTimestamp only as a defensive secondary path.
//   - ResourceName is splitter-injected ("Property") — the kept
//     parser requirement is the cross-layer tripwire; included here
//     for realism even though EnqueueAttachmentJobs doesn't read it.
//   - MlgCanView is absent (0%) — but the test arg lets specific tests
//     (HiddenSkipped) exercise the dead-but-defensive path.
func mediaRecord(t *testing.T, mediaKey string, ts time.Time, canView bool) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"MediaKey":                   mediaKey,
		"ResourceName":               "Property",
		"MlgCanView":                 canView,
		"MediaModificationTimestamp": ts.Format(time.RFC3339),
		"MediaURL":                   "https://images.example/" + mediaKey + ".jpg",
	})
	require.NoError(t, err)
	return b
}

func countJobsForKey(t *testing.T, client *ent.Client, ctx context.Context, mediaKey string) int {
	t.Helper()
	n, err := client.AttachmentJob.Query().Where(attachmentjob.MediaKey(mediaKey)).Count(ctx)
	require.NoError(t, err)
	return n
}

// TestEnqueueAttachmentJobs_IdempotentOnSameTimestamp pins §6 idempotency:
// re-running a sync over the same Media record with no MediaModification
// Timestamp advance must create zero new attachment_job rows.
//
// Pre-Phase-4 every sync inserted a fresh pending job per record; sha256
// dedup masked the wasted work at the byte layer but the job table grew
// without bound. This test pins the new behavior in place.
func TestEnqueueAttachmentJobs_IdempotentOnSameTimestamp(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	ts := time.Now().UTC().Truncate(time.Second)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	seedMedia(t, client, ctx, "M-1", ts)
	rec := []json.RawMessage{mediaRecord(t, "M-1", ts, true)}

	// First enqueue: creates one job.
	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, rec))
	assert.Equal(t, 1, countJobsForKey(t, client, ctx, "M-1"))

	// Mark the existing job succeeded (the worker would do this); the next
	// enqueue at the same timestamp must skip.
	_, err := client.AttachmentJob.Update().
		Where(attachmentjob.MediaKey("M-1")).
		SetStatus(attachmentjob.StatusSucceeded).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, rec))
	assert.Equal(t, 1, countJobsForKey(t, client, ctx, "M-1"),
		"same timestamp → no new job; the §6 existence check did its job")
}

// TestEnqueueAttachmentJobs_TimestampAdvanceEnqueues: bumping
// MediaModificationTimestamp creates a new job even though the prior one
// succeeded. This is the content-change re-download mechanism (§6).
func TestEnqueueAttachmentJobs_TimestampAdvanceEnqueues(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	t0 := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	t1 := t0.Add(30 * time.Minute)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	seedMedia(t, client, ctx, "M-1", t0)

	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, []json.RawMessage{
		mediaRecord(t, "M-1", t0, true),
	}))
	// Mark succeeded so it counts as blocking.
	_, err := client.AttachmentJob.Update().
		Where(attachmentjob.MediaKey("M-1")).
		SetStatus(attachmentjob.StatusSucceeded).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, []json.RawMessage{
		mediaRecord(t, "M-1", t1, true),
	}))
	assert.Equal(t, 2, countJobsForKey(t, client, ctx, "M-1"),
		"newer MediaModificationTimestamp → new job")
}

// TestEnqueueAttachmentJobs_CanceledDoesNotBlock is the regression test
// for the re-list bypass found during plan review: a canceled job (from
// tombstone cascade §3) sets media_modified_at when it was enqueued. If
// canceled were in the blocking set, an un-tombstoned property's media
// would silently never re-download. The §6 status filter excludes
// canceled and permanently_failed.
func TestEnqueueAttachmentJobs_CanceledDoesNotBlock(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	ts := time.Now().UTC().Truncate(time.Second)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	seedMedia(t, client, ctx, "M-1", ts)

	// 1) Enqueue, 2) cascade-cancel (timestamp preserved), 3) re-enqueue at
	// the *same* timestamp — must create a new job because the canceled one
	// must not block.
	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, []json.RawMessage{mediaRecord(t, "M-1", ts, true)}))
	_, err := client.AttachmentJob.Update().
		Where(attachmentjob.MediaKey("M-1")).
		SetStatus(attachmentjob.StatusCanceled).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, []json.RawMessage{mediaRecord(t, "M-1", ts, true)}))
	assert.Equal(t, 2, countJobsForKey(t, client, ctx, "M-1"),
		"canceled jobs MUST NOT suppress re-enqueue — re-listings depend on this")
}

// TestEnqueueAttachmentJobs_PermanentlyFailedDoesNotBlock: a 3-attempt
// terminal failure should not poison-pill the MediaKey forever. New
// revisions get a fresh shot.
func TestEnqueueAttachmentJobs_PermanentlyFailedDoesNotBlock(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	ts := time.Now().UTC().Truncate(time.Second)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	seedMedia(t, client, ctx, "M-1", ts)

	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, []json.RawMessage{mediaRecord(t, "M-1", ts, true)}))
	_, err := client.AttachmentJob.Update().
		Where(attachmentjob.MediaKey("M-1")).
		SetStatus(attachmentjob.StatusPermanentlyFailed).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, []json.RawMessage{mediaRecord(t, "M-1", ts, true)}))
	assert.Equal(t, 2, countJobsForKey(t, client, ctx, "M-1"),
		"permanently_failed MUST NOT suppress re-enqueue on a new revision")
}

// TestEnqueueAttachmentJobs_HiddenSkipped: MlgCanView=false records are
// not enqueued (preserving Phase 1 semantics through the rewrite).
func TestEnqueueAttachmentJobs_HiddenSkipped(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	ts := time.Now().UTC().Truncate(time.Second)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	// No need to seed Media — hidden records are dropped before any insert
	// reaches the FK check.
	require.NoError(t, svc.EnqueueAttachmentJobs(ctx, syncEventID, []json.RawMessage{
		mediaRecord(t, "M-hidden", ts, false),
	}))
	assert.Equal(t, 0, countJobsForKey(t, client, ctx, "M-hidden"))
}

// keep uuid imported (used by other tests in this package).
var _ = uuid.New
