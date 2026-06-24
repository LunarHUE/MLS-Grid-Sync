package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/graph"
	"github.com/LunarHUE/MLS-Grid-Sync/health"
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
// / (playground HTML, open), and the three health endpoints (open), all
// wrapped in CORS. The health endpoints are registered directly on the mux —
// only /query is wrapped in RequireAPIKey — so they stay reachable by load
// balancers and operators without the API key.
func NewMux(client *ent.Client, h *health.Service, opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/query", RequireAPIKey(opts.APIKey, graph.NewHandler(client)))
	mux.Handle("/", graph.NewPlaygroundHandler("/query"))
	// /healthz: process alive (no DB). /readyz: can serve safely. /syncz: MLS
	// sync within thresholds. All return the same {healthy, checks} JSON.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, h.Live(r.Context()))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, h.Ready(r.Context()))
	})
	mux.HandleFunc("/syncz", func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, h.Sync(r.Context()))
	})
	return CORS(opts.AllowedOrigins, mux)
}

// writeHealth renders a HealthStatus as indented JSON, returning 200 when
// healthy and 503 otherwise so orchestrators and load balancers can gate on
// the status code alone.
func writeHealth(w http.ResponseWriter, hs health.HealthStatus) {
	w.Header().Set("Content-Type", "application/json")
	if hs.Healthy {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(hs)
}
