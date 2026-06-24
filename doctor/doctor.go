// Package doctor runs independent, read-only(-ish) health checks against a
// deployment's configuration and subsystems so an operator can confirm a new
// per-customer appliance is wired correctly *before* the first sync.
//
// Every check is isolated: a failure (or panic) in one never prevents the
// others from running, and no check output ever contains a secret value (see
// sanitizeError). The cmd layer constructs real dependencies and injects them
// via Deps; tests inject fakes.
package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/migrate"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/server"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
)

// Status is a single check's outcome.
type Status string

const (
	StatusPass    Status = "pass"
	StatusWarn    Status = "warn"
	StatusFail    Status = "fail"
	StatusSkipped Status = "skipped"
)

// DefaultTimeout bounds each individual network/DB/storage check.
const DefaultTimeout = 10 * time.Second

const (
	// clockSkewThreshold is the host↔DB clock difference above which the
	// clock_skew check warns.
	clockSkewThreshold = 60 * time.Second
	// rateLimitWarnCeiling flags suspiciously high RPS values that likely
	// exceed an MLS Grid license cap.
	rateLimitWarnCeiling = 50.0
)

// CheckResult is one check's report line.
type CheckResult struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// Options carries the operator-supplied flags.
type Options struct {
	SkipMLS     bool
	SkipStorage bool
	Strict      bool          // promote warn -> non-zero exit
	Timeout     time.Duration // per-check deadline; <=0 falls back to DefaultTimeout
}

// Deps are the injected dependencies. Constructed for real in cmd/doctor.go;
// faked in tests.
type Deps struct {
	Config      *config.Config // nil => config-load failed (see ConfigErr)
	ConfigErr   error          // non-nil => config.Load() failed
	Fetcher     mls.PageFetcher
	DB          *sql.DB
	BuildStorer func(ctx context.Context, cfg config.StorageConfig) (storage.Storer, error)
	Now         func() time.Time
}

// mlsCheckNames / remainingCheckNames define the fixed check ordering and let
// the config-load failure path mark everything else skipped.
var (
	mlsCheckNames = []string{"mls_token", "mls_url", "mls_originating_system", "mls_api"}
	// remainingCheckNames is every check other than "config", in display
	// order — used to emit "skipped" rows when config itself won't load.
	remainingCheckNames = append(append([]string{}, mlsCheckNames...),
		"postgres", "postgis", "tables", "schema_version", "clock_skew",
		"storage", "server_api_key", "server_cors", "pprof", "rate_limits",
		"deployment_id",
	)
)

// Run executes every check in a fixed order and returns the results.
func Run(ctx context.Context, deps Deps, opts Options) []CheckResult {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	r := &runner{ctx: ctx, deps: deps, opts: opts}
	r.run()
	return r.out
}

type runner struct {
	ctx  context.Context
	deps Deps
	opts Options
	out  []CheckResult
}

func (r *runner) add(c CheckResult) { r.out = append(r.out, c) }
func (r *runner) skip(name, msg string) {
	r.add(CheckResult{Name: name, Status: StatusSkipped, Message: msg})
}
func (r *runner) check(name string, fn func() CheckResult) { r.add(safeCheck(name, fn)) }

// timeoutCtx returns a FRESH per-check deadline so a slow earlier check never
// eats into a later one's budget.
func (r *runner) timeoutCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.ctx, r.opts.Timeout)
}

