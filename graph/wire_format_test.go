package graph_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Wire-format tests: the custom scalars cross the wire in specific JSON
// shapes (Decimal→string, int16→number, UUID→string, StringArray→array,
// Map→object, Time→RFC3339 string). Asserted on raw map[string]any so a
// silent shape change (e.g. Decimal becoming a float) fails loudly.

const wireQuery = `query($id: ID!) {
	node(id: $id) {
		... on Property {
			listPrice
			taxYear
			currentVersionID
			appliances
			extendedFields
			sourceModifiedAt
		}
	}
}`

func TestWireFormat_PropertyScalars(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	versionID := uuid.New()
	client.Property.Create().
		SetID("prop-wire").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetListPrice(decimal.RequireFromString("450000.50")).
		SetTaxYear(2024).
		SetCurrentVersionID(versionID).
		SetAppliances(pq.StringArray{"Dishwasher", "Microwave"}).
		SetExtendedFields(map[string]any{"ACT_Foo": "bar"}).
		SaveX(context.Background())

	var data map[string]any
	testutil.GQL(t, srv, wireQuery, map[string]any{"id": "prop-wire"}, &data)
	node, ok := data["node"].(map[string]any)
	require.True(t, ok, "node missing: %v", data)

	// Decimal → JSON string, precision-safe.
	listPrice, ok := node["listPrice"].(string)
	require.True(t, ok, "listPrice must be a JSON string, got %T", node["listPrice"])
	parsed, err := decimal.NewFromString(listPrice)
	require.NoError(t, err)
	assert.True(t, parsed.Equal(decimal.RequireFromString("450000.50")), "listPrice %q ≠ 450000.50", listPrice)

	// int16-backed → JSON number.
	taxYear, ok := node["taxYear"].(float64)
	require.True(t, ok, "taxYear must be a JSON number, got %T", node["taxYear"])
	assert.Equal(t, float64(2024), taxYear)

	// UUID → JSON string.
	cvid, ok := node["currentVersionID"].(string)
	require.True(t, ok, "currentVersionID must be a JSON string, got %T", node["currentVersionID"])
	assert.Equal(t, versionID.String(), cvid)

	// StringArray → JSON array of strings.
	appliances, ok := node["appliances"].([]any)
	require.True(t, ok, "appliances must be a JSON array, got %T", node["appliances"])
	assert.Equal(t, []any{"Dishwasher", "Microwave"}, appliances)

	// Map → JSON object.
	extended, ok := node["extendedFields"].(map[string]any)
	require.True(t, ok, "extendedFields must be a JSON object, got %T", node["extendedFields"])
	assert.Equal(t, "bar", extended["ACT_Foo"])

	// Time → RFC3339 string.
	modifiedAt, ok := node["sourceModifiedAt"].(string)
	require.True(t, ok, "sourceModifiedAt must be a JSON string, got %T", node["sourceModifiedAt"])
	_, err = time.Parse(time.RFC3339, modifiedAt)
	assert.NoError(t, err, "sourceModifiedAt %q not RFC3339", modifiedAt)
}

func TestWireFormat_NullsForUnsetFields(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedProperty(t, client, "prop-bare", true)

	var data map[string]any
	testutil.GQL(t, srv, wireQuery, map[string]any{"id": "prop-bare"}, &data)
	node, ok := data["node"].(map[string]any)
	require.True(t, ok)

	assert.Nil(t, node["listPrice"])
	assert.Nil(t, node["taxYear"])
	assert.Nil(t, node["currentVersionID"])
	assert.Nil(t, node["appliances"], "unset StringArray must be null, not []")
	assert.Nil(t, node["extendedFields"])
}

// TestWireFormat_PropertyVersionParity guards the conversion resolvers
// duplicated between Property and PropertyVersion: the same scalar kinds
// must produce the same wire shapes on the version type.
func TestWireFormat_PropertyVersionParity(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	v := client.PropertyVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(propertyversion.ChangeTypeInsert).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetListingKey("LK-wire").
		SetSyncEventID(uuid.New()).
		SetListPrice(decimal.RequireFromString("999.99")).
		SetTaxYear(2023).
		SetAppliances(pq.StringArray{"Oven"}).
		SaveX(context.Background())

	var data map[string]any
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) {
			... on PropertyVersion {
				listPrice
				taxYear
				appliances
				validFrom
				syncEventID
			}
		}
	}`, map[string]any{"id": v.ID}, &data)
	node, ok := data["node"].(map[string]any)
	require.True(t, ok, "node missing: %v", data)

	listPrice, ok := node["listPrice"].(string)
	require.True(t, ok, "version listPrice must be a JSON string, got %T", node["listPrice"])
	parsed, err := decimal.NewFromString(listPrice)
	require.NoError(t, err)
	assert.True(t, parsed.Equal(decimal.RequireFromString("999.99")))

	_, ok = node["taxYear"].(float64)
	assert.True(t, ok, "version taxYear must be a JSON number, got %T", node["taxYear"])

	appliances, ok := node["appliances"].([]any)
	require.True(t, ok, "version appliances must be a JSON array, got %T", node["appliances"])
	assert.Equal(t, []any{"Oven"}, appliances)

	validFrom, ok := node["validFrom"].(string)
	require.True(t, ok)
	_, err = time.Parse(time.RFC3339, validFrom)
	assert.NoError(t, err)

	syncEventID, ok := node["syncEventID"].(string)
	require.True(t, ok, "syncEventID must be a JSON string, got %T", node["syncEventID"])
	_, err = uuid.Parse(syncEventID)
	assert.NoError(t, err)
}
