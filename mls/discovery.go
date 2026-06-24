package mls

import (
	"context"
	"encoding/json"
	"sort"
)

// ProbeOriginatingSystems fires one request against DiscoveryURL and extracts
// distinct OriginatingSystemName values from the response. Returns the
// deduplicated, sorted list (or empty if records came back but none carried
// the field).
//
// It lives here, not in cmd, so both the `systems` command and the `doctor`
// command can share one implementation without an import cycle. The fetcher
// interface lets callers inject httptest fixtures without a real *Client.
func ProbeOriginatingSystems(ctx context.Context, fetcher PageFetcher, v2URL string) ([]string, error) {
	resp, err := fetcher.FetchPage(ctx, DiscoveryURL(v2URL))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, raw := range resp.Value {
		var record struct {
			OriginatingSystemName string `json:"OriginatingSystemName"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			continue
		}
		if record.OriginatingSystemName != "" {
			seen[record.OriginatingSystemName] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
