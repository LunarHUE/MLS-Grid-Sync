package mls

import (
	"encoding/json"
	"testing"
)

// TestODataResponse_ParsesCount pins the @odata.count wiring the pull progress
// bar depends on for its denominator. A pointer distinguishes "server omitted
// the count" (nil → count-only display) from a genuine zero.
func TestODataResponse_ParsesCount(t *testing.T) {
	var present ODataResponse
	if err := json.Unmarshal([]byte(`{"@odata.count":589081,"@odata.nextLink":"next","value":[]}`), &present); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if present.Count == nil || *present.Count != 589081 {
		t.Errorf("Count = %v, want 589081", present.Count)
	}
	if present.NextLink != "next" {
		t.Errorf("NextLink = %q", present.NextLink)
	}

	var absent ODataResponse
	if err := json.Unmarshal([]byte(`{"value":[]}`), &absent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if absent.Count != nil {
		t.Errorf("Count should be nil when @odata.count is absent, got %v", *absent.Count)
	}
}
