package config

import (
	"embed"
	"fmt"
	"os"

	"github.com/lunarhue/libs-go/config"
	"github.com/lunarhue/libs-go/log"
)

//go:embed default.config.yaml
var defaultConfigFile embed.FS

type DatabaseConfig struct {
	DSN      string `mapstructure:"dsn" yaml:"dsn"`
	Password string `mapstructure:"password" yaml:"password"`
}

type MLSConfig struct {
	Token             string `mapstructure:"token" yaml:"token"`
	OriginatingSystem string `mapstructure:"originating_system" yaml:"originating_system"`
	V2URL             string `mapstructure:"v2_url" yaml:"v2_url"`
	// MediaDownloadRPS bounds the attachment worker's binary-download rate.
	// Distinct from the OData API limiter in mls/client.go because MLS Grid
	// imposes separate caps. Conservative default of 1 RPS — tune against
	// the data license agreement.
	MediaDownloadRPS float64 `mapstructure:"media_download_rps" yaml:"media_download_rps"`
	// APIRPS bounds the OData API request rate (the page-fetch limiter in
	// mls/client.go). Separate cap from MediaDownloadRPS. Conservative
	// default of 1 RPS; a non-positive value falls back to 1. Tune against
	// the data license agreement.
	APIRPS float64 `mapstructure:"api_rps" yaml:"api_rps"`
	// FetchConcurrency sets how many OData pages `init` fetches in parallel
	// via $skip offset paging (verified supported against the live feed).
	// MLS Grid's per-page response latency (server-side $expand assembly) is
	// the init bottleneck while the client sits idle; fetching pages
	// concurrently overlaps those waits. Effective parallelism is still
	// capped by APIRPS (the limiter throttles request starts), so raise both
	// together. <=1 disables it and restores sequential nextLink paging.
	// Init-only — delta stays sequential because a key can recur across delta
	// pages and must project in raw order. Default 4.
	FetchConcurrency int `mapstructure:"fetch_concurrency" yaml:"fetch_concurrency"`
}

// ProfilingConfig gates the in-process pprof endpoint used by the
// throughput investigation runbook (docs/profiling.md). The server is
// localhost-bound and intended for dev sessions — pprof exposes heap,
// goroutine, and CPU/block profile endpoints that must never face the
// public network.
type ProfilingConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
	Port    int  `mapstructure:"port" yaml:"port"`
}

// StorageConfig selects the attachment backend and carries its
// per-backend settings. backend ∈ {fake, local, azure, s3}. Phase 1
// wires fake and local; azure/s3 fail loudly at worker start with a
// "not yet available (Phase 3)" error rather than silently falling
// back. KeyPrefix is processing-time, not enqueue-time: a change
// applies to subsequent uploads, never re-tags already-uploaded rows.
type StorageConfig struct {
	Backend   string             `mapstructure:"backend" yaml:"backend"`
	KeyPrefix string             `mapstructure:"key_prefix" yaml:"key_prefix"`
	Local     LocalStorageConfig `mapstructure:"local" yaml:"local"`
	Azure     AzureStorageConfig `mapstructure:"azure" yaml:"azure"`
	S3        S3StorageConfig    `mapstructure:"s3" yaml:"s3"`
}

// LocalStorageConfig configures the LocalStorer (filesystem-backed,
// test-only). CapBytes is a hard ceiling enforced at upload time — a
// runaway-test safety bound, not a production knob.
type LocalStorageConfig struct {
	RootDir  string `mapstructure:"root_dir" yaml:"root_dir"`
	CapBytes int64  `mapstructure:"cap_bytes" yaml:"cap_bytes"`
}

// AzureStorageConfig: emulator/test fields are filled in YAML for
// Phase 3 Azurite; prod auth path is DefaultAzureCredential against
// AccountURL.
type AzureStorageConfig struct {
	ConnectionString string `mapstructure:"connection_string" yaml:"connection_string"`
	AccountURL       string `mapstructure:"account_url" yaml:"account_url"`
	Container        string `mapstructure:"container" yaml:"container"`
}

// S3StorageConfig: Endpoint set means MinIO/test; empty means real AWS.
// Static credentials are TEST shapes only — prod uses the SDK's
// default credential chain (IAM role / env / shared config).
type S3StorageConfig struct {
	Endpoint        string `mapstructure:"endpoint" yaml:"endpoint"`
	Bucket          string `mapstructure:"bucket" yaml:"bucket"`
	Region          string `mapstructure:"region" yaml:"region"`
	AccessKeyID     string `mapstructure:"access_key_id" yaml:"access_key_id"`
	SecretAccessKey string `mapstructure:"secret_access_key" yaml:"secret_access_key"`
	UsePathStyle    bool   `mapstructure:"use_path_style" yaml:"use_path_style"`
}

// ProcessorConfig tunes the raw → typed processor pass. CommitBatchSize is
// the number of raw_output records committed per transaction (distinct from
// the fetch batch). Larger batches amortize the per-record COMMIT/fsync and
// cursor write that otherwise dominate wall-clock on the I/O-bound pass (see
// docs/profiling.md). On a batch error the loop falls back to one record per
// transaction so the exact poison record is still pinpointed. <=1 restores the
// historical one-record-per-tx behavior.
type ProcessorConfig struct {
	CommitBatchSize int `mapstructure:"commit_batch_size" yaml:"commit_batch_size"`
	// InitPipeline overlaps page-fetching with the typed processor during
	// `init` (producer/consumer) so the two I/O-bound phases run concurrently.
	// Defaults to true (set in default.config.yaml); `init --no-pipeline`
	// forces the sequential fetch-then-process path.
	InitPipeline bool `mapstructure:"init_pipeline" yaml:"init_pipeline"`
	// Bulk projects a commit-chunk with batched SQL (one bulk read + a handful
	// of bulk writes) instead of per-record round-trips, for processors that
	// support it (Property + Media/OpenHouse/Rooms/UnitTypes — all versioned
	// resources except Lookup). Defaults to true; disabling forces the proven
	// per-record path (the operator kill-switch). See docs/profiling.md R4.
	Bulk bool `mapstructure:"bulk" yaml:"bulk"`
}

// ServerConfig configures the GraphQL HTTP server started by the
// `serve` subcommand. Addr is a net/http listen address (":8080",
// "127.0.0.1:9000"); override via MLS_SYNC_SERVER_ADDR or --addr.
type ServerConfig struct {
	Addr               string `mapstructure:"addr" yaml:"addr"`
	APIKey             string `mapstructure:"api_key" yaml:"api_key"`
	CORSAllowedOrigins string `mapstructure:"cors_allowed_origins" yaml:"cors_allowed_origins"`
}

type Config struct {
	LogLevel  string          `mapstructure:"log_level" yaml:"log_level"`
	Database  DatabaseConfig  `mapstructure:"database" yaml:"database"`
	MLS       MLSConfig       `mapstructure:"mls" yaml:"mls"`
	Profiling ProfilingConfig `mapstructure:"profiling" yaml:"profiling"`
	Storage   StorageConfig   `mapstructure:"storage" yaml:"storage"`
	Server    ServerConfig    `mapstructure:"server" yaml:"server"`
	Processor ProcessorConfig `mapstructure:"processor" yaml:"processor"`
}

func Load() (*Config, error) {
	overrideFile := "config.yaml"
	if _, err := os.Stat(overrideFile); os.IsNotExist(err) {
		overrideFile = ""
	}

	cfg, err := config.LoadConfig[Config](&defaultConfigFile, "default.config.yaml", overrideFile, "MLS_SYNC")
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	log.SetLevelFromString(cfg.LogLevel)

	return cfg, nil
}
