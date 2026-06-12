package mls

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"integer seconds", "30", 30 * time.Second},
		{"integer zero", "0", 0},
		{"integer negative", "-5", 0},
		{"http-date future", now.Add(45 * time.Second).UTC().Format(http.TimeFormat), 45 * time.Second},
		{"http-date past", now.Add(-1 * time.Minute).UTC().Format(http.TimeFormat), 0},
		{"unparseable", "next tuesday", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRetryAfter(tc.value, now)
			// HTTP-date parsing loses sub-second precision; allow 1s slack.
			assert.InDelta(t, tc.want.Seconds(), got.Seconds(), 1.0,
				"parseRetryAfter(%q) = %s, want ~%s", tc.value, got, tc.want)
		})
	}
}
