package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"entgo.io/ent/schema/mixin"
	"github.com/google/uuid"
)

// MediaDataMixin: shared columns between Media and MediaVersion.
// Media is polymorphic — resource_record_key may reference Property, Member,
// or Office depending on resource_type. There's no hard Ent edge to those
// targets because Ent doesn't support polymorphic relations.
type MediaDataMixin struct{ mixin.Schema }

func (MediaDataMixin) Fields() []ent.Field {
	return []ent.Field{
		field.Enum("resource_type").
			Values("property", "member", "office").
			Comment("Discriminator for the polymorphic FK"),
		field.String("resource_record_key").
			Comment("References property.listing_key | member.member_key | office.office_key"),
		field.String("media_type").Optional().Nillable(),
		field.String("media_url").Optional().Nillable(),
		field.Int64("image_height").Optional().Nillable(),
		field.Int64("image_width").Optional().Nillable(),
		field.String("image_size_description").Optional().Nillable(),
		field.Text("long_description").Optional().Nillable(),
		field.Int16("order").Optional().Nillable(),
		field.Bool("preferred_photo_yn").Optional().Nillable(),
		field.Time("media_modification_timestamp").Optional().Nillable().
			Comment("MediaModificationTimestamp — Media's version of source_modified_at"),
	}
}

type Media struct{ ent.Schema }

func (Media) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "media"},
	}
}

func (Media) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
		MediaDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (Media) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("media_key").
			Comment("RESO MediaKey"),
		field.UUID("attachment_id", uuid.UUID{}).
			Optional().Nillable().
			Comment("Set when the binary has been downloaded into attachment"),
		field.UUID("current_version_id", uuid.UUID{}).
			Optional().Nillable(),
	}
}

func (Media) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("resource_type", "resource_record_key"),
		index.Fields("source_modified_at"),
		index.Fields("preferred_photo_yn"),
	}
}

func (Media) Edges() []ent.Edge {
	return []ent.Edge{
		// To the downloaded binary (set on job completion)
		edge.From("attachment", Attachment.Type).
			Ref("media").
			Field("attachment_id").
			Unique(),
		// Download workflow
		edge.To("attachment_jobs", AttachmentJob.Type),
	}
}
