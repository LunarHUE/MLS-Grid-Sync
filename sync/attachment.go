package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	entdialectsql "entgo.io/ent/dialect/sql"
	"github.com/lunarhue/libs-go/log"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/attachment"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/attachmentjob"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/storage"
	"golang.org/x/sync/errgroup"
)

// blockingJobStatuses are the AttachmentJob statuses that, when a more
// recent job exists for a MediaKey with media_modified_at >= the incoming
// MediaModificationTimestamp, suppress a re-enqueue. Notably absent:
//
//   - canceled — tombstone-cascade output (§3). A re-listed property's
//     media must be downloadable again, not silently blocked.
//   - permanently_failed — 3-attempt-exhausted. A new revision deserves
//     a fresh shot (the failure may have been transient).
//
// See Phase 4 plan §6.
var blockingJobStatuses = []attachmentjob.Status{
	attachmentjob.StatusPending,
	attachmentjob.StatusRetrying,
	attachmentjob.StatusInProgress,
	attachmentjob.StatusSucceeded,
}

// enqueueProgressEvery is the per-record cadence for in-loop progress
// logging. Matches the processor passes' 500 so the operator sees the
// same beat across the init phases. The loop's full-corpus cost on
// fresh DBs is dominated by per-record round-trips (one SELECT + one
// INSERT, autocommit), so the silent variant of this loop looked dead
// during init on 2026-06-11 with 586k media records to enqueue.
const enqueueProgressEvery = 500

// EnqueueAttachmentJobs inserts an AttachmentJob for each visible Media
// record whose content has changed (or that we've never seen before),
// gated by MediaModificationTimestamp.
//
// Phase 4 §6: prior to this rewrite, every sync run blindly inserted a
// pending job per MediaKey — sha256 dedup kept the bytes clean but
// attachment_job rows accumulated and stale workers re-downloaded
// already-fresh images. Now: look up the most recent blocking-status job
// for the key; if it covers this revision (media_modified_at >= incoming
// timestamp), skip; otherwise enqueue with media_modified_at stamped.
func (s *Service) EnqueueAttachmentJobs(ctx context.Context, syncEventID uuid.UUID, records []json.RawMessage) error {
	total := len(records)
	if total >= enqueueProgressEvery {
		log.Infof("enqueue: starting pass over %d media records", total)
	}
	start := time.Now()
	var enqueued, skipped, processed int
	for _, raw := range records {
		processed++
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("unmarshal media record: %w", err)
		}
		if canView, ok := record["MlgCanView"].(bool); ok && !canView {
			continue
		}
		mediaKey, ok := record["MediaKey"].(string)
		if !ok || mediaKey == "" {
			continue
		}

		// Parse MediaModificationTimestamp (falls back to ModificationTimestamp
		// — Media records carry both; the more specific Media* field wins).
		incoming := parseEnqueueTimestamp(record)

		// Look up the most recent blocking-status job for this MediaKey.
		mostRecent, err := s.dbClient.AttachmentJob.Query().
			Where(
				attachmentjob.MediaKey(mediaKey),
				attachmentjob.StatusIn(blockingJobStatuses...),
			).
			Order(attachmentjob.ByCreatedAt(entdialectsql.OrderDesc())).
			First(ctx)
		if err == nil && mostRecent.MediaModifiedAt != nil && !mostRecent.MediaModifiedAt.Before(incoming) {
			// Existing job covers this revision (or a newer one). Skip.
			log.Debugf("media %s skip (current)", mediaKey)
			skipped++
			continue
		}

		creator := s.dbClient.AttachmentJob.Create().
			SetMediaKey(mediaKey).
			SetSyncEventID(syncEventID)
		if !incoming.IsZero() {
			creator.SetMediaModifiedAt(incoming)
		}
		if err := creator.Exec(ctx); err != nil {
			return fmt.Errorf("enqueue job %s: %w", mediaKey, err)
		}
		log.Debugf("media %s enqueue (new revision)", mediaKey)
		enqueued++

		if processed%enqueueProgressEvery == 0 {
			log.Infof("enqueue: %d/%d processed (%d enqueued, %d skipped)",
				processed, total, enqueued, skipped)
		}
	}
	if total >= enqueueProgressEvery {
		elapsed := time.Since(start)
		rate := float64(processed) / elapsed.Seconds()
		log.Infof("enqueue: pass complete — %d records in %s (%.1f/s): %d enqueued, %d skipped",
			processed, elapsed.Round(time.Second), rate, enqueued, skipped)
	} else if enqueued+skipped > 0 {
		log.Infof("media jobs: %d enqueued, %d skipped (already current)", enqueued, skipped)
	}
	return nil
}

