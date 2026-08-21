package main

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/rotisserie/eris"

	"twitter-bookmarks-downloader/ent"
)

/*
raw_json is the source of truth (REBUILD_NOTES 2.9); every other tweet and
media column is a function of it. This deriver is that function: it takes one
raw tweet JSON document and produces a neutral derived record that callers
turn into database writes. It is shared by the bookmarks sync, the GORM-era
migration, and the archive rebuild, so parsing fixes made here propagate to
all three.

Two formats exist in the wild, with zero overlap (REBUILD_NOTES 4.2):

  - GraphQL: the tweet object intercepted from x.com's web API, recognizable
    by its `legacy` envelope. Formerly parsed in sync.go.
  - Legacy: the Go twitter-scraper library's serialized Tweet struct,
    recognizable by its `OrderedMedia` key (present in every document, even
    as null). Formerly parsed in import.go.

The deriver detects the format itself; callers never pass it in.
*/

// DerivedTweet is the format-neutral output of the deriver: tweet fields plus
// its media list, ready to be turned into database creates. It deliberately
// excludes bookmarked, rating, and synced_at, which are not derivable from
// raw_json and belong to the caller.
type DerivedTweet struct {
	ID           string
	Name         string
	ScreenName   string
	FullText     string
	CreatedAt    time.Time
	PermanentURL string
	RawJSON      string
	Media        []DerivedMedia
}

// DerivedMedia is one media attachment in its original order within the tweet.
type DerivedMedia struct {
	MediaID  string // Twitter's media ID; reusable across tweets, so not a key
	Position int
	Type     string // photo, video, animated_gif
	URL      string
}

// screenNameRegex is our last line of defense to extract the username if JSON
// structural parsing fails.
var screenNameRegex = regexp.MustCompile(`"screen_name"\s*:\s*"([^"]+)"`)

