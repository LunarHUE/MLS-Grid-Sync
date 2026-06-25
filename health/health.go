// Package health provides a reusable, dependency-injected health service that
// answers three operationally distinct questions about a serve process:
//
//   - Live  — is the process alive? (no external dependencies)
//   - Ready — can it serve requests safely? (DB reachable, schema present, config sane)
//   - Sync  — is MLS sync within configured freshness/backlog thresholds?
//
// It backs the /healthz, /readyz, and /syncz HTTP endpoints today and is
// shaped so later consumers (the `sync status` CLI, a GraphQL syncStatus
// query, metrics) can wrap the same Service without change. It performs only
// read-only DB queries and never runs migrations.
package health

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/processorcursor"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	mlssync "github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

// Status is the per-check verdict. A HealthStatus is Healthy when no check is
// StatusFail; StatusWarn and StatusSkipped never make it unhealthy.
type Status string

const (
	StatusPass    Status = "pass"
	StatusWarn    Status = "warn"
	StatusFail    Status = "fail"
	StatusSkipped Status = "skipped"
)

const (
	// dbPingTimeout bounds the readiness ping; mirrors the prior /healthz ping.
	dbPingTimeout = 2 * time.Second
	// checkTimeout bounds each individual sync/schema query so a slow DB can't
	// stall the whole endpoint. Fresh per check.
	checkTimeout = 5 * time.Second
)

// HealthCheck is a single named verdict.
type HealthCheck struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
}

// HealthStatus is the stable JSON body returned by every health endpoint.
type HealthStatus struct {
	Healthy bool          `json:"healthy"`
	Checks  []HealthCheck `json:"checks"`
}

// Thresholds are the operator-tunable limits that gate /syncz.
type Thresholds struct {
	SyncMaxStaleness      time.Duration
	MaxRawPending         int
	MaxAttachmentFailures int
}

// Service evaluates health checks against injected dependencies. db and ping
// may be nil — Live works regardless, and Ready/Sync fail cleanly rather than
// panicking (so a partially-wired caller still gets an honest answer).
type Service struct {
	db      *ent.Client
	ping    func(context.Context) error
	trigram func(context.Context) error
	th      Thresholds
	now     func() time.Time
}

// NewService wires a health service. A nil now defaults to time.Now.
func NewService(db *ent.Client, ping func(context.Context) error, th Thresholds, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, ping: ping, th: th, now: now}
}

// WithTrigramProbe registers the readiness probe for the pg_trgm extension the
// fuzzy address-search resolvers depend on. serve wires it to
// search.CheckExtension over the raw *sql.DB. When unset, the readiness
// `trigram` check reports skipped (never a fail) so a partially-wired caller
// still gets an honest answer. Returns the service for chaining.
func (s *Service) WithTrigramProbe(fn func(context.Context) error) *Service {
	s.trigram = fn
	return s
}

// Live reports process liveness only — no DB, MLS, or storage dependency. It
// is always healthy if the process can answer the request at all.
func (s *Service) Live(_ context.Context) HealthStatus {
	return finalize([]HealthCheck{pass("process", "process alive")})
}

// Ready reports whether the service can serve requests safely: DB reachable,
// required schema present, config loaded, and health thresholds valid.
func (s *Service) Ready(ctx context.Context) HealthStatus {
	checks := []HealthCheck{
		safeCheck("database", func() HealthCheck { return s.checkDatabase(ctx) }),
		safeCheck("schema", func() HealthCheck { return s.checkSchema(ctx) }),
		safeCheck("trigram", func() HealthCheck { return s.checkTrigram(ctx) }),
		// Startup-derived: serve would not be answering if config failed to load
		// or the GraphQL handler failed to build. Named so they read as
		// startup facts, not fresh probes.
		pass("config_loaded", "config loaded during serve startup"),
		pass("graphql_initialized", "GraphQL handler initialized during serve startup"),
		safeCheck("health_config", s.checkHealthConfig),
	}
	return finalize(checks)
}

