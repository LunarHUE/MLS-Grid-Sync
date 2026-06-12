package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/graph"
)

// Options configures the API HTTP stack. Decoupled from the config
// package on purpose; cmd/serve.go maps config.ServerConfig onto it.
type Options struct {
	APIKey         string   // empty = auth disabled
	AllowedOrigins []string // empty or containing "*" = allow any origin
}

// SplitOrigins comma-splits a CORS allowlist string, trims surrounding
// spaces, and drops empty entries.
func SplitOrigins(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NewMux assembles the serve mux: /query (API key + GraphQL),
// / (playground HTML, open), /healthz (ping, open), all wrapped in CORS.
func NewMux(client *ent.Client, ping func(context.Context) error, opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/query", RequireAPIKey(opts.APIKey, graph.NewHandler(client)))
	mux.Handle("/", graph.NewPlaygroundHandler("/query"))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ping(pingCtx); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return CORS(opts.AllowedOrigins, mux)
}
