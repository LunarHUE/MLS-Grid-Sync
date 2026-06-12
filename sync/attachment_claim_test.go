package sync_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/attachmentjob"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/syncevent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/internal/testutil"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/storage"
	pkgsync "github.com/lunarhue/website-highpointe/packages/mls-grid-sync/sync"
)

func seedPendingJob(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, mediaKey string) uuid.UUID {
	t.Helper()
	seedMedia(t, client, ctx, mediaKey, time.Now())
	j := client.AttachmentJob.Create().
		SetMediaKey(mediaKey).
		SetSyncEventID(syncEventID).
		SetStatus(attachmentjob.StatusPending).
		SaveX(ctx)
	return j.ID
}

// TestClaimBatch_TwoWorkersDisjointSets pins Phase 4 §1's FOR UPDATE SKIP
// LOCKED contract: two workers claiming the same backlog concurrently get
// disjoint id sets, and no job is double-claimed.
//
// Pre-Phase-4 the worker selected pending jobs with no lock, so two
// workers polling simultaneously would race and both pick the same job —
// the symptom that motivated the SKIP LOCKED redesign.
func TestClaimBatch_TwoWorkersDisjointSets(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	const total = 20
	want := make(map[uuid.UUID]bool, total)
	for i := 0; i < total; i++ {
		id := seedPendingJob(t, client, ctx, syncEventID, randMediaKey(t, i))
		want[id] = true
	}

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)

	// Two workers fire concurrent claims for the entire backlog.
	const batch = total
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed [2][]uuid.UUID
	)
	wg.Add(2)
	for w := 0; w < 2; w++ {
		go func(idx int) {
			defer wg.Done()
			ids, err := svc.ClaimBatch(ctx, "worker-"+string(rune('A'+idx)), batch)
			require.NoError(t, err)
			mu.Lock()
			claimed[idx] = ids
			mu.Unlock()
		}(w)
	}
	wg.Wait()

	// Disjoint sets — every claimed id appears in exactly one bucket.
	seen := map[uuid.UUID]int{}
	for _, ids := range claimed {
		for _, id := range ids {
			seen[id]++
		}
	}
	for id, n := range seen {
		assert.Equalf(t, 1, n, "job %s claimed by %d workers (must be exactly 1)", id, n)
	}

	// Union covers every seeded job — none stranded.
	assert.Equal(t, total, len(seen), "all %d jobs must be claimed by exactly one worker", total)

	// Each claimed job is now in_progress with the right claimed_by stamp.
	for idx, ids := range claimed {
		for _, id := range ids {
			j := client.AttachmentJob.GetX(ctx, id)
			assert.Equal(t, attachmentjob.StatusInProgress, j.Status)
			require.NotNil(t, j.ClaimedBy)
			assert.Equal(t, "worker-"+string(rune('A'+idx)), *j.ClaimedBy)
			require.NotNil(t, j.ClaimedAt)
		}
	}
}

func randMediaKey(t *testing.T, i int) string {
	t.Helper()
	return uuid.NewString() + "-i-" + string(rune('0'+i%10))
}

// TestReapStaleClaims pins §4: a worker that crashed with claimed_at
// older than the lease has its job returned to pending without
// consuming a retry. Fresh in_progress jobs are NOT reaped.
func TestReapStaleClaims(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	seedMedia(t, client, ctx, "M-stale", time.Now())
	seedMedia(t, client, ctx, "M-fresh", time.Now())

	// Stale: claimed 20 minutes ago, attempt_count=1.
	staleID := client.AttachmentJob.Create().
		SetMediaKey("M-stale").
		SetSyncEventID(syncEventID).
		SetStatus(attachmentjob.StatusInProgress).
		SetClaimedAt(time.Now().Add(-20 * time.Minute)).
		SetClaimedBy("worker-crashed").
		SetAttemptCount(1).
		SaveX(ctx).ID

	// Fresh: claimed 30s ago, well under lease.
	freshID := client.AttachmentJob.Create().
		SetMediaKey("M-fresh").
		SetSyncEventID(syncEventID).
		SetStatus(attachmentjob.StatusInProgress).
		SetClaimedAt(time.Now().Add(-30 * time.Second)).
		SetClaimedBy("worker-live").
		SetAttemptCount(0).
		SaveX(ctx).ID

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	n, err := svc.ReapStaleClaims(ctx, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "exactly the stale job should be reaped")

	stale := client.AttachmentJob.GetX(ctx, staleID)
	assert.Equal(t, attachmentjob.StatusPending, stale.Status)
	assert.Nil(t, stale.ClaimedAt, "claimed_at cleared")
	assert.Nil(t, stale.ClaimedBy, "claimed_by cleared")
	assert.Equal(t, 1, stale.AttemptCount,
		"§4: reaping a crashed worker MUST NOT consume a retry — attempt_count unchanged")

	fresh := client.AttachmentJob.GetX(ctx, freshID)
	assert.Equal(t, attachmentjob.StatusInProgress, fresh.Status, "fresh claim untouched")
	require.NotNil(t, fresh.ClaimedBy)
	assert.Equal(t, "worker-live", *fresh.ClaimedBy)
}

// TestReapStaleClaims_LeaseBoundary: a claim exactly at the lease boundary
// is not yet stale. Avoids flaky reap of in-flight work right at expiry.
func TestReapStaleClaims_LeaseBoundary(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)
	seedMedia(t, client, ctx, "M-edge", time.Now())

	client.AttachmentJob.Create().
		SetMediaKey("M-edge").
		SetSyncEventID(syncEventID).
		SetStatus(attachmentjob.StatusInProgress).
		SetClaimedAt(time.Now().Add(-9 * time.Minute)). // inside 10-minute lease
		SetClaimedBy("worker-edge").
		SaveX(ctx)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	n, err := svc.ReapStaleClaims(ctx, 10*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "claim just inside the lease must NOT be reaped")
}
