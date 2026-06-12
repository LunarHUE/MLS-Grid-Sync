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

// Attachment is deduped binary file storage, keyed by content hash.
// Independent of any one Media record — two Media records sharing a URL
// (or content) resolve to one Attachment.
type Attachment struct{ ent.Schema }

func (Attachment) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.Skip(entgql.SkipAll),
		entsql.Annotation{Table: "attachment"},
	}
}

func (Attachment) Mixin() []ent.Mixin {
	return []ent.Mixin{AuditMixin{}}
}

func (Attachment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuidV7).
			StorageKey("attachment_id"),
		field.String("source_url").
			Comment("Upstream URL where the file was fetched from"),
		field.String("source_hash").
			Comment("Content hash for dedupe (sha256 hex)"),
		field.String("host_url").
			Comment("Where we serve it from (S3/CDN URL)"),
		field.String("mime_type").Optional().Nillable(),
		field.Int("size_bytes").Optional().Nillable(),
		field.Time("deleted_at").Optional().Nillable(),
	}
}

func (Attachment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("source_hash").Unique(),
		index.Fields("source_url"),
		index.Fields("deleted_at"),
	}
}

func (Attachment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("media", Media.Type),
		edge.To("attachment_jobs", AttachmentJob.Type),
	}
}
