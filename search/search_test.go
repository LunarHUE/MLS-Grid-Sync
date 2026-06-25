package search

import "testing"

// IsZipQuery is the load-bearing router for the single-box search
// (propertiesByAddress): a true result sends the query to an exact/prefix
// postal_code lookup instead of trigram, so a misclassification silently
// changes which SQL path runs. These pin the ZIP / not-ZIP boundary.
func TestIsZipQuery(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// ZIP-shaped: bare digits (any length — prefix or full) and ZIP+4.
		{"78704", true},
		{"787", true}, // partial → prefix lookup
		{"7", true},   // single digit is still all-digits
		{"78704-1234", true},
		{"  78704  ", true}, // surrounding whitespace is trimmed
		// Not ZIP-shaped: anything with a letter is an address.
		{"123 Main St", false},
		{"Austin", false},
		{"123 Main", false},
		{"78704 Main", false},
		// Malformed dash forms fall through to the all-digits check, which
		// fails because of the dash.
		{"78704-12", false},   // +4 must be exactly four digits
		{"7870-1234", false},  // base must be exactly five digits
		{"78704-123a", false}, // non-digit in the +4
		{"-1234", false},
		// Empty / whitespace is neither.
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := IsZipQuery(c.in); got != c.want {
			t.Errorf("IsZipQuery(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
