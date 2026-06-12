package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AttachmentJob is the workflow record for downloading an attachment.
// One job per attempt; status transitions track lifecycle.
type AttachmentJob struct{ ent.Schema }

func (AttachmentJob) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.Skip(entgql.SkipAll),
		entsql.Annotation{Table: "attachment_job"},
	}
}

func (AttachmentJob) Mixin() []ent.Mixin {
	return []ent.Mixin{AuditMixin{}}
}

func (AttachmentJob) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuidV7).
			StorageKey("attachment_job_id"),
		field.String("media_key"),
		field.UUID("sync_event_id", uuid.UUID{}),
		field.UUID("raw_output_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("attachment_id", uuid.UUID{}).
			Optional().Nillable().
			Comment("Set when status reaches Succeeded"),
		field.Enum("status").
			Values("pending", "in_progress", "succeeded", "failed", "retrying", "permanently_failed", "canceled").
			Default("pending"),
		field.Int("attempt_count").Default(0),
		field.Text("last_error").Optional().Nillable(),
		field.Time("next_retry_at").Optional().Nillable(),
		field.Time("claimed_at").
			Optional().Nillable().
			Comment("When a worker claimed this job. Cleared when status returns to pending."),
		field.String("claimed_by").
			Optional().Nillable().
			Comment("Worker identity (hostname+pid or per-process UUID). Cleared on requeue."),
		field.Time("media_modified_at").
			Optional().Nillable().
			Comment("MediaModificationTimestamp the job was enqueued for. Used to gate re-enqueue on content change."),
		field.String("mime_type").Optional().Nillable(),
		field.Int("size_bytes").Optional().Nillable(),
		field.Strings("logs").Optional(),
	}
}

func (AttachmentJob) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
		index.Fields("status", "next_retry_at"),
		index.Fields("status", "claimed_at"),
		index.Fields("media_key"),
		index.Fields("sync_event_id"),
	}
}

func (AttachmentJob) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("media", Media.Type).
			Ref("attachment_jobs").
			Field("media_key").
			Unique().
			Required(),
		edge.From("sync_event", SyncEvent.Type).
			Ref("attachment_jobs").
			Field("sync_event_id").
			Unique().
			Required(),
		edge.From("raw_output", RawOutput.Type).
			Ref("attachment_jobs").
			Field("raw_output_id").
			Unique(),
		edge.From("attachment", Attachment.Type).
			Ref("attachment_jobs").
			Field("attachment_id").
			Unique(),
	}
}
