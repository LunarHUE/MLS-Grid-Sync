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
		// Deliberately not setupComponents: the read-only API server needs no
		// MLS token and must not run schema migrations (sync/init own the
		// schema). It does need a storage backend now — /media/{mediaKey}
		// streams attachment binaries — but only the read half.
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

		// A storage backend the media route can read from. Non-fatal: an
		// unconfigured or unreachable backend should cost you the image
		// endpoint, not the whole API, so log and carry on with it nil —
		// NewMux then simply does not register the route.
		//
		// Unless the route is redirecting, in which case it never reads an
		// object and the missing backend costs nothing. That is the supported
		// way to run serve without storage credentials at all.
		mediaStorer, err := newStorer(cmd.Context(), appConfig.Storage)
		if err != nil {
			if appConfig.Server.MediaPublicBaseURL != "" {
				log.Infof("serve: no storage backend (%v); /media redirects to %s",
					err, appConfig.Server.MediaPublicBaseURL)
			} else {
				log.Warnf("serve: media endpoint disabled — storage backend: %v", err)
			}
			mediaStorer = nil
		}

		// Own mux, never http.DefaultServeMux — root.go's pprof import
		// registers /debug/pprof/ there, which must not face the network.
		handler := server.NewMux(db, hsvc, server.Options{
			APIKey:         appConfig.Server.APIKey,
			AllowedOrigins: server.SplitOrigins(appConfig.Server.CORSAllowedOrigins),
			Playground:     appConfig.Server.PlaygroundEnabled,
			Introspection:  appConfig.Server.IntrospectionEnabled,
			Media: server.MediaOptions{
				Storer:        mediaStorer,
				KeyPrefix:     appConfig.Storage.KeyPrefix,
				PublicBaseURL: appConfig.Server.MediaPublicBaseURL,
			},
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