func (r *runner) run() {
	// Check #0: config must load. If it didn't, nothing else can run.
	if r.deps.ConfigErr != nil {
		r.add(CheckResult{
			Name:        "config",
			Status:      StatusFail,
			Message:     "configuration failed to load: " + sanitizeError(r.deps.ConfigErr, r.deps.Config),
			Remediation: "fix config.yaml or MLS_SYNC_* env vars (see config/default.config.yaml)",
		})
		for _, name := range remainingCheckNames {
			r.skip(name, "skipped: configuration did not load")
		}
		return
	}
	r.add(CheckResult{Name: "config", Status: StatusPass, Message: "configuration loaded"})

	// MLS group — --skip-mls skips the entire group (config-presence too), so
	// an operator can validate DB/storage/server without a token on hand.
	if r.opts.SkipMLS {
		for _, name := range mlsCheckNames {
			r.skip(name, "skipped via --skip-mls")
		}
	} else {
		r.check("mls_token", r.checkMLSToken)
		r.check("mls_url", r.checkMLSURL)
		r.check("mls_originating_system", r.checkMLSOriginatingSystem)
		r.check("mls_api", r.checkMLSAPI)
	}

	// Database group (strictly read-only).
	r.check("postgres", r.checkPostgres)
	r.check("postgis", r.checkPostGIS)
	r.check("tables", r.checkTables)
	r.skip("schema_version", "no versioned-migration ledger exists yet (deferred); nothing to compare")
	r.check("clock_skew", r.checkClockSkew)

	// Storage — --skip-storage guarantees ZERO remote side effects by never
	// invoking the builder (which may idempotently create a bucket/container).
	if r.opts.SkipStorage {
		r.skip("storage", "skipped via --skip-storage")
	} else {
		r.check("storage", r.checkStorage)
	}

	// Server / runtime config.
	r.check("server_api_key", r.checkServerAPIKey)
	r.check("server_cors", r.checkServerCORS)
	r.check("pprof", r.checkPprof)
	r.check("rate_limits", r.checkRateLimits)
	r.skip("deployment_id", "no deployment name/id field in config; nearest signals are mls.originating_system and storage.key_prefix")
}

// --- MLS checks ---

func (r *runner) checkMLSToken() CheckResult {
	if r.deps.Config.MLS.Token == "" {
		return CheckResult{Status: StatusFail,
			Message:     "MLS token is not configured — the deployment cannot sync",
			Remediation: "set mls.token or MLS_SYNC_MLS_TOKEN"}
	}
	return CheckResult{Status: StatusPass, Message: "MLS token is configured (value redacted)"}
}

func (r *runner) checkMLSURL() CheckResult {
	if r.deps.Config.MLS.V2URL == "" {
		return CheckResult{Status: StatusFail,
			Message:     "MLS API URL (mls.v2_url) is not configured",
			Remediation: "set mls.v2_url or MLS_SYNC_MLS_V2_URL"}
	}
	return CheckResult{Status: StatusPass, Message: "MLS API URL: " + r.deps.Config.MLS.V2URL}
}

func (r *runner) checkMLSOriginatingSystem() CheckResult {
	if r.deps.Config.MLS.OriginatingSystem == "" {
		return CheckResult{Status: StatusFail,
			Message:     "originating system is not configured — the deployment is not ready",
			Remediation: "set mls.originating_system or MLS_SYNC_MLS_ORIGINATING_SYSTEM (`mls-cli systems` to discover)"}
	}
	return CheckResult{Status: StatusPass, Message: "originating system configured: " + r.deps.Config.MLS.OriginatingSystem}
}

func (r *runner) checkMLSAPI() CheckResult {
	want := r.deps.Config.MLS.OriginatingSystem
	if want == "" {
		return CheckResult{Status: StatusFail,
			Message:     "cannot verify MLS API: originating system is not configured",
			Remediation: "set mls.originating_system first"}
	}
	if r.deps.Fetcher == nil {
		return CheckResult{Status: StatusFail, Message: "no MLS client available (token missing?)"}
	}
	ctx, cancel := r.timeoutCtx()
	defer cancel()
	names, err := mls.ProbeOriginatingSystems(ctx, r.deps.Fetcher, r.deps.Config.MLS.V2URL)
	if err != nil {
		return CheckResult{Status: StatusFail,
			Message:     "MLS Grid API probe failed: " + sanitizeError(err, r.deps.Config),
			Remediation: "verify mls.v2_url reachability and that the token is accepted"}
	}
	for _, n := range names {
		if strings.EqualFold(n, want) {
			return CheckResult{Status: StatusPass,
				Message: fmt.Sprintf("MLS Grid API reachable; originating system %q resolved", want)}
		}
	}
	return CheckResult{Status: StatusFail,
		Message:     fmt.Sprintf("MLS Grid API reachable but originating system %q is not among the systems this token can see (%v)", want, names),
		Remediation: "confirm mls.originating_system matches a visible system (`mls-cli systems`)"}
}

