package sync_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachment"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
)

// recordingStorer captures upload keys so tests can assert on key
// construction (notably the KeyPrefix refactor).
type recordingStorer struct {
	mu   sync.Mutex
	keys []string
}

func (r *recordingStorer) Upload(_ context.Context, key string, body io.Reader, _ string) (string, error) {
	io.Copy(io.Discard, body) //nolint:errcheck
	r.mu.Lock()
	r.keys = append(r.keys, key)
	r.mu.Unlock()
	return "recorded://" + key, nil
}

func (r *recordingStorer) lastKey(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.keys) == 0 {
		t.Fatal("no keys recorded")
	}
	return r.keys[len(r.keys)-1]
}

// seedMediaRawOutput inserts a raw_output row for a Media record carrying
// the URL the worker resolves at processJob time. The worker reads
// MediaURL from raw_output.payload, not from the Media table.
func seedMediaRawOutput(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, mediaKey, url string) {
	t.Helper()
	client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourceMedia).
		SetSourceKey(mediaKey).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(time.Now()).
		SetPayload(map[string]any{
			"MediaKey": mediaKey,
			"MediaURL": url,
		}).
		SaveX(ctx)
}

// TestWorker_SuccessfulDownload pins the happy path: 200 OK, bytes
// stored, attachment row created, job moved to succeeded under the
// §2 CAS guard.
func TestWorker_SuccessfulDownload(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("happy-path-bytes"))
	}))
	defer srv.Close()

	seedMedia(t, client, ctx, "M-ok", time.Now())
	seedMediaRawOutput(t, client, ctx, syncEventID, "M-ok", srv.URL)
	jobID := seedPendingJob(t, client, ctx, syncEventID, "M-ok")

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	worker := pkgsync.NewAttachmentWorker(svc,
		pkgsync.WithHTTPDoer(srv.Client()),
	)
	_, err := worker.Run(ctx)
	require.NoError(t, err)

	got := client.AttachmentJob.GetX(ctx, jobID)
	assert.Equal(t, attachmentjob.StatusSucceeded, got.Status)
	require.NotNil(t, got.AttachmentID)
}

// TestWorker_RetriesThenPermanentlyFails pins the failure ladder: 404
// twice → retrying, third attempt → permanently_failed. attempt_count
// advances under the §2 CAS guard.
func TestWorker_RetriesThenPermanentlyFails(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	seedMedia(t, client, ctx, "M-404", time.Now())
	seedMediaRawOutput(t, client, ctx, syncEventID, "M-404", srv.URL)
	jobID := seedPendingJob(t, client, ctx, syncEventID, "M-404")

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	worker := pkgsync.NewAttachmentWorker(svc, pkgsync.WithHTTPDoer(srv.Client()))

	// Attempt 1 → retrying (attempt_count=1).
	_, err := worker.Run(ctx)
	require.NoError(t, err)
	got := client.AttachmentJob.GetX(ctx, jobID)
	assert.Equal(t, attachmentjob.StatusRetrying, got.Status)
	assert.Equal(t, 1, got.AttemptCount)

	// Attempt 2 → still retrying (attempt_count=2).
	_, err = worker.Run(ctx)
	require.NoError(t, err)
	got = client.AttachmentJob.GetX(ctx, jobID)
	assert.Equal(t, attachmentjob.StatusRetrying, got.Status)
	assert.Equal(t, 2, got.AttemptCount)

	// Attempt 3 → permanently_failed (attempt_count=3).
	_, err = worker.Run(ctx)
	require.NoError(t, err)
	got = client.AttachmentJob.GetX(ctx, jobID)
	assert.Equal(t, attachmentjob.StatusPermanentlyFailed, got.Status)
	assert.Equal(t, 3, got.AttemptCount)
}

