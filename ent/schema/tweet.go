package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Tweet holds the schema definition for the Tweet entity.
// raw_json is the source of truth; every other field is derived from it.
type Tweet struct {
	ent.Schema
}

// Fields of the Tweet.
func (Tweet) Fields() []ent.Field {
	return []ent.Field{
		// Tweet ID assigned by Twitter, never auto-generated.
		field.String("id"),
		field.String("name").
			Comment("Author display name."),
		field.String("screen_name"),
		field.String("full_text"),
		field.Time("created_at").
			Comment("Tweet publish time."),
		field.String("permanent_url"),
		field.String("raw_json").
			Comment("Source of truth; every other field is derived from it."),
		field.Time("synced_at"),
		field.Enum("bookmarked").
			Values("yes", "formerly", "no").
			Comment("Whether the tweet is currently on the bookmarks list (yes), " +
				"was once but no longer is (formerly), or never was (no). " +
				"Written only by capture flows."),
		field.Int8("rating").
			Default(0).
			Min(-1).
			Max(5).
			Comment("Human judgment following Shotwell's model: " +
				"-1 = rejected, 0 = unrated, 1-5 = stars. " +
				"Capture flows seed it exactly once at creation " +
				"(bookmarking is itself a cheap positive judgment, hence the " +
				"lowest positive tier) and may never rewrite it afterwards; " +
				"only humans change it."),
	}
}

// Edges of the Tweet.
func (Tweet) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("media", Media.Type),
	}
}

// Annotations of the Tweet.
//
// ent does not emit CHECK constraints for enums or Min/Max bounds on SQLite,
// and its generated validators run only on mutations; reads coerce whatever
// the column holds without checking. These CHECKs are therefore the only
// guard against a hand-written sqlite3/datasette write storing values the Go
// side would never produce and would never notice.
func (Tweet) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Checks(map[string]string{
			"bookmarked_valid": "`bookmarked` IN ('yes', 'formerly', 'no')",
			"rating_range":     "`rating` BETWEEN -1 AND 5",
		}),
	}
}