// --- Database checks (read-only) ---

func (r *runner) checkPostgres() CheckResult {
	if r.deps.DB == nil {
		return CheckResult{Status: StatusFail,
			Message:     "no database handle (database.dsn missing?)",
			Remediation: "set database.dsn or MLS_SYNC_DATABASE_DSN"}
	}
	ctx, cancel := r.timeoutCtx()
	defer cancel()
	if err := r.deps.DB.PingContext(ctx); err != nil {
		return CheckResult{Status: StatusFail,
			Message:     "cannot connect to PostgreSQL: " + sanitizeError(err, r.deps.Config),
			Remediation: "check database.dsn host/port/credentials and that the server is reachable"}
	}
	return CheckResult{Status: StatusPass, Message: "connected to PostgreSQL"}
}

func (r *runner) checkPostGIS() CheckResult {
	if r.deps.DB == nil {
		return CheckResult{Status: StatusFail, Message: "no database handle"}
	}
	ctx, cancel := r.timeoutCtx()
	defer cancel()
	var ext string
	err := r.deps.DB.QueryRowContext(ctx,
		"SELECT extname FROM pg_extension WHERE extname = 'postgis'").Scan(&ext)
	if err == sql.ErrNoRows {
		return CheckResult{Status: StatusFail,
			Message:     "PostGIS extension is not installed",
			Remediation: "run `mls-cli init` (or normal sync startup) — geo migrations install PostGIS"}
	}
	if err != nil {
		return CheckResult{Status: StatusFail,
			Message: "could not query for the PostGIS extension: " + sanitizeError(err, r.deps.Config)}
	}
	return CheckResult{Status: StatusPass, Message: "PostGIS extension available"}
}

func (r *runner) checkTables() CheckResult {
	if r.deps.DB == nil {
		return CheckResult{Status: StatusFail, Message: "no database handle"}
	}
	ctx, cancel := r.timeoutCtx()
	defer cancel()
	var missing []string
	for _, tbl := range migrate.Tables {
		var exists bool
		err := r.deps.DB.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)",
			tbl.Name).Scan(&exists)
		if err != nil {
			return CheckResult{Status: StatusFail,
				Message: "could not query expected tables: " + sanitizeError(err, r.deps.Config)}
		}
		if !exists {
			missing = append(missing, tbl.Name)
		}
	}
	if len(missing) > 0 {
		return CheckResult{Status: StatusFail,
			Message:     fmt.Sprintf("missing %d of %d expected tables: %s", len(missing), len(migrate.Tables), strings.Join(missing, ", ")),
			Remediation: "expected on a brand-new database — run `mls-cli init` (or normal sync startup) to create/migrate the schema"}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("all %d expected tables present", len(migrate.Tables))}
}

func (r *runner) checkClockSkew() CheckResult {
	if r.deps.DB == nil {
		return CheckResult{Status: StatusSkipped, Message: "database unreachable; cannot compare clocks"}
	}
	ctx, cancel := r.timeoutCtx()
	defer cancel()
	var dbNow time.Time
	if err := r.deps.DB.QueryRowContext(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		return CheckResult{Status: StatusSkipped, Message: "could not read database time: " + sanitizeError(err, r.deps.Config)}
	}
	skew := r.deps.Now().Sub(dbNow)
	if skew < 0 {
		skew = -skew
	}
	if skew > clockSkewThreshold {
		return CheckResult{Status: StatusWarn,
			Message:     fmt.Sprintf("host↔database clock skew is %s (> %s)", skew.Round(time.Second), clockSkewThreshold),
			Remediation: "sync clocks via NTP on the host and database server"}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("clock skew within tolerance (%s)", skew.Round(time.Millisecond))}
}

