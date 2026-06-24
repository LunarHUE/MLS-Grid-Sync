package processor

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// forceSampling sets the drift sampler to fire on every record and resets the
// per-process dedupe map, restoring both afterward so tests don't leak state.
func forceSampling(t *testing.T) {
	t.Helper()
	prevRate := sampleRate
	sampleRate = 1.0
	driftMu.Lock()
	warnedDrift = map[string]bool{}
	driftFindings = map[rawoutput.Resource][]string{}
	driftMu.Unlock()
	t.Cleanup(func() {
		sampleRate = prevRate
		driftMu.Lock()
		warnedDrift = map[string]bool{}
		driftFindings = map[rawoutput.Resource][]string{}
		driftMu.Unlock()
	})
}

func warned(key string) bool {
	driftMu.Lock()
	defer driftMu.Unlock()
	return warnedDrift[key]
}

func warnedCount() int {
	driftMu.Lock()
	defer driftMu.Unlock()
	return len(warnedDrift)
}

// propertyRaw builds a minimal valid Property raw_output row (ListingKey +
// ModificationTimestamp are required by parseProperty) plus any extra keys.
func propertyRaw(t *testing.T, extra map[string]any) *ent.RawOutput {
	t.Helper()
	m := map[string]any{
		"ListingKey":            "L123",
		"ModificationTimestamp": "2026-06-24T00:00:00Z",
	}
	for k, v := range extra {
		m[k] = v
	}
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &ent.RawOutput{ID: uuid.New(), Payload: payload}
}

// TestCheckFieldDrift_NovelKeyWarnsOnce asserts a key absent from the baseline
// produces exactly one dedupe entry and a repeat call does not add another.
func TestCheckFieldDrift_NovelKeyWarnsOnce(t *testing.T) {
	forceSampling(t)

	raw := propertyRaw(t, map[string]any{"ZZZDriftTestField": 8.5})

	novelKey := string(rawoutput.ResourceProperty) + ".ZZZDriftTestField"
	checkFieldDrift(rawoutput.ResourceProperty, raw)
	if !warned(novelKey) {
		t.Fatalf("expected novel field %q to be warned, warnedDrift=%v", novelKey, warnedDrift)
	}
	if got := warnedCount(); got != 1 {
		t.Fatalf("expected exactly 1 warned key, got %d (%v)", got, warnedDrift)
	}

	// Second call (e.g. another record carrying the same new field) is deduped.
	checkFieldDrift(rawoutput.ResourceProperty, propertyRaw(t, map[string]any{"ZZZDriftTestField": 9.0}))
	if got := warnedCount(); got != 1 {
		t.Fatalf("expected dedupe to keep 1 warned key, got %d", got)
	}
}

// TestCheckFieldDrift_BaselineKeyIgnored asserts a key in the per-resource
// allowlist never warns.
func TestCheckFieldDrift_BaselineKeyIgnored(t *testing.T) {
	forceSampling(t)

	const key = "AllowlistedField"
	knownExtendedFields[rawoutput.ResourceProperty][key] = true
	t.Cleanup(func() { delete(knownExtendedFields[rawoutput.ResourceProperty], key) })

	checkFieldDrift(rawoutput.ResourceProperty, propertyRaw(t, map[string]any{key: "x"}))
	if warned(string(rawoutput.ResourceProperty) + "." + key) {
		t.Fatalf("baseline key %q should not warn", key)
	}
	if got := warnedCount(); got != 0 {
		t.Fatalf("expected no warnings for baseline-only payload, got %d (%v)", got, warnedDrift)
	}
}

// TestCheckFieldDrift_NoExtraFieldsNoWarn asserts a payload whose keys are all
// mapped produces no warning.
func TestCheckFieldDrift_NoExtraFieldsNoWarn(t *testing.T) {
	forceSampling(t)

	checkFieldDrift(rawoutput.ResourceProperty, propertyRaw(t, nil))
	if got := warnedCount(); got != 0 {
		t.Fatalf("expected no warnings for fully-mapped payload, got %d (%v)", got, warnedDrift)
	}
}