// TestWorker_CASGuardLetsCancelWin pins the headline §2 invariant: a
// job canceled mid-download survives the worker's late success write.
// We simulate the race by canceling the job between claim and download:
// the claim moves it to in_progress, the cancel flips it to canceled
// (clearing claimed_by), then the download succeeds and the CAS write
// matches zero rows.
//
// This is the regression test for the silent-overwrite bug Phase 4 §2
// closes — the pre-Phase-4 unconditional UpdateOneID would have
// stomped the cancel and silently served tombstoned bytes.
func TestWorker_CASGuardLetsCancelWin(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)

	// Slow handler: blocks on a channel the test controls. Lets us
	// inject the cancel between claim and download.
	gate := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-gate
		w.Write([]byte("late-bytes"))
	}))
	defer srv.Close()

	seedMedia(t, client, ctx, "M-race", time.Now())
	seedMediaRawOutput(t, client, ctx, syncEventID, "M-race", srv.URL)
	jobID := seedPendingJob(t, client, ctx, syncEventID, "M-race")

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	worker := pkgsync.NewAttachmentWorker(svc, pkgsync.WithHTTPDoer(srv.Client()))

	// Run the worker in the background — it will block in the http handler
	// until we release the gate.
	done := make(chan error, 1)
	go func() {
		_, err := worker.Run(ctx)
		done <- err
	}()

	// Wait for the worker to actually be in the download.
	for hits.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// Cancel the job out-of-band (simulating the tombstone cascade).
	_, err := client.AttachmentJob.Update().
		Where(attachmentjob.IDEQ(jobID)).
		SetStatus(attachmentjob.StatusCanceled).
		ClearClaimedBy().
		ClearClaimedAt().
		Save(ctx)
	require.NoError(t, err)

	// Release the download. The worker will compute the hash, try the
	// CAS-guarded UPDATE, match 0 rows, log + return nil. Final state:
	// job stays canceled.
	close(gate)
	require.NoError(t, <-done)

	got := client.AttachmentJob.GetX(ctx, jobID)
	assert.Equal(t, attachmentjob.StatusCanceled, got.Status,
		"§2 CAS guard MUST preserve the cancel — pre-Phase-4 this was the silent-overwrite bug")
	assert.Nil(t, got.AttachmentID, "no attachment linkage written under the lost CAS")

	// The attachment row itself may have been inserted as a side effect
	// (content-addressed by sha256, harmless). Document that explicitly:
	// the dedup index makes it reusable for a future job with the same hash.
	attachments := client.Attachment.Query().Where(attachment.SourceURL(srv.URL)).AllX(ctx)
	assert.LessOrEqual(t, len(attachments), 1, "at most one attachment row per sha256, no duplicates from the race")
}

// capFullStorer simulates LocalStorer at capacity — returns the
// storage.ErrLocalStorerFull sentinel from every Upload.
type capFullStorer struct{}

func (capFullStorer) Upload(_ context.Context, _ string, body io.Reader, _ string) (string, error) {
	io.Copy(io.Discard, body) //nolint:errcheck
	return "", storage.ErrLocalStorerFull
}

// TestWorker_ErrLocalStorerFull_FailsPermanentlyOnFirstAttempt pins
// the cap-error classifier branch: a config-bound failure transitions
// straight to permanently_failed without burning two retries on a
// cause that cannot change between attempts.
func TestWorker_ErrLocalStorerFull_FailsPermanentlyOnFirstAttempt(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("bytes-we-cannot-store"))
	}))
	defer srv.Close()

	seedMedia(t, client, ctx, "M-cap", time.Now())
	seedMediaRawOutput(t, client, ctx, syncEventID, "M-cap", srv.URL)
	jobID := seedPendingJob(t, client, ctx, syncEventID, "M-cap")

	svc := pkgsync.NewService(nil, client, sqlDB, capFullStorer{}, nil)
	worker := pkgsync.NewAttachmentWorker(svc, pkgsync.WithHTTPDoer(srv.Client()))
	_, err := worker.Run(ctx)
	require.NoError(t, err)

	got := client.AttachmentJob.GetX(ctx, jobID)
	assert.Equal(t, attachmentjob.StatusPermanentlyFailed, got.Status,
		"ErrLocalStorerFull is config-bound — retry cannot help, classifier must short-circuit to permanent")
	assert.Equal(t, 1, got.AttemptCount, "no retries burned on permanent classification")
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "local storer at capacity")
}