// Sync reports whether MLS sync is within thresholds. Until the initial import
// completes for every required resource, initial_import is the single hard
// fail and all downstream checks are reported skipped (one honest reason on a
// fresh deploy).
func (s *Service) Sync(ctx context.Context) HealthStatus {
	if s.db == nil {
		return finalize([]HealthCheck{fail("initial_import", "database handle not configured")})
	}

	ic := safeCheck("initial_import", func() HealthCheck { return s.checkInitialImport(ctx) })
	checks := []HealthCheck{ic}

	if ic.Status != StatusPass {
		const reason = "skipped until initial import completes"
		for _, r := range processor.FetchableResources {
			checks = append(checks, skip(fetchFreshnessName(r), reason))
		}
		checks = append(checks,
			skip("processing_freshness", reason),
			skip("raw_backlog", reason),
			skip("attachment_failures", reason),
			skip("resource_state", reason),
		)
		return finalize(checks)
	}

	for _, r := range processor.FetchableResources {
		r := r
		checks = append(checks, safeCheck(fetchFreshnessName(r), func() HealthCheck {
			return s.checkFetchFreshness(ctx, r)
		}))
	}

	// raw_backlog is the hard fail for "processing is stuck"; processing_freshness
	// stays advisory and only borrows the backlog verdict to sharpen its message.
	backlog := safeCheck("raw_backlog", func() HealthCheck { return s.checkRawBacklog(ctx) })
	checks = append(checks,
		safeCheck("processing_freshness", func() HealthCheck {
			return s.checkProcessingFreshness(ctx, backlog.Status == StatusFail)
		}),
		backlog,
		safeCheck("attachment_failures", func() HealthCheck { return s.checkAttachmentFailures(ctx) }),
		safeCheck("resource_state", func() HealthCheck { return s.checkResourceState(ctx) }),
	)
	return finalize(checks)
}

// --- readiness checks ---

