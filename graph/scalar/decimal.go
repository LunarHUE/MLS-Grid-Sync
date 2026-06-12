// Package scalar provides gqlgen marshaling for custom scalar types used in
// generated where-inputs. The Decimal binding here lets entgql's generated
// *WhereInput structs (which hold shopspring/decimal.Decimal values for
// numeric predicates like listPriceGTE) round-trip through GraphQL.
//
// The field-level Decimal output binding (Property.listPrice etc.) stays on
// graphql.String via the manual conversion resolvers; this binding only
// matters where gqlgen needs to (un)marshal a decimal.Decimal directly —
// i.e. where-input arguments.
package scalar

import (
	"encoding/json"
	"fmt"

	"github.com/99designs/gqlgen/graphql"
	"github.com/shopspring/decimal"
)

// MarshalDecimal emits the decimal as a JSON string, preserving exact
// precision (no float rounding).
func MarshalDecimal(d decimal.Decimal) graphql.Marshaler {
	return graphql.MarshalString(d.String())
}

// UnmarshalDecimal parses a decimal from a GraphQL string or a JSON number,
// using exact decimal parsing (decimal.NewFromString) so no precision is
// lost. Any other input type is an error.
func UnmarshalDecimal(v any) (decimal.Decimal, error) {
	switch val := v.(type) {
	case string:
		return decimal.NewFromString(val)
	case json.Number:
		return decimal.NewFromString(val.String())
	default:
		return decimal.Decimal{}, fmt.Errorf("Decimal must be a string or number, got %T", v)
	}
}