// DeriveTweet parses one raw tweet JSON document into a DerivedTweet,
// detecting the format on its own.
func DeriveTweet(raw json.RawMessage) (*DerivedTweet, error) {
	// The legacy format serializes the whole Go struct, so the OrderedMedia
	// key is present in every document (as null when the tweet has no media).
	// GraphQL documents never contain it.
	var probe struct {
		OrderedMedia json.RawMessage `json:"OrderedMedia"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, eris.Wrap(err, "failed to unmarshal tweet JSON")
	}
	if probe.OrderedMedia != nil {
		return deriveLegacyTweet(raw)
	}
	return deriveGraphQLTweet(raw)
}

// deriveGraphQLTweet converts a raw Twitter GraphQL JSON message into a
// structured DerivedTweet. This is a pure parsing function with no database
// side effects.
func deriveGraphQLTweet(res json.RawMessage) (*DerivedTweet, error) {
	// 1. Parse minimal fields required for indexing using a comprehensive struct matching common patterns.
	var tweet struct {
		Legacy struct {
			IDStr            string `json:"id_str"`
			FullText         string `json:"full_text"`
			CreatedAt        string `json:"created_at"`
			ExtendedEntities struct {
				Media []struct {
					IDStr         string `json:"id_str"`
					Type          string `json:"type"`
					MediaURLHttps string `json:"media_url_https"`
					VideoInfo     struct {
						Variants []struct {
							Bitrate     int    `json:"bitrate"`
							ContentType string `json:"content_type"`
							URL         string `json:"url"`
						} `json:"variants"`
					} `json:"video_info"`
				} `json:"media"`
			} `json:"extended_entities"`
		} `json:"legacy"`
		Core struct {
			UserResults struct {
				Result struct {
					Core struct {
						Name       string `json:"name"`
						ScreenName string `json:"screen_name"`
					} `json:"core"`
					Legacy struct {
						Name       string `json:"name"`
						ScreenName string `json:"screen_name"`
					} `json:"legacy"`
				} `json:"result"`
			} `json:"user_results"`
		} `json:"core"`
	}

	if err := json.Unmarshal(res, &tweet); err != nil {
		return nil, eris.Wrap(err, "failed to unmarshal tweet JSON")
	}

	tweetID := tweet.Legacy.IDStr
	if tweetID == "" {
		return nil, eris.New("missing tweet ID")
	}

	// 2. Extract Screen Name with fallback logic.
	// Try legacy path first, then core path.
	screenName := tweet.Core.UserResults.Result.Legacy.ScreenName
	if screenName == "" {
		screenName = tweet.Core.UserResults.Result.Core.ScreenName
	}
	name := tweet.Core.UserResults.Result.Legacy.Name
	if name == "" {
		name = tweet.Core.UserResults.Result.Core.Name
	}

	// Regex Fallback: If struct parsing failed (likely due to unexpected JSON structure or nesting),
	// try to find the screen_name directly from the raw string. This is critical for generating correct filenames.
	if screenName == "" {
		matches := screenNameRegex.FindStringSubmatch(string(res))
		if len(matches) > 1 {
			screenName = matches[1]
			PrintWarningF("Recovered ScreenName via regex for tweet %s: %s", tweetID, screenName)
		} else {
			PrintWarningF("Failed to extract ScreenName for tweet %s", tweetID)
		}
	}

	// 3. Parse creation time.
	createdAt, _ := time.Parse(time.RubyDate, tweet.Legacy.CreatedAt)
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	// 4. Construct the derived record.
	dt := &DerivedTweet{
		ID:           tweetID,
		FullText:     tweet.Legacy.FullText,
		Name:         name,
		ScreenName:   screenName,
		CreatedAt:    createdAt,
		PermanentURL: "https://x.com/" + screenName + "/status/" + tweetID,
		RawJSON:      string(res),
	}

	for i, m := range tweet.Legacy.ExtendedEntities.Media {
		mediaURL := m.MediaURLHttps

		// Handle Video/GIF variants to find best quality
		if m.Type == "video" || m.Type == "animated_gif" {
			bestBitrate := -1
			for _, v := range m.VideoInfo.Variants {
				if v.ContentType == "video/mp4" {
					if v.Bitrate > bestBitrate {
						bestBitrate = v.Bitrate
						mediaURL = v.URL
					}
				}
			}
		}

		dt.Media = append(dt.Media, DerivedMedia{
			MediaID:  m.IDStr,
			Position: i,
			Type:     m.Type,
			URL:      mediaURL,
		})
	}

	return dt, nil
}

// deriveLegacyTweet converts a serialized twitter-scraper Tweet into a
// DerivedTweet. The library stored ready-to-use values (including proper
// video URLs in OrderedMedia), so fields are copied as is.
func deriveLegacyTweet(raw json.RawMessage) (*DerivedTweet, error) {
	var lt struct {
		ID           string    `json:"ID"`
		Username     string    `json:"Username"`
		Name         string    `json:"Name"`
		Text         string    `json:"Text"`
		TimeParsed   time.Time `json:"TimeParsed"`
		PermanentURL string    `json:"PermanentURL"`
		OrderedMedia []struct {
			ID   string `json:"ID"`
			Type string `json:"Type"` // "photo", "video", "animated_gif"
			URL  string `json:"URL"`
		} `json:"OrderedMedia"`
	}
	if err := json.Unmarshal(raw, &lt); err != nil {
		return nil, eris.Wrap(err, "failed to unmarshal legacy json")
	}

	if lt.ID == "" {
		return nil, eris.New("missing tweet ID")
	}

	dt := &DerivedTweet{
		ID:           lt.ID,
		Name:         lt.Name,
		ScreenName:   lt.Username,
		FullText:     lt.Text,
		CreatedAt:    lt.TimeParsed,
		PermanentURL: lt.PermanentURL,
		RawJSON:      string(raw),
	}

	for i, m := range lt.OrderedMedia {
		dt.Media = append(dt.Media, DerivedMedia{
			MediaID:  m.ID,
			Position: i,
			Type:     m.Type,
			URL:      m.URL,
		})
	}

	return dt, nil
}

// mediaCreates builds the insert builders for a derived tweet's media list.
// Callers wrap them in CreateBulk within their own transaction.
func mediaCreates(tx *ent.Tx, d *DerivedTweet) []*ent.MediaCreate {
	builders := make([]*ent.MediaCreate, len(d.Media))
	for i, m := range d.Media {
		// Attachment key, not asset key: the same media ID can hang off more
		// than one tweet (three known cases in the real archive), and using it
		// alone would make the second tweet's insert collide and roll back.
		builders[i] = tx.Media.Create().
			SetID(d.ID + "-" + m.MediaID).
			SetTweetID(d.ID).
			SetMediaID(m.MediaID).
			SetPosition(m.Position).
			SetType(m.Type).
			SetURL(m.URL)
	}
	return builders
}
