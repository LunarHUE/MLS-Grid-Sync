package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/graph"
	"github.com/LunarHUE/MLS-Grid-Sync/health"
	"github.com/LunarHUE/MLS-Grid-Sync/tracing"
)

// Options configures the API HTTP stack. Decoupled from the config
// package on purpose; cmd/serve.go maps config.ServerConfig onto it.
type Options struct {
	APIKey         string   // empty = auth disabled
	AllowedOrigins []string // empty or containing "*" = allow any origin

	// Playground serves the GraphiQL UI at /. It is intentionally NOT behind
	// RequireAPIKey — a UI cannot send the header before you have typed it in
	// — so enabling it publishes the existence and shape of the API to anyone
	// who reaches the host. Off unless explicitly enabled.
	Playground bool

	// Introspection allows __schema/__type queries on /query.
	Introspection bool

	// Media registers GET /media/{mediaKey} when its Storer is non-nil or a
	// PublicBaseURL is set.
	Media MediaOptions
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
	mux.Handle("/query", RequireAPIKey(opts.APIKey, graph.NewHandler(client, graph.HandlerOptions{
		Introspection: opts.Introspection,
	})))
	if opts.Playground {
		mux.Handle("/", graph.NewPlaygroundHandler("/query"))
	}
	// Binary media by RESO MediaKey. Deliberately NOT key-gated: browsers
	// request these straight from <img>, which cannot carry the header. The
	// keys are opaque and the handler only ever serves what sync already
	// pulled, so this exposes no more than the listing pages themselves do.
	// A PublicBaseURL alone is enough: redirect mode never reads an object, so
	// it needs no backend and no storage credentials on this process.
	if opts.Media.Storer != nil || opts.Media.PublicBaseURL != "" {
		mux.Handle("GET /media/{mediaKey}", newMediaHandler(client, opts.Media))
	}
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
	// tracing is outermost so every request — including CORS preflights and
	// health probes — gets a trace ID, an echoed response header, and a
	// completion log line. The trace ID it stores in the context flows into the
	// GraphQL operation log for end-to-end correlation.
	return tracing.Middleware(CORS(opts.AllowedOrigins, mux))
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
