package main

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/rotisserie/eris"

	"twitter-bookmarks-downloader/ent"
	"twitter-bookmarks-downloader/ent/media"
	"twitter-bookmarks-downloader/ent/tweet"
)

/*
Architecture and Logic Overview:

This file handles the synchronization of Twitter bookmarks from raw GraphQL responses
intercepted by the userscript.

Key Challenges with Twitter API:
1.  Deeply Nested Structure: The data we need (screen_name, full_text, media) is buried deep within
    `data.bookmark_timeline_v2.timeline.instructions...`.
2.  Polymorphic Results: The `result` field in `user_results` is not always a simple User object.
    It can be a wrapper containing `__typename`, or sometimes it's missing entire sections if the user is suspended.
3.  Redundant & Inconsistent Paths: Information like `screen_name` appears in multiple places (`legacy.screen_name`,
    `core.screen_name`), but not always consistently across different tweet types (original, retweet, quote).

Our Parsing Strategy (envelope unwrapping below, per-tweet parsing in
deriver.go):
1.  Comprehensive Struct Mapping: We define a single, large struct that maps to the most common
    successful response pattern observed (User object nested in `core.user_results.result`).
    We attempt to extract data from multiple known paths within this struct (e.g., trying both `Legacy` and `Core`
    sub-fields for screen_name). This lives in deriver.go, shared with migration and rebuild.
2.  Regex Fallback (The Safety Net): If struct parsing fails (e.g., due to a new wrapper layer or
    unexpected nulls), deriver.go falls back to a regex search on the raw JSON string for
    `"screen_name":"..."`. This ensures that even if the structural shape changes slightly, we can
    still likely identify the user and save the tweet (since preserving raw_json allows for future
    re-parsing).
3.  Raw Preservation: We always save the original JSON bytes into the database (`raw_json` column).
    This is crucial for data integrity and allows fixing parsing logic later without losing data.
*/

const DUPLICATE_THRESHOLD = 5 // Stop syncing if we encounter this many existing tweets in a row

// ProcessSyncRaw acts as the entry point. It unwraps the top-level GraphQL envelope
// to find the actual list of tweet entries.
func ProcessSyncRaw(fullJSON json.RawMessage) SyncResponse {
	// 1. Unwrap the outer GraphQL envelope to access the timeline
	var resp struct {
		Data struct {
			BookmarkTimeline struct {
				Timeline struct {
					Instructions []struct {
						Type    string            `json:"type"`
						Entries []json.RawMessage `json:"entries"`
					} `json:"instructions"`
				} `json:"timeline"`
			} `json:"bookmark_timeline_v2"`
		} `json:"data"`
	}

	if err := json.Unmarshal(fullJSON, &resp); err != nil {
		PrintError(err)
		return SyncResponse{Success: false, Message: "JSON unmarshal error"}
	}

	// 2. Iterate through instructions to find "TimelineAddEntries".
	// Twitter sometimes splits data across different instruction types, but "AddEntries" is the primary one for lists.
	var rawTweets []json.RawMessage
	var foundTypes []string

	for _, inst := range resp.Data.BookmarkTimeline.Timeline.Instructions {
		foundTypes = append(foundTypes, inst.Type)
		if inst.Type == "TimelineAddEntries" {
			for _, entryMsg := range inst.Entries {
				// Each entry might be a Tweet, a Promoted Tweet, or a Cursor.
				// We dig into content.itemContent.tweet_results.result to find the actual tweet data.
				var entry struct {
					Content struct {
						ItemContent struct {
							TweetResults struct {
								Result json.RawMessage `json:"result"`
							} `json:"tweet_results"`
						} `json:"itemContent"`
					} `json:"content"`
				}
				if err := json.Unmarshal(entryMsg, &entry); err == nil && entry.Content.ItemContent.TweetResults.Result != nil {
					rawTweets = append(rawTweets, entry.Content.ItemContent.TweetResults.Result)
				}
			}
		}
	}

	if len(rawTweets) == 0 {
		PrintWarningF("No tweets extracted. Found instruction types: %v", foundTypes)
	} else {
		PrintInfoF("Extracted %d tweets from raw GraphQL response", len(rawTweets))
	}

	// 3. Normalize the tweet objects.
	// Sometimes the result IS the tweet (typename="Tweet"), sometimes it WRAPS the tweet (typename="TweetWithVisibilityResults").
	var cleanTweets []json.RawMessage
	for _, rt := range rawTweets {
		var wrap struct {
			Typename string          `json:"__typename"`
			Tweet    json.RawMessage `json:"tweet"`
		}
		if err := json.Unmarshal(rt, &wrap); err == nil {
			if wrap.Typename == "Tweet" {
				cleanTweets = append(cleanTweets, rt)
			} else if wrap.Tweet != nil {
				cleanTweets = append(cleanTweets, wrap.Tweet)
			}
		}
	}

	return processRawTweetResults(cleanTweets)
}

