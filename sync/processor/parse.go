package processor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

// Shared parse primitives for raw_output.payload -> typed struct conversion.
// Every resource parser (property, member, office, ...) uses these so
// timestamp / decimal / null conventions stay consistent across resources.

var jsonNull = []byte("null")

func isJSONNull(b json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace([]byte(b)), jsonNull)
}

func parseString(b json.RawMessage) (*string, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s == "" {
		return nil, nil
	}
	return &s, nil
}

func parseTime(b json.RawMessage) (*time.Time, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("expected RFC 3339 string, got %s", string(b))
	}
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("RFC 3339 parse %q: %w", s, err)
	}
	return &t, nil
}

// parseDate is parseTime's sibling for RESO Date-typed fields (YYYY-MM-DD,
// no time component). Routing is per-resource — see the comment blocks above
// each parser's dateFields map.
//
// Strict: layout "2006-01-02" only. A full RFC 3339 value here is rejected
// rather than silently truncated — that asymmetry mirrors parseTime, and
// the rejection is the tripwire that catches a Date field accidentally
// routed through parseTime or vice versa.
//
// Returns midnight UTC. The schema columns are timestamptz (Ent field.Time
// default); midnight-UTC is the encoding convention for date-only values.
// Consumers (GraphQL scalars, drift comparison) must treat these as dates,
// not instants.
func parseDate(b json.RawMessage) (*time.Time, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("expected YYYY-MM-DD string, got %s", string(b))
	}
	if s == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return nil, fmt.Errorf("date parse %q: %w", s, err)
	}
	return &t, nil
}

func parseDecimal(b json.RawMessage) (*decimal.Decimal, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var d decimal.Decimal
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func parseBool(b json.RawMessage) (*bool, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func parseInt16(b json.RawMessage) (*int16, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var v int16
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func parseInt32(b json.RawMessage) (*int32, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var v int32
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func parseInt64(b json.RawMessage) (*int64, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// parseStringArray returns nil for absent/null/empty inputs, otherwise the
// parsed []string. Empty-vs-absent distinction is intentionally collapsed —
// the schema treats both as "no array" and the GIN containment queries we
// care about don't distinguish either.
func parseStringArray(b json.RawMessage) (pq.StringArray, error) {
	if isJSONNull(b) {
		return nil, nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, nil
	}
	return pq.StringArray(arr), nil
}

// bucketExtendedFields turns whatever keys remain in raw (after a parser has
// consumed everything it recognizes) into a map[string]any suitable for the
// `extended_fields` JSONB column. Returns nil if raw is empty so the parser
// can leave the ExtendedFields struct field as nil rather than an empty map.
func bucketExtendedFields(raw map[string]json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	ext := make(map[string]any, len(raw))
	for k, v := range raw {
		var anyV any
		if err := json.Unmarshal(v, &anyV); err != nil {
			return nil, fmt.Errorf("extended field %s: %w", k, err)
		}
		ext[k] = anyV
	}
	return ext, nil
}
