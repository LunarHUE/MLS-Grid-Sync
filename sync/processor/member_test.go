package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/member"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/memberversion"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/internal/testutil"
)

func runMemberProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewMemberProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertMemberRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	memberKey, _ := payload["MemberKey"].(string)
	require.NotEmpty(t, memberKey, "test payload must include MemberKey")
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourceMember).
		SetSourceKey(memberKey).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(payload).
		SaveX(ctx)
}

func setupMemberTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)

	src := client.SourceSystem.Create().
		SetID("test-src").
		SetSourceSystemName("test").
		SaveX(ctx)

	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("member").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

func TestMemberProcess_FreshInsert(t *testing.T) {
	ctx, client, evID := setupMemberTest(t)

	// office_key is a soft key now (no DB FK) — see Member.Edges() / plan §1.
	// Seeding the Office is no longer required for insert; we keep it here so
	// the GraphQL Member.office resolver finds a real row when this test's
	// member is later traversed.
	client.Office.Create().
		SetID("OFF-1").
		SetSourceModifiedAt(time.Now().UTC()).
		SaveX(ctx)

	ts := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"MemberFirstName":       "Jane",
		"MemberLastName":        "Smith",
		"MemberMlsId":           "AGT123",
		"OfficeKey":             "OFF-1",
	}
	raw := insertMemberRaw(t, client, ctx, evID, payload, ts)
	require.NoError(t, runMemberProcess(t, client, ctx, raw))

	m, err := client.Member.Query().Where(member.IDEQ("MEM-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, m.MemberFirstName)
	assert.Equal(t, "Jane", *m.MemberFirstName)
	require.NotNil(t, m.MemberLastName)
	assert.Equal(t, "Smith", *m.MemberLastName)
	require.NotNil(t, m.CurrentVersionID)
	require.NotNil(t, m.OfficeKey)
	assert.Equal(t, "OFF-1", *m.OfficeKey)

	versions := client.MemberVersion.Query().
		Where(memberversion.MemberKeyEQ("MEM-1")).
		AllX(ctx)
	require.Len(t, versions, 1)
	v := versions[0]
	assert.Equal(t, memberversion.ChangeTypeInsert, v.ChangeType)
	assert.Nil(t, v.ValidTo)
	assert.Nil(t, v.ChangedFields, "fresh insert has nil changed_fields")
}

func TestMemberProcess_UpdateWithDiff(t *testing.T) {
	ctx, client, evID := setupMemberTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"MemberFirstName":       "Jane",
		"MemberLastName":        "Smith",
	}, ts1)
	require.NoError(t, runMemberProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	second := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MemberFirstName":       "Janet",
		"MemberLastName":        "Smith",
	}, ts2)
	require.NoError(t, runMemberProcess(t, client, ctx, second))

	versions := client.MemberVersion.Query().
		Where(memberversion.MemberKeyEQ("MEM-1")).
		Order(ent.Asc(memberversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2, "two versions: old (closed) and new (open)")
	assert.NotNil(t, versions[0].ValidTo, "first version closed")
	assert.Nil(t, versions[1].ValidTo, "second version open")
	assert.Equal(t, memberversion.ChangeTypeUpdate, versions[1].ChangeType)
	require.NotNil(t, versions[1].ChangedFields)
	diff, ok := versions[1].ChangedFields["member_first_name"].(map[string]any)
	require.True(t, ok, "expected member_first_name diff entry")
	assert.Equal(t, "Jane", diff["old"])
	assert.Equal(t, "Janet", diff["new"])

	// Entity has the new value.
	m, err := client.Member.Query().Where(member.IDEQ("MEM-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, m.MemberFirstName)
	assert.Equal(t, "Janet", *m.MemberFirstName)
}

func TestMemberProcess_UpdateClearsField(t *testing.T) {
	// Bug-1 regression: ent's SetNillableX(nil) is a no-op. When a payload
	// omits a previously-set field, the entity row used to keep the stale
	// value while the version row recorded NULL — entity/version drift.
	ctx, client, evID := setupMemberTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"MemberFirstName":       "Jane",
		"MemberLastName":        "Smith",
	}, ts1)
	require.NoError(t, runMemberProcess(t, client, ctx, first))

	// Sanity: field set.
	m, err := client.Member.Query().Where(member.IDEQ("MEM-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, m.MemberFirstName)
	assert.Equal(t, "Jane", *m.MemberFirstName)

	// Second payload OMITS MemberFirstName entirely → clear semantics.
	ts2 := ts1.Add(time.Hour)
	second := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MemberLastName":        "Smith",
	}, ts2)
	require.NoError(t, runMemberProcess(t, client, ctx, second))

	m2, err := client.Member.Query().Where(member.IDEQ("MEM-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, m2.MemberFirstName, "entity row clears MemberFirstName when payload omits it")

	versions := client.MemberVersion.Query().
		Where(memberversion.MemberKeyEQ("MEM-1")).
		Order(ent.Asc(memberversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Nil(t, versions[1].MemberFirstName, "new version row also NULL — no entity/version drift")
	require.NotNil(t, versions[1].ChangedFields)
	diff, ok := versions[1].ChangedFields["member_first_name"].(map[string]any)
	require.True(t, ok, "diff records the clear")
	assert.Equal(t, "Jane", diff["old"])
	assert.Nil(t, diff["new"])
}

func TestMemberProcess_AlreadyTombstonedIsNoop(t *testing.T) {
	// Bug-2 regression: a second MlgCanView=false record on an entity that's
	// already tombstoned must skip. Without the skip we'd write another
	// delete version every time the upstream re-asserts the deletion.
	ctx, client, evID := setupMemberTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"MemberFirstName":       "Jane",
	}, ts1)
	require.NoError(t, runMemberProcess(t, client, ctx, first))

	// First MlgCanView=false: writes the delete version, tombstones entity.
	ts2 := ts1.Add(time.Hour)
	tomb := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runMemberProcess(t, client, ctx, tomb))

	// Second MlgCanView=false at a later timestamp: must no-op.
	ts3 := ts2.Add(time.Hour)
	tomb2 := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts3.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts3)
	require.NoError(t, runMemberProcess(t, client, ctx, tomb2))

	versions := client.MemberVersion.Query().
		Where(memberversion.MemberKeyEQ("MEM-1")).
		AllX(ctx)
	require.Len(t, versions, 2, "second tombstone is no-op — only insert + delete versions exist")
}