// createBookmarkedTweet writes a freshly synced bookmark and its media.
// Seeding rule (REBUILD_NOTES 4.3): a bookmark is itself a cheap positive
// judgment, so a new tweet arrives as bookmarked=yes with the lowest positive
// rating. Tweets that already exist are never updated by a sync: incremental
// syncs may not downgrade bookmarked, and rating belongs to humans.
func createBookmarkedTweet(ctx context.Context, d *DerivedTweet) error {
	return WithTx(ctx, func(tx *ent.Tx) error {
		err := tx.Tweet.Create().
			SetID(d.ID).
			SetName(d.Name).
			SetScreenName(d.ScreenName).
			SetFullText(d.FullText).
			SetCreatedAt(d.CreatedAt).
			SetPermanentURL(d.PermanentURL).
			SetRawJSON(d.RawJSON).
			SetSyncedAt(time.Now()).
			SetBookmarked(tweet.BookmarkedYes).
			SetRating(1).
			Exec(ctx)
		if err != nil {
			return err
		}
		if len(d.Media) == 0 {
			return nil
		}
		return tx.Media.CreateBulk(mediaCreates(tx, d)...).Exec(ctx)
	})
}

// processRawTweetResults iterates over individual tweet JSON objects, extracts metadata,
// checks for duplicates, and saves them to the database.
func processRawTweetResults(results []json.RawMessage) SyncResponse {
	ctx := context.Background()
	savedCount := 0
	duplicateStreak := 0
	duplicateLimitReached := false

	type belowCutoffEntry struct {
		URL        string
		MediaFiles []string
	}
	// Tweets saved after the duplicate streak had already tripped the cutoff.
	//
	// The cutoff rests on the assumption that a run of DUPLICATE_THRESHOLD
	// known tweets means everything below is known too, and a save past that
	// point is that assumption failing. What follows from it depends on the
	// caller: an ordinary sync halts on the cutoff, so whatever sits below
	// these tweets was never fetched, whereas a forced full scroll keeps going
	// and the same entries only mark where the assumption would have cut the
	// run short.
	//
	// Whether the flag was already set is the entire distinction. A normal
	// incremental page saves its handful of new bookmarks at the top and only
	// then walks into the old ones, which trips the flag before the page ends
	// and says nothing about anything being missed.
	var belowCutoff []belowCutoffEntry

	for _, res := range results {
		d, err := DeriveTweet(res)
		if err != nil {
			continue
		}

		// Check for duplicates in the database.
		exists, err := DB.Tweet.Query().Where(tweet.ID(d.ID)).Exist(ctx)
		if err != nil {
			// Skip without touching duplicateStreak: an unanswered existence
			// check says nothing about whether the tweet is a duplicate, so it
			// neither extends a run of known tweets nor breaks one.
			PrintError(eris.Wrapf(err, "Failed to check existence of tweet %s", d.ID))
			continue
		}
		if exists {
			duplicateStreak++
			if duplicateStreak >= DUPLICATE_THRESHOLD {
				duplicateLimitReached = true
			}
			continue
		}
		duplicateStreak = 0

		if err := createBookmarkedTweet(ctx, d); err == nil {
			savedCount++
			if duplicateLimitReached {
				// Filenames the worker is about to derive, so the report names
				// the files on disk rather than URLs nobody can grep for.
				var mediaFilenames []string
				mediaCount := len(d.Media)
				for i, m := range d.Media {
					if parsedURL, err := url.Parse(m.URL); err == nil {
						mediaFilenames = append(mediaFilenames, buildFilename(d.ScreenName, d.ID, d.CreatedAt, i, mediaCount, parsedURL))
					}
				}
				belowCutoff = append(belowCutoff, belowCutoffEntry{
					URL:        d.PermanentURL,
					MediaFiles: mediaFilenames,
				})
			}
		}
	}

	PrintInfoF("Batch processing complete: %d new tweets saved. (Duplicate limit reached: %v)", savedCount, duplicateLimitReached)

	if len(belowCutoff) > 0 {
		PrintWarningF("%d tweets were saved below the duplicate cutoff; unless this was a forced full scroll, the run stopped there and left the rest behind:", len(belowCutoff))
		for _, info := range belowCutoff {
			PrintWarningF("  [Below Cutoff] URL: %s", info.URL)
			for _, mf := range info.MediaFiles {
				PrintWarningF("                 Media: %s", mf)
			}
		}
	}

	return SyncResponse{
		Success:               true,
		Message:               "Raw sync complete",
		DuplicateLimitReached: duplicateLimitReached,
		SavedCount:            savedCount,
	}
}

