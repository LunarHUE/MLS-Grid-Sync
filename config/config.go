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

type Config struct {
	LogLevel  string          `mapstructure:"log_level" yaml:"log_level"`
	Database  DatabaseConfig  `mapstructure:"database" yaml:"database"`
	MLS       MLSConfig       `mapstructure:"mls" yaml:"mls"`
	Profiling ProfilingConfig `mapstructure:"profiling" yaml:"profiling"`
	Storage   StorageConfig   `mapstructure:"storage" yaml:"storage"`
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
