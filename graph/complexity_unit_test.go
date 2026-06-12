package graph

import "testing"

func TestEffectivePageSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		first, last *int
		want        int
	}{
		{"both nil defaults to max", nil, nil, MaxPageSize},
		{"first small", intPtr(10), nil, 10},
		{"first over max clamped", intPtr(2_000_000_000), nil, MaxPageSize},
		{"first negative floored to zero", intPtr(-5), nil, 0},
		{"last small", nil, intPtr(25), 25},
		{"last over max clamped", nil, intPtr(900), MaxPageSize},
		{"last negative floored to zero", nil, intPtr(-1), 0},
		{"first takes precedence over last", intPtr(7), intPtr(900), 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := effectivePageSize(tt.first, tt.last); got != tt.want {
				t.Errorf("effectivePageSize = %d, want %d", got, tt.want)
			}
		})
	}
}