// TestCheckFieldDrift_NotSampledIsNoop asserts that with the sampler off no
// inspection happens even for a payload carrying a novel field.
func TestCheckFieldDrift_NotSampledIsNoop(t *testing.T) {
	prevRate := sampleRate
	sampleRate = 0.0
	driftMu.Lock()
	warnedDrift = map[string]bool{}
	driftMu.Unlock()
	t.Cleanup(func() {
		sampleRate = prevRate
		driftMu.Lock()
		warnedDrift = map[string]bool{}
		driftMu.Unlock()
	})

	checkFieldDrift(rawoutput.ResourceProperty, propertyRaw(t, map[string]any{"ZZZDriftTestField": 1}))
	if got := warnedCount(); got != 0 {
		t.Fatalf("expected no inspection when sampler off, got %d", got)
	}
}

// TestRenderDriftValue_Truncates asserts long values are clipped so a fat
// array/object can't flood the log line.
func TestRenderDriftValue_Truncates(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := renderDriftValue(long)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Fatalf("expected truncation suffix, got %q", got)
	}
	if len(got) > 220 { // 200 + suffix
		t.Fatalf("rendered value too long: %d", len(got))
	}
}

// TestDescribeDriftValue_ShapeOnly asserts the URL-bound descriptor reports the
// JSON kind and size but never the value's contents — the PII guard.
func TestDescribeDriftValue_ShapeOnly(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"string", "agent@example.com", "string, len 17"},
		{"number", float64(8.5), "number"},
		{"bool", true, "bool"},
		{"null", nil, "null"},
		{"array", []any{1, 2, 3}, "array, 3 items"},
		{"object", map[string]any{"a": 1, "b": 2}, "object, 2 keys"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describeDriftValue(c.value); got != c.want {
				t.Fatalf("describeDriftValue(%v) = %q, want %q", c.value, got, c.want)
			}
		})
	}

	// The descriptor must never leak the actual content.
	const secret = "agent@example.com"
	if got := describeDriftValue(secret); strings.Contains(got, secret) {
		t.Fatalf("descriptor leaked the value: %q", got)
	}
}

// TestWarnNovelField_IssueURLOmitsValue asserts the shareable issue URL carries
// only the value's shape, not its (possibly PII) contents — the body is built
// from describeDriftValue, so a decoded URL must never contain the raw value.
func TestWarnNovelField_IssueURLOmitsValue(t *testing.T) {
	const secret = "owner@example.com"
	title := "New unmapped field: " + string(rawoutput.ResourceProperty) + ".NewAgentEmail"
	u := version.NewIssueURL(title, "Example value shape: "+describeDriftValue(secret))

	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("issue URL unparseable: %v", err)
	}
	decodedBody := parsed.Query().Get("body")
	if strings.Contains(decodedBody, secret) {
		t.Fatalf("issue URL body leaked the value %q: %q", secret, decodedBody)
	}
	if !strings.Contains(decodedBody, "string, len") {
		t.Fatalf("issue URL body missing shape descriptor: %q", decodedBody)
	}
}

// TestDriftSummaryLine asserts the end-of-pass summary lists the novel fields
// seen for a resource, and is empty when nothing was flagged.
func TestDriftSummaryLine(t *testing.T) {
	forceSampling(t)

	if line, ok := driftSummaryLine(rawoutput.ResourceProperty); ok {
		t.Fatalf("expected no summary before any findings, got %q", line)
	}

	checkFieldDrift(rawoutput.ResourceProperty, propertyRaw(t, map[string]any{"ZZZDriftA": 1}))
	checkFieldDrift(rawoutput.ResourceProperty, propertyRaw(t, map[string]any{"ZZZDriftB": 2}))

	line, ok := driftSummaryLine(rawoutput.ResourceProperty)
	if !ok {
		t.Fatalf("expected a summary after findings")
	}
	for _, want := range []string{"2 novel", string(rawoutput.ResourceProperty), "ZZZDriftA", "ZZZDriftB"} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q missing %q", line, want)
		}
	}
}

// TestSetDriftSampleRate asserts the config knob clamps and disables correctly.
func TestSetDriftSampleRate(t *testing.T) {
	prev := sampleRate
	t.Cleanup(func() { sampleRate = prev })

	SetDriftSampleRate(2.0)
	if sampleRate != 1.0 {
		t.Fatalf(">1 should clamp to 1, got %v", sampleRate)
	}
	SetDriftSampleRate(0)
	if sampleRate != 0 {
		t.Fatalf("0 should disable (rate 0), got %v", sampleRate)
	}
	if sampled() {
		t.Fatalf("sampled() must be false when rate is 0")
	}
	sampleRate = 0.5
	SetDriftSampleRate(-1)
	if sampleRate != 0.5 {
		t.Fatalf("negative should be ignored, got %v", sampleRate)
	}
}