func TestMemberProcess_MlgCanViewFalseTombstone(t *testing.T) {
	ctx, client, evID := setupMemberTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"MemberFirstName":       "Jane",
	}, ts1)
	require.NoError(t, runMemberProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	tomb := insertMemberRaw(t, client, ctx, evID, map[string]any{
		"MemberKey":             "MEM-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runMemberProcess(t, client, ctx, tomb))

	m, err := client.Member.Query().Where(member.IDEQ("MEM-1")).Only(ctx)
	require.NoError(t, err)
	assert.False(t, m.MlgCanView, "entity tombstoned")

	versions := client.MemberVersion.Query().
		Where(memberversion.MemberKeyEQ("MEM-1")).
		Order(ent.Asc(memberversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Equal(t, memberversion.ChangeTypeDelete, versions[1].ChangeType)
	assert.Nil(t, versions[1].ChangedFields, "delete carries no diff")
}

// Orphan office reference — Member.office_key references an Office absent
// from this feed. Pre-plan §1 this raised pq 23503 on the FK; soft-key
// model accepts the row and the resolver returns null at query time.
func TestMemberProcess_OrphanOfficeInserts(t *testing.T) {
	ctx, client, evID := setupMemberTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"MemberKey":             "MEM-ORPHAN-1",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"MemberFirstName":       "Ghost",
		"OfficeKey":             "NO-SUCH-OFFICE-1",
	}
	raw := insertMemberRaw(t, client, ctx, evID, payload, ts)

	require.NoError(t, runMemberProcess(t, client, ctx, raw),
		"orphan OfficeKey must not block Member insert (FK constraint dropped in plan §1)")

	ent_, err := client.Member.Query().Where(member.IDEQ("MEM-ORPHAN-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, ent_.OfficeKey)
	assert.Equal(t, "NO-SUCH-OFFICE-1", *ent_.OfficeKey)

	officeCount, err := client.Office.Query().Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, officeCount, "no phantom Office row was upserted")
}