// parseEnqueueTimestamp pulls the most appropriate timestamp from a Media
// payload for the enqueue check. MediaModificationTimestamp is preferred
// (per-image revision); falls back to ModificationTimestamp (record-level)
// and finally to zero, which forces an enqueue.
func parseEnqueueTimestamp(record map[string]any) time.Time {
	for _, key := range []string{"MediaModificationTimestamp", "ModificationTimestamp"} {
		if ts, ok := record[key].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// AttachmentWorker processes pending AttachmentJob rows.
type AttachmentWorker struct {
	svc       *Service
	workerID  string
	limiter   Limiter
	httpDoer  HTTPDoer
	lease     time.Duration
	batchSize int
	// keyPrefix is prepended to every uploaded object key. Empty
	// preserves today's "media/<key>/<hash>" shape (regression-safe
	// for the existing backlog). Applied at processing time, not
	// enqueue time — a config change between enqueue and processing
	// means subsequent uploads use the new prefix; jobs already
	// uploaded retain their original key.
	keyPrefix string
}

// Limiter is anything the worker can Wait(ctx) on before a media
// download. Wired from golang.org/x/time/rate.Limiter in production;
// kept as an interface so the worker tests can drop in a nop.
type Limiter interface {
	Wait(ctx context.Context) error
}

// HTTPDoer matches the subset of *http.Client used by downloadBytes;
// httptest.Server-backed tests stub it.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// noopLimiter satisfies Limiter without rate limiting — used when no
// limiter is wired in (e.g. unit tests that don't care).
type noopLimiter struct{}

func (noopLimiter) Wait(context.Context) error { return nil }

// AttachmentWorkerOption configures the worker at construction.
type AttachmentWorkerOption func(*AttachmentWorker)

// WithMediaLimiter installs the media-download rate limiter.
// Production wiring uses rate.NewLimiter(rate.Limit(cfg.MediaDownloadRPS), 1).
func WithMediaLimiter(l Limiter) AttachmentWorkerOption {
	return func(w *AttachmentWorker) { w.limiter = l }
}

// WithHTTPDoer overrides the worker's http.Doer (tests).
func WithHTTPDoer(d HTTPDoer) AttachmentWorkerOption {
	return func(w *AttachmentWorker) { w.httpDoer = d }
}

// WithClaimLease overrides the default 10-minute claim lease.
func WithClaimLease(d time.Duration) AttachmentWorkerOption {
	return func(w *AttachmentWorker) { w.lease = d }
}

// WithBatchSize overrides the default 50-job claim batch.
func WithBatchSize(n int) AttachmentWorkerOption {
	return func(w *AttachmentWorker) { w.batchSize = n }
}

// WithKeyPrefix sets the object-key prefix prepended to every upload.
// Empty (the default) preserves "media/<key>/<hash>". A non-empty
// prefix is normalized to end with "/" — "drain-test" and
// "drain-test/" produce the same result.
func WithKeyPrefix(prefix string) AttachmentWorkerOption {
	return func(w *AttachmentWorker) {
		if prefix != "" && prefix[len(prefix)-1] != '/' {
			prefix += "/"
		}
		w.keyPrefix = prefix
	}
}

// NewAttachmentWorker constructs a worker with the §1 worker_id and
// the production HTTP client (30s timeout). Pass options to install
// the media rate limiter, override the HTTP doer for testing, etc.
func NewAttachmentWorker(svc *Service, opts ...AttachmentWorkerOption) *AttachmentWorker {
	w := &AttachmentWorker{
		svc:      svc,
		workerID: NewWorkerID(),
		limiter:  noopLimiter{},
		// §5 — bound per-job time; http.DefaultClient has no timeout
		// and can hang a worker on a stuck connection forever.
		httpDoer:  &http.Client{Timeout: 30 * time.Second},
		lease:     DefaultClaimLease,
		batchSize: DefaultClaimBatch,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// WorkerID returns the identity stamped into claimed_by. Exposed for
// tests that need to inject competing workers.
func (w *AttachmentWorker) WorkerID() string { return w.workerID }

// RunResult signals to the polling loop whether the cycle did any work, so
// it can detect busy→empty edges and emit an "idling" line once instead of
// per poll. The terminal-status counters are populated only when Worked is
// true; the polling loop doesn't use them but tests assert against them.
type RunResult struct {
	Worked            bool
	Succeeded         int64
	Retrying          int64
	PermanentlyFailed int64
	LostCAS           int64
}

// cycleStats accumulates per-job outcomes during the errgroup fan-out.
// Atomic so the 8 concurrent goroutines spawned at line ~235 can bump
// counters without contention. Reset per cycle (held by-value inside Run).
type cycleStats struct {
	succeeded         atomic.Int64
	retrying          atomic.Int64
	permanentlyFailed atomic.Int64
	lostCAS           atomic.Int64
}

func (s *cycleStats) add(r jobResult) {
	switch r {
	case jobSucceeded:
		s.succeeded.Add(1)
	case jobRetrying:
		s.retrying.Add(1)
	case jobPermanentlyFailed:
		s.permanentlyFailed.Add(1)
	case jobLostCAS:
		s.lostCAS.Add(1)
	}
}

// jobResult is the terminal status reported by a single processJob call.
// Used to render the per-job DEBUG line and to accumulate cycleStats so
// the cycle-complete INFO line is honest.
type jobResult int

const (
	jobResultUnknown jobResult = iota
	jobSucceeded
	jobRetrying
	jobPermanentlyFailed
	jobLostCAS
)

func (r jobResult) String() string {
	switch r {
	case jobSucceeded:
		return "succeeded"
	case jobRetrying:
		return "retrying"
	case jobPermanentlyFailed:
		return "permanently-failed"
	case jobLostCAS:
		return "lost-cas"
	}
	return "unknown"
}

// Run processes one poll cycle: reap stale claims → claim a batch via
// SKIP LOCKED → dispatch claimed jobs to the 8-way pool. Returns a
// RunResult so the polling loop can detect idle transitions and emit the
// "idling" line once per busy→empty edge instead of per poll tick.
func (w *AttachmentWorker) Run(ctx context.Context) (RunResult, error) {
	// 1) Reap: return crashed-worker leases to pending so we can re-claim
	// them. attempt_count is unchanged — see §4.
	if n, err := w.svc.ReapStaleClaims(ctx, w.lease); err != nil {
		log.Errorf("reap stale claims: %v", err)
	} else if n > 0 {
		log.Infof("reaped %d stale in_progress job(s)", n)
	}

	// 2) Claim: SKIP LOCKED so a sibling worker takes a disjoint set.
	ids, err := w.svc.ClaimBatch(ctx, w.workerID, w.batchSize)
	if err != nil {
		return RunResult{}, fmt.Errorf("claim batch: %w", err)
	}
	if len(ids) == 0 {
		return RunResult{}, nil
	}
	log.Infof("worker %s claimed %d job(s)", w.workerID, len(ids))

	// 3) Process: load media keys and fan out.
	jobs, err := w.svc.dbClient.AttachmentJob.Query().
		Where(attachmentjob.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return RunResult{Worked: true}, fmt.Errorf("load claimed jobs: %w", err)
	}

	var stats cycleStats
	sem := make(chan struct{}, 8)
	g, gctx := errgroup.WithContext(ctx)
	for _, job := range jobs {
		sem <- struct{}{}
		g.Go(func() error {
			defer func() { <-sem }()
			return w.processJob(gctx, job.ID, job.MediaKey, job.AttemptCount, &stats)
		})
	}
	err = g.Wait()

	result := RunResult{
		Worked:            true,
		Succeeded:         stats.succeeded.Load(),
		Retrying:          stats.retrying.Load(),
		PermanentlyFailed: stats.permanentlyFailed.Load(),
		LostCAS:           stats.lostCAS.Load(),
	}
	log.Infof("worker %s: cycle complete — %d succeeded, %d retrying, %d permanently-failed, %d lost-cas",
		w.workerID, result.Succeeded, result.Retrying, result.PermanentlyFailed, result.LostCAS)
	return result, err
}

func (w *AttachmentWorker) processJob(ctx context.Context, jobID uuid.UUID, mediaKey string, priorAttempts int, stats *cycleStats) error {
	start := time.Now()
	var result jobResult
	var sizeBytes int
	dedupHit := false

	// Paired with the finish line below — a wedged download is
	// diagnosable from the log alone (start logged, no finish yet).
	// priorAttempts is the count BEFORE this run; the attempt about
	// to execute is priorAttempts+1.
	log.Debugf("worker %s: job %s starting (media %s, attempt %d)",
		w.workerID, jobID, mediaKey, priorAttempts+1)

	defer func() {
		if result == jobResultUnknown {
			return // a DB-level failure inside the CAS helpers — no honest summary to print
		}
		stats.add(result)
		log.Debugf("worker %s: job %s %s in %s (%dB, dedup=%v)",
			w.workerID, jobID, result, time.Since(start).Round(time.Millisecond), sizeBytes, dedupHit)
	}()

	// fail records the result then forwards the failJob outcome upward.
	// Centralizing the result-assignment keeps the eight failure call
	// sites below from drifting.
	fail := func(cause error) error {
		r, e := w.failJob(ctx, jobID, cause)
		result = r
		return e
	}

	imageURL, err := w.resolveMediaURL(ctx, mediaKey)
	if err != nil {
		return fail(err)
	}

	// §5: cap media download rate. The OData API limiter in mls/client.go
	// is independent — these are separate caps in the data license.
	if err := w.limiter.Wait(ctx); err != nil {
		return fail(fmt.Errorf("rate limit wait: %w", err))
	}

	data, contentType, err := downloadBytes(ctx, w.httpDoer, imageURL)
	if err != nil {
		return fail(err)
	}
	sizeBytes = len(data)

	hash := sha256hex(data)

	// Deduplicate: reuse existing attachment if same content hash exists.
	existing, err := w.svc.dbClient.Attachment.Query().
		Where(attachment.SourceHash(hash)).
		Only(ctx)

	var attachmentID uuid.UUID
	if err == nil {
		attachmentID = existing.ID
		dedupHit = true
	} else {
		key := w.keyPrefix + "media/" + mediaKey + "/" + hash
		hostURL, err := w.svc.storer.Upload(ctx, key, bytes.NewReader(data), contentType)
		if err != nil {
			return fail(fmt.Errorf("s3 upload: %w", err))
		}
		created, err := w.svc.dbClient.Attachment.Create().
			SetSourceURL(imageURL).
			SetSourceHash(hash).
			SetHostURL(hostURL).
			SetMimeType(contentType).
			SetSizeBytes(sizeBytes).
			Save(ctx)
		if err != nil {
			return fail(fmt.Errorf("create attachment: %w", err))
		}
		attachmentID = created.ID
	}

	// §2: CAS guard. If the job was canceled (or reaped) since we claimed
	// it, the WHERE clause matches zero rows. We log and discard our work;
	// the attachment row is sha256-content-addressed and harmless to keep.
	r, casErr := w.compareAndSetSucceeded(ctx, jobID, attachmentID)
	result = r
	return casErr
}

// compareAndSetSucceeded transitions the job to succeeded only if it's
// still in_progress under our worker_id — see §2. Returns nil error even
// when the guard matches zero rows, because the cancel/reap that beat us
// is not the worker's failure; it's an expected race. The jobResult
// disambiguates so processJob can stamp the right cycle counter.
func (w *AttachmentWorker) compareAndSetSucceeded(ctx context.Context, jobID uuid.UUID, attachmentID uuid.UUID) (jobResult, error) {
	const guarded = `UPDATE attachment_job
                       SET status = 'succeeded',
                           attachment_id = $1,
                           modified_at = now()
                     WHERE attachment_job_id = $2
                       AND status = 'in_progress'
                       AND claimed_by = $3`
	res, err := w.svc.sqlDB.ExecContext(ctx, guarded, attachmentID, jobID, w.workerID)
	if err != nil {
		return jobResultUnknown, fmt.Errorf("cas succeed: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		log.Infof("worker %s: CAS-success on job %s matched 0 rows (canceled or reaped mid-download — discarding)", w.workerID, jobID)
		return jobLostCAS, nil
	}
	return jobSucceeded, nil
}

// resolveMediaURL looks up the MediaURL field from the most recent raw_output
// row for the given mediaKey.
func (w *AttachmentWorker) resolveMediaURL(ctx context.Context, mediaKey string) (string, error) {
	raw, err := w.svc.dbClient.RawOutput.Query().
		Where(
			rawoutput.ResourceEQ(rawoutput.ResourceMedia),
			rawoutput.SourceKey(mediaKey),
		).
		Order(rawoutput.ByCreatedAt(entdialectsql.OrderDesc())).
		First(ctx)
	if err != nil {
		return "", fmt.Errorf("find raw output for media key %s: %w", mediaKey, err)
	}
	mediaURL, ok := raw.Payload["MediaURL"].(string)
	if !ok || mediaURL == "" {
		return "", fmt.Errorf("no MediaURL in payload for media key %s", mediaKey)
	}
	return mediaURL, nil
}

// failJob CAS-transitions the job to retrying (or permanently_failed
// at attempt 3). Same §2 guard as compareAndSetSucceeded: if the row
// no longer matches in_progress + claimed_by=$me, log and yield.
// Critically, attempt_count is read inside the guard and only
// incremented when the UPDATE actually lands — a lost CAS does NOT
// consume a retry.
func (w *AttachmentWorker) failJob(ctx context.Context, jobID uuid.UUID, cause error) (jobResult, error) {
	job, err := w.svc.dbClient.AttachmentJob.Get(ctx, jobID)
	if err != nil {
		return jobResultUnknown, err
	}
	newCount := job.AttemptCount + 1
	status := attachmentjob.StatusRetrying
	if newCount >= 3 {
		status = attachmentjob.StatusPermanentlyFailed
	}
	// Cap-exceeded: a config-bound limit, not a transient remote
	// failure. Retrying cannot help — surface loudly as permanent on
	// the first attempt rather than burning two retries on a cause
	// that won't change. Other failure semantics (404 ladders, timeout
	// retries, etc.) are untouched.
	if errors.Is(cause, storage.ErrLocalStorerFull) {
		status = attachmentjob.StatusPermanentlyFailed
	}
	errMsg := cause.Error()

	const guarded = `UPDATE attachment_job
                       SET status = $1,
                           attempt_count = $2,
                           last_error = $3,
                           modified_at = now()
                     WHERE attachment_job_id = $4
                       AND status = 'in_progress'
                       AND claimed_by = $5`
	res, err := w.svc.sqlDB.ExecContext(ctx, guarded, string(status), newCount, errMsg, jobID, w.workerID)
	if err != nil {
		return jobResultUnknown, fmt.Errorf("cas fail: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		log.Infof("worker %s: CAS-fail on job %s matched 0 rows (canceled or reaped — attempt_count unchanged)", w.workerID, jobID)
		return jobLostCAS, nil
	}
	if status == attachmentjob.StatusPermanentlyFailed {
		return jobPermanentlyFailed, nil
	}
	return jobRetrying, nil
}

func downloadBytes(ctx context.Context, doer HTTPDoer, imageURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download %s: %w", imageURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download %s: status %d", imageURL, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read body: %w", err)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	return data, ct, nil
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
