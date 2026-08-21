package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Media holds the schema definition for the Media entity.
type Media struct {
	ent.Schema
}

// Fields of the Media.
func (Media) Fields() []ent.Field {
	return []ent.Field{
		// Media ID assigned by Twitter, never auto-generated.
		field.String("id"),
		// Explicit FK field bound to the tweet edge.
		field.String("tweet_id"),
		// Named position rather than index: "index" is a reserved word in
		// SQLite, which breaks hand-written queries in sqlite3/datasette.
		field.Int("position").
			Default(0).
			Comment("Position within the tweet."),
		field.String("url"),
		field.String("type").
			Comment("photo, video, or animated_gif today; kept an open string " +
				"so inserts survive new types x.com may introduce."),
		field.Bool("downloaded").
			Default(false),
		field.Bool("failed").
			Default(false),
		field.Int("retry_count").
			Default(0),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Media.
func (Media) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("tweet", Tweet.Type).
			Ref("media").
			Field("tweet_id").
			Unique().
			Required(),
	}
}

// Indexes of the Media.
//
// SQLite does not index FK columns on its own and neither does ent; without
// this, every media-of-tweet lookup scans the whole table.
func (Media) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tweet_id"),
	}
}