// ReparseTweets updates existing records using their RawJSON with latest parsing logic.
func ReparseTweets(ids []string) {
	ctx := context.Background()
	PrintInfoF("Reparsing %d tweets...", len(ids))
	successCount := 0

	for _, id := range ids {
		t, err := DB.Tweet.Get(ctx, id)
		if err != nil {
			PrintError(eris.Wrapf(err, "Tweet %s not found in DB", id))
			continue
		}

		d, err := DeriveTweet(json.RawMessage(t.RawJSON))
		if err != nil {
			PrintError(eris.Wrapf(err, "Failed to parse RawJSON for tweet %s", id))
			continue
		}

		// Use a transaction to update tweet and refresh media records
		err = WithTx(ctx, func(tx *ent.Tx) error {
			// 1. Update derived tweet metadata. bookmarked, rating, and
			// synced_at stay untouched: none of them derive from raw_json.
			err := tx.Tweet.UpdateOneID(id).
				SetName(d.Name).
				SetScreenName(d.ScreenName).
				SetFullText(d.FullText).
				SetCreatedAt(d.CreatedAt).
				SetPermanentURL(d.PermanentURL).
				Exec(ctx)
			if err != nil {
				return err
			}

			// 2. Clear and recreate media records (only for this tweet)
			// We delete the old ones and insert new ones to reflect any parsing changes (like high-res URLs).
			if _, err := tx.Media.Delete().Where(media.TweetID(id)).Exec(ctx); err != nil {
				return err
			}
			if len(d.Media) > 0 {
				return tx.Media.CreateBulk(mediaCreates(tx, d)...).Exec(ctx)
			}
			return nil
		})

		if err != nil {
			PrintError(eris.Wrapf(err, "Transaction failed for tweet %s", id))
		} else {
			successCount++
		}
	}

	PrintInfoF("Reparse complete. Updated: %d/%d", successCount, len(ids))
}

func ReparseMediaTweets() {
	PrintInfo("Scanning database for tweets with videos or GIFs to re-parse...")

	// Find IDs where raw_json contains video or animated_gif tags
	ids, err := DB.Tweet.Query().
		Where(tweet.Or(
			tweet.RawJSONContains(`"type":"video"`),
			tweet.RawJSONContains(`"type":"animated_gif"`),
		)).
		IDs(context.Background())
	if err != nil {
		PrintError(eris.Wrap(err, "Failed to query media tweets for re-parsing"))
		return
	}

	if len(ids) == 0 {
		PrintInfo("No media tweets found needing re-parse.")
		return
	}

	PrintInfoF("Found %d media tweets. Starting re-parse...", len(ids))
	ReparseTweets(ids)
}
