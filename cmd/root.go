package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	_ "net/http/pprof" // registers handlers on http.DefaultServeMux when imported
	"os"
	"runtime"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/lib/pq" // Postgres driver for Ent
	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/geo"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/search"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	"github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

var appConfig *config.Config

var rootCmd = &cobra.Command{
	Use:   "mls-cli",
	Short: "MLS Grid Sync and Asset Pipeline",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		appConfig = cfg
		startProfilingServer(cfg.Profiling)
		return nil
	},
}

// startProfilingServer starts the in-process pprof HTTP server on
// 127.0.0.1 when profiling.enabled is true. Net effect when disabled is
// zero (no listener, no block sampling). See docs/profiling.md for the
// investigation runbook this enables.
func startProfilingServer(cfg config.ProfilingConfig) {
	if !cfg.Enabled {
		return
	}
	port := cfg.Port
	if port == 0 {
		port = 6060
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Block profile sampling rate of 1 reports every blocking event;
	// fine for an investigation pass, would be too noisy in steady state.
	runtime.SetBlockProfileRate(1)

	go func() {
		log.Infof("pprof: listening on http://%s/debug/pprof/", addr)
		srv := &http.Server{Addr: addr, Handler: nil} // nil = http.DefaultServeMux
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorf("pprof: server exited with error: %v", err)
		}
	}()
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// components bundles everything a subcommand might need so reprocess and
// validate-typed can reach the processor stack and the raw *sql.DB
// without copy-pasting the wiring.
type components struct {
	svc   *sync.Service
	db    *ent.Client
	sqlDB *sql.DB
	proc  *processor.Processor
}

// Bounded retry for the initial DB connection, which is established lazily on
// the first Schema.Create. Backoff is linear: attempt N waits N*backoff, so
// three attempts span ~0s + 2s + 4s before giving up.
const (
	schemaCreateAttempts = 3
	schemaCreateBackoff  = 2 * time.Second
)

func setupComponents(ctx context.Context) (*components, error) {
	if appConfig.MLS.Token == "" {
		return nil, fmt.Errorf("fatal: MLS token is missing from configuration")
	}

	sqlDB, err := sql.Open("postgres", appConfig.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed opening connection to postgres: %w", err)
	}

	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	db := ent.NewClient(ent.Driver(drv))

	// db.Schema.Create is the first call that actually dials Postgres (sql.Open
	// is lazy), so a still-booting or briefly-unreachable server surfaces here
	// as a connection EOF rather than a schema problem. Retry a few times with
	// linear backoff so a DB that is just coming up (compose start, restart)
	// doesn't fail the whole command on the first try.
	var schemaErr error
	for attempt := 1; attempt <= schemaCreateAttempts; attempt++ {
		if schemaErr = db.Schema.Create(ctx); schemaErr == nil {
			break
		}
		if attempt < schemaCreateAttempts {
			backoff := time.Duration(attempt) * schemaCreateBackoff
			log.Warnf("schema create attempt %d/%d failed (%v); retrying in %s",
				attempt, schemaCreateAttempts, schemaErr, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				db.Close()
				return nil, ctx.Err()
			}
		}
	}
	if schemaErr != nil {
		db.Close()
		return nil, fmt.Errorf("failed creating schema resources after %d attempts: %w", schemaCreateAttempts, schemaErr)
	}

	if err := geo.Migrate(ctx, sqlDB); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed applying postgis migrations: %w", err)
	}

	if err := search.Migrate(ctx, sqlDB); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed applying trigram migrations: %w", err)
	}

	mlsClient := mls.NewClient(appConfig.MLS.Token, appConfig.MLS.APIRPS)

	proc := processor.New(db, sqlDB,
		processor.NewLookupProcessor(),
		processor.NewOfficeProcessor(),
		processor.NewMemberProcessor(),
		processor.NewPropertyProcessor(),
		processor.NewOpenHouseProcessor(),
		processor.NewMediaProcessor(),
		processor.NewPropertyRoomProcessor(),
		processor.NewPropertyUnitTypeProcessor(),
	).WithCommitBatchSize(appConfig.Processor.CommitBatchSize).
		WithBulk(appConfig.Processor.Bulk).
		WithDriftSampleRate(appConfig.Processor.DriftSampleRate)

	storer, err := newStorer(ctx, appConfig.Storage)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("storage backend: %w", err)
	}
	svc := sync.NewService(mlsClient, db, sqlDB, storer, proc).
		WithFetchConcurrency(appConfig.MLS.FetchConcurrency)

	return &components{svc: svc, db: db, sqlDB: sqlDB, proc: proc}, nil
}

// newStorer dispatches on Storage.Backend. All four backends are
// wired: fake (no-op), local (filesystem with cap + atomic writes),
// azure (Azure Blob via azblob), s3 (S3 / S3-compatible via
// aws-sdk-go-v2). Per-backend validation lives in the constructor.
//
// ctx is threaded into the azure/s3 constructors (which round-trip to the
// backend at startup) so a caller-supplied deadline can cancel a stuck SDK
// dial — used by `doctor`, whose storage check runs under --timeout.
func newStorer(ctx context.Context, cfg config.StorageConfig) (storage.Storer, error) {
	backend := cfg.Backend
	if backend == "" {
		backend = "fake"
	}
	switch backend {
	case "fake":
		return &storage.FakeStorer{}, nil
	case "local":
		root := cfg.Local.RootDir
		if root == "" {
			return nil, fmt.Errorf("local backend requires storage.local.root_dir")
		}
		cap := cfg.Local.CapBytes
		if cap <= 0 {
			return nil, fmt.Errorf("local backend requires storage.local.cap_bytes > 0")
		}
		return storage.NewLocal(root, cap)
	case "azure":
		return storage.NewAzureBlob(ctx,
			cfg.Azure.ConnectionString, cfg.Azure.AccountURL, cfg.Azure.Container)
	case "s3":
		return storage.NewS3(ctx,
			cfg.S3.Endpoint, cfg.S3.Bucket, cfg.S3.Region,
			cfg.S3.AccessKeyID, cfg.S3.SecretAccessKey, cfg.S3.UsePathStyle)
	default:
		return nil, fmt.Errorf("unknown backend %q (want fake | local | azure | s3)", backend)
	}
}

// setupService is the legacy 2-return convenience wrapper over setupComponents,
// used by the subcommands that only need (svc, db): init, import, sync, worker,
// validate-raw, validate-typed. serve and reprocess call setupComponents
// directly.
func setupService(ctx context.Context) (*sync.Service, *ent.Client, error) {
	c, err := setupComponents(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c.svc, c.db, nil
}
