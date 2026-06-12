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
	"github.com/LunarHUE/MLS-Grid-Sync/graph"
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

		// Own mux, never http.DefaultServeMux — root.go's pprof import
		// registers /debug/pprof/ there, which must not face the network.
		mux := http.NewServeMux()
		mux.Handle("/query", graph.NewHandler(db))
		mux.Handle("/", graph.NewPlaygroundHandler("/query"))
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := sqlDB.PingContext(pingCtx); err != nil {
				http.Error(w, "database unreachable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ok")
		})

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		srv := &http.Server{Addr: addr, Handler: mux}
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