// TestWorker_KeyPrefix_Empty preserves today's "media/<key>/<hash>"
// shape — regression-safe for the existing job backlog.
func TestWorker_KeyPrefix_Empty(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("bytes"))
	}))
	defer srv.Close()

	seedMedia(t, client, ctx, "M-noprefix", time.Now())
	seedMediaRawOutput(t, client, ctx, syncEventID, "M-noprefix", srv.URL)
	seedPendingJob(t, client, ctx, syncEventID, "M-noprefix")

	rec := &recordingStorer{}
	svc := pkgsync.NewService(nil, client, sqlDB, rec, nil)
	worker := pkgsync.NewAttachmentWorker(svc, pkgsync.WithHTTPDoer(srv.Client()))
	_, err := worker.Run(ctx)
	require.NoError(t, err)

	got := rec.lastKey(t)
	assert.True(t, len(got) > len("media/M-noprefix/"), "key should include hash suffix")
	assert.Contains(t, got, "media/M-noprefix/")
	assert.False(t, len(got) > 0 && got[0] == '/', "no leading slash for empty prefix")
}

// TestWorker_KeyPrefix_NonEmpty prepends the prefix and auto-normalizes
// the trailing slash so "drain-test" and "drain-test/" both produce
// "drain-test/media/..." keys.
func TestWorker_KeyPrefix_NonEmpty(t *testing.T) {
	for _, prefix := range []string{"drain-test", "drain-test/"} {
		t.Run(prefix, func(t *testing.T) {
			client, sqlDB := testutil.NewTestDBWithSQL(t)
			ctx := context.Background()
			syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte("bytes"))
			}))
			defer srv.Close()

			seedMedia(t, client, ctx, "M-prefix", time.Now())
			seedMediaRawOutput(t, client, ctx, syncEventID, "M-prefix", srv.URL)
			seedPendingJob(t, client, ctx, syncEventID, "M-prefix")

			rec := &recordingStorer{}
			svc := pkgsync.NewService(nil, client, sqlDB, rec, nil)
			worker := pkgsync.NewAttachmentWorker(svc,
				pkgsync.WithHTTPDoer(srv.Client()),
				pkgsync.WithKeyPrefix(prefix),
			)
			_, err := worker.Run(ctx)
			require.NoError(t, err)

			got := rec.lastKey(t)
			assert.True(t, len(got) > len("drain-test/media/M-prefix/"), "key should include hash")
			assert.Contains(t, got, "drain-test/media/M-prefix/")
			assert.False(t, len(got) > 0 && got[0] == '/', "no leading slash even with prefix")
		})
	}
}

// TestWorker_Sha256Dedup: two jobs against different URLs that resolve
// to identical bytes share one attachment row.
func TestWorker_Sha256Dedup(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceMedia)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("identical-bytes"))
	}))
	defer srv.Close()

	for _, k := range []string{"M-dup1", "M-dup2"} {
		seedMedia(t, client, ctx, k, time.Now())
		seedMediaRawOutput(t, client, ctx, syncEventID, k, srv.URL+"/"+k)
		seedPendingJob(t, client, ctx, syncEventID, k)
	}

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)
	worker := pkgsync.NewAttachmentWorker(svc, pkgsync.WithHTTPDoer(srv.Client()))
	_, err := worker.Run(ctx)
	require.NoError(t, err)

	count := client.Attachment.Query().CountX(ctx)
	assert.Equal(t, 1, count, "two jobs, same bytes → exactly one attachment row")
}
