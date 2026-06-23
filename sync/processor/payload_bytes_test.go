package processor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
)

// TestRawOutputPayload_IsRawBytes locks the raw_output.payload contract:
// ent must surface it as json.RawMessage (raw bytes), NOT a decoded
// map[string]any. The whole point of the 2026-06 fix was to stop ent from
// decoding the JSONB column to a map that every processor then re-marshalled
// back to bytes per record (the double-marshal that dominated CPU + GC — see
// docs/profiling.md). The assignment below fails to COMPILE if the ent schema
// (ent/schema/raw-output.go) ever regresses to a map type.
func TestRawOutputPayload_IsRawBytes(t *testing.T) {
	var raw ent.RawOutput
	var _ json.RawMessage = raw.Payload // compile-time type lock — do not remove

	// Runtime belt-and-suspenders: the field's dynamic type is the bytes alias.
	require.IsType(t, json.RawMessage(nil), raw.Payload)
}

// TestPayload_NotReMarshalled guards against the double-marshal anti-pattern
// returning. raw_output.payload arrives as json.RawMessage already, so the
// processors (and the validate/worker paths that read it) must parse those
// bytes directly — never json.Marshal(raw.Payload) to rebuild what we were
// just handed. A reviewer who "helpfully" re-adds the marshal reintroduces the
// per-record allocation churn this whole change removed; this test fails the
// build instead. See docs/profiling.md (R1, 2026-06 case study).
func TestPayload_NotReMarshalled(t *testing.T) {
	// json.Marshal( <anything> .Payload ) — the forbidden shape.
	forbidden := regexp.MustCompile(`json\.Marshal\([^)]*\.Payload\b`)

	// The processor package itself, plus the sibling sync/ package (which holds
	// the worker's resolveMediaURL and the raw_output write path). Both read
	// raw_output.payload; both must keep it as bytes.
	dirs := []string{".", ".."}
	scanned := 0
	for _, dir := range dirs {
		for _, f := range goSourceFiles(t, dir) {
			b, err := os.ReadFile(f)
			require.NoError(t, err)
			scanned++
			assert.NotRegexpf(t, forbidden, string(b),
				"%s re-marshals raw_output.Payload — it is already json.RawMessage; "+
					"parse the bytes directly (see TestPayload_NotReMarshalled)", f)
		}
	}
	require.Positive(t, scanned, "guard scanned no source files — path resolution broke")
}

// goSourceFiles lists non-test .go files directly under dir (non-recursive).
// Test files are skipped so this guard never trips on its own regex source.
func goSourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out
}
