package processor

import (
	"time"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

// Shared set-or-clear helpers for entity UPDATE paths.
//
// Why this exists: ent's SetNillableX(nil) is a no-op — it does NOT clear the
// column. On the UPDATE path, a nil pointer in our typed Fields struct means
// "the field was absent from the payload," which MLS Grid uses to signal a
// clear. Using SetNillableX(nil) would leave the entity row stale while the
// version row (built fresh from the typed struct → NULL) records the clear,
// causing entity/version drift. These helpers route nil → Clear() explicitly.
//
// CREATE paths don't need this: columns start NULL, and SetNillableX(nil)
// correctly leaves them NULL.
//
// The return type of every SetX / ClearX method is the same builder type per
// entity (e.g. *MemberUpdateOne). Generics let one helper serve every entity.

func setOrClearStr[B any](v *string, set func(string) B, clear func() B) {
	if v != nil {
		set(*v)
	} else {
		clear()
	}
}

func setOrClearTime[B any](v *time.Time, set func(time.Time) B, clear func() B) {
	if v != nil {
		set(*v)
	} else {
		clear()
	}
}

func setOrClearBool[B any](v *bool, set func(bool) B, clear func() B) {
	if v != nil {
		set(*v)
	} else {
		clear()
	}
}

func setOrClearInt16[B any](v *int16, set func(int16) B, clear func() B) {
	if v != nil {
		set(*v)
	} else {
		clear()
	}
}

func setOrClearInt32[B any](v *int32, set func(int32) B, clear func() B) {
	if v != nil {
		set(*v)
	} else {
		clear()
	}
}

func setOrClearInt64[B any](v *int64, set func(int64) B, clear func() B) {
	if v != nil {
		set(*v)
	} else {
		clear()
	}
}

func setOrClearDecimal[B any](v *decimal.Decimal, set func(decimal.Decimal) B, clear func() B) {
	if v != nil {
		set(*v)
	} else {
		clear()
	}
}

// setOrClearSlice handles []string slice fields (MlgCanUse etc).
func setOrClearSlice[B any](v []string, set func([]string) B, clear func() B) {
	if v != nil {
		set(v)
	} else {
		clear()
	}
}

// setOrClearStringArray handles pq.StringArray columns (RESO text[] arrays).
func setOrClearStringArray[B any](v pq.StringArray, set func(pq.StringArray) B, clear func() B) {
	if v != nil {
		set(v)
	} else {
		clear()
	}
}

// setOrClearMap handles map[string]any columns (ExtendedFields JSONB).
func setOrClearMap[B any](v map[string]any, set func(map[string]any) B, clear func() B) {
	if v != nil {
		set(v)
	} else {
		clear()
	}
}
