package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/health"
	"github.com/LunarHUE/MLS-Grid-Sync/search"
	"github.com/LunarHUE/MLS-Grid-Sync/server"
)

var serveAddr string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the GraphQL API over HTTP",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Deliberately not setupComponents: the read-only API server
		// needs no MLS token, no storage backend, and must not run
		// schema migrations (sync/worker own the schema).
		sqlDB, err := sql.Open("postgres", appConfig.Database.DSN)
		if err != nil {
			return fmt.Errorf("failed opening connection to postgres: %w", err)
		}
		drv := entsql.OpenDB(dialect.Postgres, sqlDB)
		db := ent.NewClient(ent.Driver(drv))
		defer db.Close()

		addr := serveAddr
		if addr == "" {
			addr = appConfig.Server.Addr
		}
		if addr == "" {
			addr = ":8080"
		}

		hsvc := health.NewService(db, sqlDB.PingContext, health.Thresholds{
			SyncMaxStaleness:      appConfig.Health.SyncMaxStaleness,
			MaxRawPending:         appConfig.Health.MaxRawPending,
			MaxAttachmentFailures: appConfig.Health.MaxAttachmentFailures,
		}, time.Now).WithTrigramProbe(func(ctx context.Context) error {
			return search.CheckExtension(ctx, sqlDB)
		})

		// Own mux, never http.DefaultServeMux — root.go's pprof import
		// registers /debug/pprof/ there, which must not face the network.
		handler := server.NewMux(db, hsvc, server.Options{
			APIKey:         appConfig.Server.APIKey,
			AllowedOrigins: server.SplitOrigins(appConfig.Server.CORSAllowedOrigins),
		})

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		srv := &http.Server{Addr: addr, Handler: handler}
		errCh := make(chan error, 1)
		go func() {
			log.Infof("serve: GraphQL playground on http://%s/ (API at /query)", addr)
			errCh <- srv.ListenAndServe()
		}()

		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-ctx.Done():
			log.Infof("serve: shutdown signal received, draining connections...")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("graceful shutdown: %w", err)
			}
			return nil
		}
	},
}

func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", "",
		"Listen address (e.g. :8080). Overrides server.addr / MLS_SYNC_SERVER_ADDR.")
	rootCmd.AddCommand(serveCmd)
}
