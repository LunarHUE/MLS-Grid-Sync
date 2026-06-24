package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
)

// fakeFetcher is a PageFetcher returning a canned response or error.
type fakeFetcher struct {
	resp  *mls.ODataResponse
	err   error
	block bool // if true, block until ctx is done (timeout test)
}

func (f *fakeFetcher) FetchPage(ctx context.Context, _ string) (*mls.ODataResponse, error) {
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// odataWith builds an ODataResponse whose records carry the given originating
// system names.
func odataWith(names ...string) *mls.ODataResponse {
	resp := &mls.ODataResponse{}
	for _, n := range names {
		resp.Value = append(resp.Value, json.RawMessage(`{"OriginatingSystemName":"`+n+`"}`))
	}
	return resp
}

// byName indexes results for assertions.
func byName(results []CheckResult) map[string]CheckResult {
	m := map[string]CheckResult{}
	for _, c := range results {
		m[c.Name] = c
	}
	return m
}

// baseConfig is a fully-populated, healthy-looking config the tests mutate.
func baseConfig() *config.Config {
	return &config.Config{
		MLS: config.MLSConfig{
			Token:             "secret-token",
			OriginatingSystem: "actris",
			V2URL:             "https://api.example.test/v2",
			APIRPS:            1,
			MediaDownloadRPS:  1,
		},
		Database: config.DatabaseConfig{DSN: "host=localhost dbname=mls password=pw sslmode=disable"},
		Storage:  config.StorageConfig{Backend: "fake"},
		Server:   config.ServerConfig{APIKey: "k", CORSAllowedOrigins: "https://app.example.test"},
	}
}

func okStorer(context.Context, config.StorageConfig) (storage.Storer, error) {
	return &storage.FakeStorer{}, nil
}

func TestRun_ConfigLoadFailure_SkipsEverythingElse(t *testing.T) {
	results := Run(context.Background(), Deps{
		ConfigErr: errors.New("bad yaml at line 3"),
	}, Options{})

	m := byName(results)
	require.Equal(t, StatusFail, m["config"].Status)
	// Every other check is skipped, none run.
	for _, name := range remainingCheckNames {
		assert.Equal(t, StatusSkipped, m[name].Status, "%s should be skipped when config fails", name)
	}
}

func TestRun_MLSMissingTokenAndSystem_Fail(t *testing.T) {
	cfg := baseConfig()
	cfg.MLS.Token = ""
	cfg.MLS.OriginatingSystem = ""

	results := Run(context.Background(), Deps{Config: cfg, BuildStorer: okStorer}, Options{})
	m := byName(results)
	assert.Equal(t, StatusFail, m["mls_token"].Status)
	assert.Equal(t, StatusFail, m["mls_originating_system"].Status)
}

func TestRun_MLSProbeError_Fail(t *testing.T) {
	cfg := baseConfig()
	results := Run(context.Background(), Deps{
		Config:      cfg,
		Fetcher:     &fakeFetcher{err: errors.New("401 unauthorized")},
		BuildStorer: okStorer,
	}, Options{})
	assert.Equal(t, StatusFail, byName(results)["mls_api"].Status)
}

func TestRun_MLSOriginatingSystemNotVisible_Fail(t *testing.T) {
	cfg := baseConfig()
	cfg.MLS.OriginatingSystem = "actris"
	results := Run(context.Background(), Deps{
		Config:      cfg,
		Fetcher:     &fakeFetcher{resp: odataWith("flinthills", "harmls")}, // actris absent
		BuildStorer: okStorer,
	}, Options{})
	api := byName(results)["mls_api"]
	assert.Equal(t, StatusFail, api.Status, "configured system not in visible set must fail, never warn")
}

func TestRun_MLSResolves_Pass(t *testing.T) {
	cfg := baseConfig()
	results := Run(context.Background(), Deps{
		Config:      cfg,
		Fetcher:     &fakeFetcher{resp: odataWith("flinthills", "actris")},
		BuildStorer: okStorer,
	}, Options{})
	assert.Equal(t, StatusPass, byName(results)["mls_api"].Status)
}

func TestRun_SkipMLS_SkipsEntireGroup(t *testing.T) {
	cfg := baseConfig()
	cfg.MLS.Token = "" // would otherwise fail
	results := Run(context.Background(), Deps{Config: cfg, BuildStorer: okStorer}, Options{SkipMLS: true})
	m := byName(results)
	for _, name := range mlsCheckNames {
		assert.Equal(t, StatusSkipped, m[name].Status, "%s should be skipped via --skip-mls", name)
	}
}

func TestRun_SkipStorage_DoesNotInvokeBuilder(t *testing.T) {
	called := false
	cfg := baseConfig()
	results := Run(context.Background(), Deps{
		Config: cfg,
		BuildStorer: func(context.Context, config.StorageConfig) (storage.Storer, error) {
			called = true
			return &storage.FakeStorer{}, nil
		},
	}, Options{SkipStorage: true, SkipMLS: true})

	assert.False(t, called, "--skip-storage must guarantee zero remote calls: builder must not be invoked")
	assert.Equal(t, StatusSkipped, byName(results)["storage"].Status)
}

func TestRun_StorageBuilderError_Fail(t *testing.T) {
	cfg := baseConfig()
	cfg.Storage.Backend = "s3"
	results := Run(context.Background(), Deps{
		Config: cfg,
		BuildStorer: func(context.Context, config.StorageConfig) (storage.Storer, error) {
			return nil, errors.New("dial tcp: connection refused")
		},
	}, Options{SkipMLS: true})
	assert.Equal(t, StatusFail, byName(results)["storage"].Status)
}

func TestRun_NoDB_DBChecksFailCleanly(t *testing.T) {
	cfg := baseConfig()
	results := Run(context.Background(), Deps{Config: cfg, BuildStorer: okStorer}, Options{SkipMLS: true})
	m := byName(results)
	assert.Equal(t, StatusFail, m["postgres"].Status)
	assert.Equal(t, StatusFail, m["postgis"].Status)
	assert.Equal(t, StatusFail, m["tables"].Status)
	// clock_skew degrades to skipped (not fail) when there's no DB.
	assert.Equal(t, StatusSkipped, m["clock_skew"].Status)
}

func TestRun_ServerWarnings(t *testing.T) {
	cfg := baseConfig()
	cfg.Server.APIKey = ""
	cfg.Server.CORSAllowedOrigins = "*"
	results := Run(context.Background(), Deps{Config: cfg, BuildStorer: okStorer}, Options{SkipMLS: true})
	m := byName(results)
	assert.Equal(t, StatusWarn, m["server_api_key"].Status)
	assert.Equal(t, StatusWarn, m["server_cors"].Status)
}

func TestRun_RateLimits(t *testing.T) {
	cfg := baseConfig()
	cfg.MLS.APIRPS = 0 // invalid
	results := Run(context.Background(), Deps{Config: cfg, BuildStorer: okStorer}, Options{SkipMLS: true})
	assert.Equal(t, StatusFail, byName(results)["rate_limits"].Status)

	cfg = baseConfig()
	cfg.MLS.APIRPS = 1000 // implausibly high
	results = Run(context.Background(), Deps{Config: cfg, BuildStorer: okStorer}, Options{SkipMLS: true})
	assert.Equal(t, StatusWarn, byName(results)["rate_limits"].Status)
}

func TestRun_DeferredChecksSkipped(t *testing.T) {
	results := Run(context.Background(), Deps{Config: baseConfig(), BuildStorer: okStorer}, Options{SkipMLS: true})
	m := byName(results)
	assert.Equal(t, StatusSkipped, m["schema_version"].Status)
	assert.Equal(t, StatusSkipped, m["deployment_id"].Status)
}

func TestRun_Timeout_FetcherBlocks_FailsPromptly(t *testing.T) {
	cfg := baseConfig()
	start := time.Now()
	results := Run(context.Background(), Deps{
		Config:      cfg,
		Fetcher:     &fakeFetcher{block: true},
		BuildStorer: okStorer,
	}, Options{Timeout: 50 * time.Millisecond, SkipStorage: true})
	elapsed := time.Since(start)

	assert.Equal(t, StatusFail, byName(results)["mls_api"].Status)
	assert.Less(t, elapsed, 2*time.Second, "a blocked probe must fail at the timeout, not hang")
}

func TestRun_PanicIsolatedToOneCheck(t *testing.T) {
	got := safeCheck("boom", func() CheckResult {
		panic("kaboom")
	})
	assert.Equal(t, "boom", got.Name)
	assert.Equal(t, StatusFail, got.Status)
	assert.Contains(t, got.Message, "panicked")
}

func TestExitCode(t *testing.T) {
	warnOnly := []CheckResult{{Status: StatusPass}, {Status: StatusWarn}, {Status: StatusSkipped}}
	assert.Equal(t, 0, ExitCode(warnOnly, false))
	assert.Equal(t, 1, ExitCode(warnOnly, true), "strict promotes warn to failure")

	withFail := []CheckResult{{Status: StatusPass}, {Status: StatusFail}}
	assert.Equal(t, 1, ExitCode(withFail, false))
}

func TestNewReport_SummaryAndOK(t *testing.T) {
	results := []CheckResult{
		{Name: "a", Status: StatusPass},
		{Name: "b", Status: StatusWarn},
		{Name: "c", Status: StatusFail},
		{Name: "d", Status: StatusSkipped},
	}
	rep := NewReport(results, false)
	assert.False(t, rep.OK)
	assert.Equal(t, Summary{Pass: 1, Warn: 1, Fail: 1, Skipped: 1}, rep.Summary)

	// JSON round-trips into a stable shape with fixed check order.
	b, err := json.Marshal(rep)
	require.NoError(t, err)
	var decoded Report
	require.NoError(t, json.Unmarshal(b, &decoded))
	assert.Equal(t, results, decoded.Checks)
	assert.False(t, decoded.OK)
}

// TestRun_SecretRedaction is the hard requirement: no secret value may ever
// appear in any result message, across pass AND fail paths, including when an
// underlying error echoes the DSN/credentials.
func TestRun_SecretRedaction(t *testing.T) {
	const (
		token     = "TOKEN-SUPERSECRET-123"
		dbPass    = "DBPASS-SUPERSECRET-456"
		s3Secret  = "S3SECRET-SUPERSECRET-789"
		s3KeyID   = "AKIASUPERSECRETKEYID"
		azureConn = "AZURECONN-SUPERSECRET-000"
	)
	cfg := baseConfig()
	cfg.MLS.Token = token
	cfg.Database.DSN = "host=db user=postgres password=" + dbPass + " dbname=mls"
	cfg.Database.Password = dbPass
	cfg.Storage.Backend = "s3"
	cfg.Storage.S3 = config.S3StorageConfig{Bucket: "media", AccessKeyID: s3KeyID, SecretAccessKey: s3Secret}
	cfg.Storage.Azure = config.AzureStorageConfig{ConnectionString: azureConn}

	// Errors that deliberately echo the secrets, as a real SDK/driver might.
	results := Run(context.Background(), Deps{
		Config:  cfg,
		Fetcher: &fakeFetcher{err: errors.New("auth failed with Bearer " + token)},
		BuildStorer: func(context.Context, config.StorageConfig) (storage.Storer, error) {
			return nil, errors.New("s3: bad credentials key=" + s3KeyID + " secret=" + s3Secret + " conn=" + azureConn)
		},
	}, Options{})

	secrets := []string{token, dbPass, s3Secret, s3KeyID, azureConn}
	for _, c := range results {
		for _, secret := range secrets {
			assert.NotContainsf(t, c.Message, secret, "check %q message leaked a secret", c.Name)
			assert.NotContainsf(t, c.Remediation, secret, "check %q remediation leaked a secret", c.Name)
		}
	}
}

func TestSanitizeError_NilConfigSafe(t *testing.T) {
	// config.Load failures happen before cfg exists; must not panic and must
	// still mask credential-shaped patterns.
	out := sanitizeError(errors.New(`parse "postgres://u:p@host/db?password=hunter2": bad`), nil)
	assert.NotContains(t, out, "hunter2")
	assert.Contains(t, out, "password=***")

	assert.Equal(t, "", sanitizeError(nil, nil))
}

func TestSanitizeError_MasksURLUserinfo(t *testing.T) {
	out := sanitizeError(errors.New("dial postgres://admin:topsecret@db:5432 failed"), nil)
	assert.NotContains(t, out, "topsecret")
	assert.Contains(t, out, "admin:***@")
	_ = strings.TrimSpace(out)
}
