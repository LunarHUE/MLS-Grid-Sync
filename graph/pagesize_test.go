package graph

import "testing"

func intPtr(i int) *int { return &i }

func TestClampPage(t *testing.T) {
	t.Parallel()

	deref := func(p *int) any {
		if p == nil {
			return nil
		}
		return *p
	}

	tests := []struct {
		name        string
		first, last *int
		wantFirst   any
		wantLast    any
	}{
		{"both nil defaults to max first", nil, nil, MaxPageSize, nil},
		{"first over max clamped", intPtr(501), nil, MaxPageSize, nil},
		{"last over max clamped", nil, intPtr(501), nil, MaxPageSize},
		{"first zero passes through", intPtr(0), nil, 0, nil},
		{"first negative passes through", intPtr(-1), nil, -1, nil},
		{"first small passes through", intPtr(10), nil, 10, nil},
		{"both over max both clamped", intPtr(501), intPtr(900), MaxPageSize, MaxPageSize},
		{"both small passthrough", intPtr(5), intPtr(7), 5, 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotFirst, gotLast := clampPage(tt.first, tt.last)
			if deref(gotFirst) != tt.wantFirst {
				t.Errorf("first = %v, want %v", deref(gotFirst), tt.wantFirst)
			}
			if deref(gotLast) != tt.wantLast {
				t.Errorf("last = %v, want %v", deref(gotLast), tt.wantLast)
			}
		})
	}
}