func (s *Service) checkDatabase(ctx context.Context) HealthCheck {
	if s.ping == nil {
		return fail("database", "ping not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, dbPingTimeout)
	defer cancel()
	if err := s.ping(cctx); err != nil {
		return fail("database", "database unreachable: "+sanitizeError(err))
	}
	return pass("database", "database reachable")
}

func (s *Service) checkSchema(ctx context.Context) HealthCheck {
	if s.db == nil {
		return fail("schema", "database handle not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	// A clean (false, nil) means "table exists, zero rows" — fine on a freshly
	// migrated DB. Only a query error (missing relation) fails readiness.
	if _, err := s.db.ProcessorCursor.Query().Exist(cctx); err != nil {
		return fail("schema", "required schema missing or incompatible: "+sanitizeError(err))
	}
	return pass("schema", "required schema present")
}

// checkTrigram verifies the pg_trgm extension the fuzzy address-search
// resolvers depend on is installed. Without it, every word_similarity(...)
// query 500s at request time, so a serve process missing it is not ready to
// serve those endpoints safely. The probe is injected (search.CheckExtension
// over the raw *sql.DB); when unset the check is skipped rather than failed so
// a partially-wired caller still gets an honest answer.
func (s *Service) checkTrigram(ctx context.Context) HealthCheck {
	const name = "trigram"
	if s.trigram == nil {
		return skip(name, "pg_trgm probe not configured")
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := s.trigram(cctx); err != nil {
		return fail(name, "fuzzy search unavailable: "+sanitizeError(err)+
			" — run `mls-cli migrate` to install pg_trgm")
	}
	return pass(name, "pg_trgm extension available")
}

func (s *Service) checkHealthConfig() HealthCheck {
	var probs []string
	if s.th.SyncMaxStaleness <= 0 {
		probs = append(probs, "sync_max_staleness must be > 0")
	}
	if s.th.MaxRawPending < 0 {
		probs = append(probs, "max_raw_pending must be >= 0")
	}
	if s.th.MaxAttachmentFailures < 0 {
		probs = append(probs, "max_attachment_failures must be >= 0")
	}
	if len(probs) > 0 {
		return fail("health_config", "invalid health thresholds: "+strings.Join(probs, "; "))
	}
	return pass("health_config", "health thresholds valid")
}

// --- sync checks ---

func (s *Service) checkInitialImport(ctx context.Context) HealthCheck {
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var missing []string
	for _, r := range processor.FetchableResources {
		n, err := s.db.SyncEvent.Query().Where(
			syncevent.ResourceEQ(syncevent.Resource(r)),
			syncevent.RunTypeEQ(syncevent.RunTypeBackfill),
			syncevent.StatusEQ(syncevent.StatusSuccess),
		).Count(cctx)
		if err != nil {
			return fail("initial_import", "initial import check failed: "+sanitizeError(err))
		}
		if n == 0 {
			missing = append(missing, string(r))
		}
	}
	if len(missing) > 0 {
		return fail("initial_import", "initial import not complete for: "+strings.Join(missing, ", "))
	}
	return pass("initial_import", "initial import complete for all required resources")
}

func (s *Service) checkFetchFreshness(ctx context.Context, r rawoutput.Resource) HealthCheck {
	name := fetchFreshnessName(r)
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	ev, err := s.db.SyncEvent.Query().Where(
		syncevent.ResourceEQ(syncevent.Resource(r)),
		syncevent.StatusEQ(syncevent.StatusSuccess),
		syncevent.EndedAtNotNil(),
	).Order(ent.Desc(syncevent.FieldEndedAt)).First(cctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return fail(name, fmt.Sprintf("no successful %s fetch on record", r))
		}
		return fail(name, "fetch freshness check failed: "+sanitizeError(err))
	}
	if ev.EndedAt == nil { // defensive; EndedAtNotNil filter should prevent this
		return fail(name, fmt.Sprintf("last successful %s fetch has no end time", r))
	}
	age := s.now().Sub(*ev.EndedAt)
	if age > s.th.SyncMaxStaleness {
		return fail(name, fmt.Sprintf("last successful %s fetch was %s ago", r, age.Round(time.Second)))
	}
	return pass(name, fmt.Sprintf("last successful %s fetch %s ago", r, age.Round(time.Second)))
}

func (s *Service) checkRawBacklog(ctx context.Context) HealthCheck {
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	cursors, err := s.db.ProcessorCursor.Query().All(cctx)
	if err != nil {
		return fail("raw_backlog", "raw backlog check failed: "+sanitizeError(err))
	}
	cursorByResource := make(map[processorcursor.Resource]*uuid.UUID, len(cursors))
	for _, c := range cursors {
		cursorByResource[c.Resource] = c.LastRawOutputID
	}

	total := 0
	var missingCursor []string
	for _, r := range processor.AllValidatedResources {
		after, hasCursor := cursorByResource[processorcursor.Resource(r)]
		q := s.db.RawOutput.Query().Where(rawoutput.ResourceEQ(r))
		// No cursor row, or a cursor that has processed nothing yet (NULL
		// last_raw_output_id), means every raw row is still pending — count
		// them all rather than silently treating it as zero backlog.
		if after != nil {
			q = q.Where(rawoutput.IDGT(*after))
		}
		n, err := q.Count(cctx)
		if err != nil {
			return fail("raw_backlog", "raw backlog check failed: "+sanitizeError(err))
		}
		total += n
		if !hasCursor && n > 0 {
			missingCursor = append(missingCursor, string(r))
		}
	}

	note := ""
	if len(missingCursor) > 0 {
		note = fmt.Sprintf(" (no processor cursor yet for: %s)", strings.Join(missingCursor, ", "))
	}
	if total > s.th.MaxRawPending {
		return fail("raw_backlog", fmt.Sprintf("raw backlog %d exceeds threshold %d%s", total, s.th.MaxRawPending, note))
	}
	return pass("raw_backlog", fmt.Sprintf("%d raw records pending (threshold %d)%s", total, s.th.MaxRawPending, note))
}

func (s *Service) checkProcessingFreshness(ctx context.Context, backlogOver bool) HealthCheck {
	const name = "processing_freshness"
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	c, err := s.db.ProcessorCursor.Query().Order(ent.Desc(processorcursor.FieldModifiedAt)).First(cctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return warn(name, "no processor cursor has advanced yet")
		}
		return warn(name, "processing freshness check failed: "+sanitizeError(err))
	}
	age := s.now().Sub(c.ModifiedAt)
	if age <= s.th.SyncMaxStaleness {
		return pass(name, fmt.Sprintf("processor cursor advanced %s ago", age.Round(time.Second)))
	}
	// Never a fail: a quiet feed legitimately leaves the cursor un-advanced.
	// raw_backlog is the hard fail; here we only correlate the two symptoms.
	if backlogOver {
		return warn(name, fmt.Sprintf("processor cursor stale (%s) while backlog is over threshold", age.Round(time.Second)))
	}
	return warn(name, fmt.Sprintf("processor cursor has not advanced in %s", age.Round(time.Second)))
}

func (s *Service) checkAttachmentFailures(ctx context.Context) HealthCheck {
	const name = "attachment_failures"
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	n, err := s.db.AttachmentJob.Query().
		Where(attachmentjob.StatusEQ(attachmentjob.StatusPermanentlyFailed)).
		Count(cctx)
	if err != nil {
		return fail(name, "attachment failure check failed: "+sanitizeError(err))
	}
	if n > s.th.MaxAttachmentFailures {
		return fail(name, fmt.Sprintf("%d permanently-failed attachments exceeds threshold %d", n, s.th.MaxAttachmentFailures))
	}
	return pass(name, fmt.Sprintf("%d permanently-failed attachments (threshold %d)", n, s.th.MaxAttachmentFailures))
}

func (s *Service) checkResourceState(ctx context.Context) HealthCheck {
	const name = "resource_state"
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var stuck []string
	for _, r := range processor.FetchableResources {
		ev, err := s.db.SyncEvent.Query().
			Where(syncevent.ResourceEQ(syncevent.Resource(r))).
			Order(ent.Desc(syncevent.FieldStartedAt)).
			First(cctx)
		if err != nil {
			if ent.IsNotFound(err) {
				continue // no events; the initial_import gate owns fresh deploys
			}
			return fail(name, "resource state check failed: "+sanitizeError(err))
		}
		switch ev.Status {
		case syncevent.StatusFailed:
			stuck = append(stuck, fmt.Sprintf("%s (failed)", r))
		case syncevent.StatusRunning:
			if s.now().Sub(ev.StartedAt) > mlssync.DefaultStaleRunningThreshold {
				stuck = append(stuck, fmt.Sprintf("%s (stuck running)", r))
			}
		}
	}
	if len(stuck) > 0 {
		return fail(name, "resource(s) in a bad state: "+strings.Join(stuck, ", "))
	}
	return pass(name, "no required resource stuck in a failed state")
}

// --- helpers ---

func fetchFreshnessName(r rawoutput.Resource) string {
	return string(r) + "_fetch_freshness"
}

func pass(name, msg string) HealthCheck {
	return HealthCheck{Name: name, Status: StatusPass, Message: msg}
}
func warn(name, msg string) HealthCheck {
	return HealthCheck{Name: name, Status: StatusWarn, Message: msg}
}
func fail(name, msg string) HealthCheck {
	return HealthCheck{Name: name, Status: StatusFail, Message: msg}
}
func skip(name, msg string) HealthCheck {
	return HealthCheck{Name: name, Status: StatusSkipped, Message: msg}
}

// finalize stamps Healthy = no check failed.
func finalize(checks []HealthCheck) HealthStatus {
	healthy := true
	for _, c := range checks {
		if c.Status == StatusFail {
			healthy = false
			break
		}
	}
	return HealthStatus{Healthy: healthy, Checks: checks}
}

// safeCheck runs fn, converting a panic into a fail so one broken check never
// 500s the endpoint. It always stamps the canonical name.
func safeCheck(name string, fn func() HealthCheck) (res HealthCheck) {
	defer func() {
		if rec := recover(); rec != nil {
			res = HealthCheck{Status: StatusFail, Message: fmt.Sprintf("check panicked: %v", rec)}
		}
		res.Name = name
	}()
	return fn()
}

// --- secret redaction ---

// The health service holds no config secrets, so it can't scrub exact values
// like doctor.sanitizeError does. These pattern masks catch credential-shaped
// substrings (a DSN, a URL with userinfo, a bearer token) that a wrapped DB or
// driver error might echo into an operator-facing endpoint.
var (
	reDSNPassword = regexp.MustCompile(`(?i)password=([^\s]+)`)
	reURLUserinfo = regexp.MustCompile(`://([^:/@\s]+):([^@/\s]+)@`)
	reBearer      = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
)

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = reDSNPassword.ReplaceAllString(msg, "password=***")
	msg = reURLUserinfo.ReplaceAllString(msg, "://$1:***@")
	msg = reBearer.ReplaceAllString(msg, "Bearer ***")
	return msg
}
