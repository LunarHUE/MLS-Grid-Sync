package processor

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
)

// TestCascade_PropertyTombstoneCancelsPendingJobs verifies Phase 4 §3:
//   - pending, retrying, AND in_progress attachment_jobs for the listing's
//     media are canceled, with claimed_at/claimed_by cleared.
//
// Pre-Phase-4 this test asserted in_progress was *left alone* because the
// worker's success write was unconditional and would stomp any cancel.
// Phase 4 §2's CAS guard (WHERE status='in_progress' AND claimed_by=$me)
// makes the wider cascade safe — a late success write hits zero rows.
func TestCascade_PropertyTombstoneCancelsPendingJobs(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	// Seed Property + Media (one row), MediaKey = M-1.
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyProcess(t, client, ctx, insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "500000.00",
	}, ts1)))

	client.Media.Create().
		SetID("M-1").
		SetSourceModifiedAt(ts1).
		SetResourceType(entmedia.ResourceTypeProperty).
		SetResourceRecordKey("LK-1").
		SaveX(ctx)

	// Three attachment_jobs against M-1: one pending, one retrying, one in_progress.
	pendingID := uuid.New()
	client.AttachmentJob.Create().
		SetID(pendingID).
		SetMediaKey("M-1").
		SetSyncEventID(evID).
		SetStatus(attachmentjob.StatusPending).
		SaveX(ctx)
	retryingID := uuid.New()
	client.AttachmentJob.Create().
		SetID(retryingID).
		SetMediaKey("M-1").
		SetSyncEventID(evID).
		SetStatus(attachmentjob.StatusRetrying).
		SetClaimedBy("worker-old").
		SaveX(ctx)
	inProgressID := uuid.New()
	client.AttachmentJob.Create().
		SetID(inProgressID).
		SetMediaKey("M-1").
		SetSyncEventID(evID).
		SetStatus(attachmentjob.StatusInProgress).
		SetClaimedBy("worker-live").
		SetClaimedAt(time.Now()).
		SaveX(ctx)

	// Tombstone the Property.
	ts2 := ts1.Add(time.Hour)
	tomb := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runPropertyProcess(t, client, ctx, tomb))

	// Assert pending and retrying went to canceled; in_progress untouched.
	pending := client.AttachmentJob.GetX(ctx, pendingID)
	assert.Equal(t, attachmentjob.StatusCanceled, pending.Status)
	assert.Nil(t, pending.ClaimedAt)
	assert.Nil(t, pending.ClaimedBy)

	retrying := client.AttachmentJob.GetX(ctx, retryingID)
	assert.Equal(t, attachmentjob.StatusCanceled, retrying.Status)
	assert.Nil(t, retrying.ClaimedBy, "ClaimedBy cleared on cancel")

	inProgress := client.AttachmentJob.GetX(ctx, inProgressID)
	assert.Equal(t, attachmentjob.StatusCanceled, inProgress.Status,
		"Phase 4 §3: in_progress IS canceled now that the worker's success "+
			"write is CAS-guarded — see plan §2/§3")
	assert.Nil(t, inProgress.ClaimedBy,
		"claimed_by cleared on cancel so the reaper doesn't mistake this for a live worker")
	assert.Nil(t, inProgress.ClaimedAt)
}

// TestCascade_LateSuccessWriteAfterCancelIsZeroRows pins the §2 CAS
// guard from the cascade side: after the cascade flips an in_progress
// job to canceled (with claimed_by cleared), a worker arriving late
// with the success write hits the WHERE status='in_progress' AND
// claimed_by=$me guard and matches zero rows. The job stays canceled.
func TestCascade_LateSuccessWriteAfterCancelIsZeroRows(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyProcess(t, client, ctx, insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-LATE",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
	}, ts1)))

	client.Media.Create().
		SetID("M-LATE").
		SetSourceModifiedAt(ts1).
		SetResourceType(entmedia.ResourceTypeProperty).
		SetResourceRecordKey("LK-LATE").
		SaveX(ctx)

	// Worker has the job in_progress under workerID "worker-X".
	jobID := uuid.New()
	client.AttachmentJob.Create().
		SetID(jobID).
		SetMediaKey("M-LATE").
		SetSyncEventID(evID).
		SetStatus(attachmentjob.StatusInProgress).
		SetClaimedBy("worker-X").
		SetClaimedAt(time.Now()).
		SaveX(ctx)

	// Tombstone fires → cascade cancels the job.
	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runPropertyProcess(t, client, ctx, insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-LATE",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)))

	// The worker "wakes up" after the cancel and attempts its guarded
	// success UPDATE. The simulated guard mirrors the worker's:
	//   WHERE status='in_progress' AND claimed_by=$me
	// Expected: 0 rows affected; status remains canceled.
	updated := client.AttachmentJob.Update().
		Where(
			attachmentjob.IDEQ(jobID),
			attachmentjob.StatusEQ(attachmentjob.StatusInProgress),
			attachmentjob.ClaimedByEQ("worker-X"),
		).
		SetStatus(attachmentjob.StatusSucceeded).
		SaveX(ctx)
	assert.Equal(t, 0, updated, "late success write must match 0 rows under the CAS guard")

	after := client.AttachmentJob.GetX(ctx, jobID)
	assert.Equal(t, attachmentjob.StatusCanceled, after.Status, "job stays canceled — the cascade wins")
}

// TestCascade_FirstSightingTombstoneIsHarmlessOnEmptyMedia: a Property
// arriving already-tombstoned with no Media yet → cascade runs as a no-op,
// doesn't error.
func TestCascade_FirstSightingTombstoneIsHarmlessOnEmptyMedia(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-INVISIBLE",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts)
	require.NoError(t, runPropertyProcess(t, client, ctx, raw))

	// No Media → no attachment_jobs → cascade does nothing. Just verify
	// the entity was tombstoned and the cascade didn't error out.
	count := client.AttachmentJob.Query().CountX(ctx)
	assert.Zero(t, count)
}

// ensure the unused suppressor stays
var _ = rawoutput.ResourceProperty
