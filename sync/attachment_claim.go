package sync

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
)

// DefaultClaimLease is the wall-clock window a worker holds an
// in_progress claim before the reaper returns the job to pending. The
// 10-minute default is generous: it covers slow downloads even under
// the §5 media RPS limiter, while still surfacing crashed workers
// within a useful timeframe. Used by ReapStaleClaims.
const DefaultClaimLease = 10 * time.Minute

// DefaultClaimBatch is the per-poll claim batch size. Bound by the
// §1 constraint batch <= lease * mediaRPS * 0.5 — at the 10-minute
// lease and 1 RPS media defaults the ceiling is 300; 50 leaves plenty
// of room for download-time variance without ever sitting on a claim
// long enough to be reaped.
const DefaultClaimBatch = 50

// NewWorkerID returns an identity unique per worker process. The format
// is hostname:pid:uuidv7 — host gives an operator a thing to grep, pid
// disambiguates colocated workers, and the uuid survives a quick
// process restart that re-uses the pid.
func NewWorkerID() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	return fmt.Sprintf("%s:%d:%s", hostname, os.Getpid(), uuid.Must(uuid.NewV7()).String())
}

// ClaimBatch atomically claims up to limit pending/retrying jobs for
// processing by workerID. It returns the ids of jobs the caller now
// owns. The transaction uses FOR UPDATE SKIP LOCKED so a concurrent
// worker selecting in parallel sees a disjoint set instead of blocking
// or double-claiming.
//
// Phase 4 §1. ent's fluent API doesn't expose SKIP LOCKED; this uses
// raw SQL via *sql.DB — same precedent as the Phase 3 AfterPass.
func (s *Service) ClaimBatch(ctx context.Context, workerID string, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = DefaultClaimBatch
	}

	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1) Select-for-update-skip-locked the eligible ids.
	selectSQL := `SELECT attachment_job_id FROM attachment_job
                  WHERE status IN ('pending', 'retrying')
                  ORDER BY created_at
                  LIMIT $1
                  FOR UPDATE SKIP LOCKED`
	rows, err := tx.QueryContext(ctx, selectSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("claim select: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("claim scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim rows: %w", err)
	}
	if len(ids) == 0 {
		// Nothing to claim — commit the empty tx to release any momentary
		// catalog locks taken by the empty SELECT.
		return nil, tx.Commit()
	}

	// 2) Transition the locked rows to in_progress and stamp claim metadata.
	updateSQL := `UPDATE attachment_job
                     SET status = 'in_progress',
                         claimed_at = now(),
                         claimed_by = $1,
                         modified_at = now()
                   WHERE attachment_job_id = ANY($2)`
	if _, err := tx.ExecContext(ctx, updateSQL, workerID, uuidArray(ids)); err != nil {
		return nil, fmt.Errorf("claim update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim commit: %w", err)
	}
	return ids, nil
}

// ReapStaleClaims returns in_progress jobs whose claimed_at is older
// than lease to the pending state. claimed_at and claimed_by are
// cleared. attempt_count is NOT incremented — a crashed worker is not
// the job's fault and must not consume a retry. Returns the count of
// rows reaped.
//
// Phase 4 §4. Called by the worker at the top of each poll cycle.
func (s *Service) ReapStaleClaims(ctx context.Context, lease time.Duration) (int, error) {
	if lease <= 0 {
		lease = DefaultClaimLease
	}
	updateSQL := `UPDATE attachment_job
                     SET status = 'pending',
                         claimed_at = NULL,
                         claimed_by = NULL,
                         modified_at = now()
                   WHERE status = 'in_progress'
                     AND claimed_at IS NOT NULL
                     AND claimed_at < now() - $1::interval`
	res, err := s.sqlDB.ExecContext(ctx, updateSQL, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("reap: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reap rows affected: %w", err)
	}
	return int(n), nil
}

// uuidArray is a thin adapter so we can pass []uuid.UUID directly to
// $1::uuid[] in pq. lib/pq accepts the slice via its Array helper, but
// the standard driver path requires explicit driver.Valuer wrapping.
// We use a one-shot fmt of the literal array since UUIDs are
// non-quoting and well-formed.
//
// (postgres uuid[] literal: {uuid-1,uuid-2,...})
func uuidArray(ids []uuid.UUID) string {
	if len(ids) == 0 {
		return "{}"
	}
	out := make([]byte, 0, 1+len(ids)*37)
	out = append(out, '{')
	for i, id := range ids {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, id.String()...)
	}
	out = append(out, '}')
	return string(out)
}
