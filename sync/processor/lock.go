package processor

import (
	"context"
	"database/sql"
	"fmt"
)

// acquireResourceLock takes a Postgres session-scoped advisory lock keyed by
// "processor:<resource>", pinned to a single connection for the lifetime of
// the returned release function.
//
// Why pinned: pg_advisory_lock is session-scoped (lives on one specific
// connection). If we ran it through the normal *sql.DB pool, the connection
// that acquired the lock would return to the pool and our subsequent
// per-record transactions could run on different connections that don't hold
// the lock — silently no-op. Pinning a *sql.Conn keeps the lock alive until
// we Close() it.
//
// The caller MUST invoke the returned release function (typically `defer`),
// which runs pg_advisory_unlock on the pinned conn and then Close()s it
// (returning it to the pool).
func acquireResourceLock(ctx context.Context, db *sql.DB, resource string) (release func(), err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("processor lock: pin conn: %w", err)
	}

	key := "processor:" + resource
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("processor lock: acquire %q: %w", key, err)
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		// Best-effort unlock on the same pinned conn, then return it to the pool.
		// Use background context because the caller's ctx may already be cancelled.
		_, _ = conn.ExecContext(context.Background(),
			"SELECT pg_advisory_unlock(hashtext($1))", key)
		_ = conn.Close()
	}, nil
}