// --- Storage check ---

func (r *runner) checkStorage() CheckResult {
	cfg := r.deps.Config
	backend := cfg.Storage.Backend
	if backend == "" {
		backend = "fake"
	}
	if r.deps.BuildStorer == nil {
		return CheckResult{Status: StatusFail, Message: "no storage builder available"}
	}
	ctx, cancel := r.timeoutCtx()
	defer cancel()
	if _, err := r.deps.BuildStorer(ctx, cfg.Storage); err != nil {
		return CheckResult{Status: StatusFail,
			Message:     fmt.Sprintf("storage backend %q failed: %s", backend, sanitizeError(err, cfg)),
			Remediation: "check storage." + backend + " settings (endpoint/bucket/container/credentials)"}
	}
	switch backend {
	case "local":
		return CheckResult{Status: StatusPass, Message: fmt.Sprintf("local backend OK at %s (no remote round-trip)", cfg.Storage.Local.RootDir)}
	case "s3":
		return CheckResult{Status: StatusPass, Message: fmt.Sprintf("s3 backend reachable; bucket %q exists or was created by startup-compatible probe", cfg.Storage.S3.Bucket)}
	case "azure":
		return CheckResult{Status: StatusPass, Message: fmt.Sprintf("azure backend reachable; container %q exists or was created by startup-compatible probe", cfg.Storage.Azure.Container)}
	default:
		return CheckResult{Status: StatusPass, Message: "fake backend OK (no remote round-trip)"}
	}
}

// --- Server / runtime checks ---

func (r *runner) checkServerAPIKey() CheckResult {
	if r.deps.Config.Server.APIKey == "" {
		return CheckResult{Status: StatusWarn,
			Message:     "GraphQL API key is not set — the API is unauthenticated (doctor cannot tell prod from dev)",
			Remediation: "set server.api_key for any internet-facing deployment"}
	}
	return CheckResult{Status: StatusPass, Message: "GraphQL API key is configured (value redacted)"}
}

func (r *runner) checkServerCORS() CheckResult {
	origins := server.SplitOrigins(r.deps.Config.Server.CORSAllowedOrigins)
	open := len(origins) == 0
	for _, o := range origins {
		if o == "*" {
			open = true
		}
	}
	if open {
		return CheckResult{Status: StatusWarn,
			Message:     "CORS allowlist is open (allows any origin); doctor cannot tell prod from dev",
			Remediation: "set server.cors_allowed_origins to an explicit comma-separated list in production"}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("CORS restricted to %v", origins)}
}

func (r *runner) checkPprof() CheckResult {
	p := r.deps.Config.Profiling
	if !p.Enabled {
		return CheckResult{Status: StatusPass, Message: "pprof disabled"}
	}
	port := p.Port
	if port == 0 {
		port = 6060
	}
	return CheckResult{Status: StatusPass,
		Message: fmt.Sprintf("pprof enabled, bound to 127.0.0.1:%d (localhost-only by construction)", port)}
}

func (r *runner) checkRateLimits() CheckResult {
	m := r.deps.Config.MLS
	if m.APIRPS <= 0 {
		return CheckResult{Status: StatusFail,
			Message:     fmt.Sprintf("mls.api_rps must be > 0 (got %g)", m.APIRPS),
			Remediation: "set mls.api_rps to a positive value within your data-license limits"}
	}
	if m.MediaDownloadRPS <= 0 {
		return CheckResult{Status: StatusFail,
			Message:     fmt.Sprintf("mls.media_download_rps must be > 0 (got %g)", m.MediaDownloadRPS),
			Remediation: "set mls.media_download_rps to a positive value"}
	}
	if m.APIRPS > rateLimitWarnCeiling || m.MediaDownloadRPS > rateLimitWarnCeiling {
		return CheckResult{Status: StatusWarn,
			Message:     fmt.Sprintf("rate limits look high (api_rps=%g, media_download_rps=%g); verify against your MLS Grid license", m.APIRPS, m.MediaDownloadRPS),
			Remediation: "MLS Grid enforces per-license caps; exceeding them risks throttling or a ban"}
	}
	return CheckResult{Status: StatusPass,
		Message: fmt.Sprintf("rate limits set (api_rps=%g, media_download_rps=%g)", m.APIRPS, m.MediaDownloadRPS)}
}

// --- isolation, exit code, report shaping ---

// safeCheck runs fn, converting a panic into a fail result so one broken check
// never aborts the whole run. It always stamps the canonical name.
func safeCheck(name string, fn func() CheckResult) (res CheckResult) {
	defer func() {
		if rec := recover(); rec != nil {
			res = CheckResult{Status: StatusFail, Message: fmt.Sprintf("check panicked: %v", rec)}
		}
		res.Name = name
	}()
	res = fn()
	return res
}

// ExitCode is 1 if any check failed; with strict, any warn also fails.
func ExitCode(results []CheckResult, strict bool) int {
	for _, c := range results {
		if c.Status == StatusFail {
			return 1
		}
	}
	if strict {
		for _, c := range results {
			if c.Status == StatusWarn {
				return 1
			}
		}
	}
	return 0
}

// Summary holds per-status counts for machine consumers.
type Summary struct {
	Pass    int `json:"pass"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
	Skipped int `json:"skipped"`
}

// Report is the stable JSON shape emitted by `doctor --json`.
type Report struct {
	OK      bool          `json:"ok"`
	Strict  bool          `json:"strict"`
	Summary Summary       `json:"summary"`
	Checks  []CheckResult `json:"checks"`
}

// NewReport tallies results into the JSON report shape.
func NewReport(results []CheckResult, strict bool) Report {
	var s Summary
	for _, c := range results {
		switch c.Status {
		case StatusPass:
			s.Pass++
		case StatusWarn:
			s.Warn++
		case StatusFail:
			s.Fail++
		case StatusSkipped:
			s.Skipped++
		}
	}
	return Report{
		OK:      ExitCode(results, strict) == 0,
		Strict:  strict,
		Summary: s,
		Checks:  results,
	}
}

// --- secret redaction ---

var (
	reDSNPassword = regexp.MustCompile(`(?i)password=([^\s]+)`)
	reURLUserinfo = regexp.MustCompile(`://([^:/@\s]+):([^@/\s]+)@`)
	reBearer      = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
)

// sanitizeError renders err with every known secret removed. It is safe to
// call with a nil cfg (config-load failures happen before cfg exists) — in
// that case only the pattern masks apply.
func sanitizeError(err error, cfg *config.Config) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if cfg != nil {
		for _, secret := range secretValues(cfg) {
			if secret != "" {
				msg = strings.ReplaceAll(msg, secret, "***")
			}
		}
	}
	return maskKnownPatterns(msg)
}

// secretValues lists the literal secret strings to scrub from any message.
func secretValues(cfg *config.Config) []string {
	return []string{
		cfg.MLS.Token,
		cfg.Database.Password,
		dsnPassword(cfg.Database.DSN),
		cfg.Storage.S3.AccessKeyID,
		cfg.Storage.S3.SecretAccessKey,
		cfg.Storage.Azure.ConnectionString,
	}
}

// maskKnownPatterns redacts credential-shaped substrings even when the exact
// value isn't known (e.g. a config-parse error before cfg is built, or an SDK
// error echoing a DSN).
func maskKnownPatterns(msg string) string {
	msg = reDSNPassword.ReplaceAllString(msg, "password=***")
	msg = reURLUserinfo.ReplaceAllString(msg, "://$1:***@")
	msg = reBearer.ReplaceAllString(msg, "Bearer ***")
	return msg
}

// dsnPassword extracts the libpq `password=...` value so it can be redacted
// even when it appears embedded in a longer message.
func dsnPassword(dsn string) string {
	if m := reDSNPassword.FindStringSubmatch(dsn); len(m) == 2 {
		return m[1]
	}
	return ""
}
