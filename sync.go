package main

import (
	"encoding/json"
	"net/url"
	"regexp"
	"time"

	"github.com/rotisserie/eris"
	"gorm.io/gorm"
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

Our Parsing Strategy (in processRawTweetResults):
1.  Comprehensive Struct Mapping: We define a single, large struct that maps to the most common
    successful response pattern observed (User object nested in `core.user_results.result`).
    We attempt to extract data from multiple known paths within this struct (e.g., trying both `Legacy` and `Core`
    sub-fields for screen_name).
2.  Regex Fallback (The Safety Net): If struct parsing fails (e.g., due to a new wrapper layer or
    unexpected nulls), we fall back to a regex search on the raw JSON string for `"screen_name":"..."`.
    This ensures that even if the structural shape changes slightly, we can still likely identify the user
    and save the tweet (since preserving the RawJSON allows for future re-parsing).
3.  Raw Preservation: We always save the original JSON bytes into the database (`RawJSON` column).
    This is crucial for data integrity and allows fixing parsing logic later without losing data.
*/

// rawTweetEntry pairs a normalized tweet object with the bookmark timeline
// entry's sortIndex — the ordering value x.com itself uses for bookmarks (a
// *bookmark's* position, not the tweet's publish time). Everything except the
// sidecar in bookmark_meta.go ignores it.
type rawTweetEntry struct {
	Result    json.RawMessage
	SortIndex string
	EntryID   string
}

const DUPLICATE_THRESHOLD = 5 // Stop syncing if we encounter this many existing tweets in a row

// screenNameRegex is our last line of defense to extract the username if JSON structural parsing fails.
var screenNameRegex = regexp.MustCompile(`"screen_name"\s*:\s*"([^"]+)"`)

// ProcessSyncRaw acts as the entry point. It unwraps the top-level GraphQL envelope
// to find the actual list of tweet entries.
func ProcessSyncRaw(fullJSON json.RawMessage) SyncResponse {
	page, err := parseTimelineEntries(fullJSON)
	if err != nil {
		PrintError(err)
		return SyncResponse{Success: false, Message: "JSON unmarshal error"}
	}

	if len(page.Tweets) == 0 {
		if page.Cursors > 0 {
			// A page of cursors and nothing else is x.com's answer once a scroll
			// walks past the last bookmark. Informational: the client ends the
			// run on empty_page, and a scroll that kept asking would get this
			// same page back forever.
			PrintInfoF("End of timeline (%d entries, %d cursors, no tweets)",
				page.Entries, page.Cursors)
		} else {
			PrintWarningF("No tweets extracted. Found instruction types: %v", page.Types)
		}
	} else {
		PrintInfoF("Extracted %d tweets from raw GraphQL response", len(page.Tweets))
	}

	cleanTweets := normalizeTweetEntries(page.Tweets)

	// Sidecar first, and ahead of the duplicate cutoff inside
	// processRawTweetResults: an all-duplicate batch (the periodic auto reload)
	// still refreshes the top of the timeline.
	recordBookmarkMeta(cleanTweets)

	results := make([]json.RawMessage, 0, len(cleanTweets))
	for _, e := range cleanTweets {
		results = append(results, e.Result)
	}
	resp := processRawTweetResults(results)
	// End of timeline: entries came back, none of them a tweet, at least one of
	// them a cursor. Requiring the cursor keeps a bare payload — which is what a
	// rate limit looks like — on the client's failure path instead.
	resp.EmptyPage = page.endOfTimeline()
	return resp
}

// timelinePage is one parsed sync payload.
type timelinePage struct {
	Tweets  []rawTweetEntry // tweet objects, each paired with its entry sortIndex
	Types   []string        // instruction types seen (diagnostics)
	Entries int             // raw timeline entries seen, cursors included
	Cursors int             // of those, the cursor entries
}

// endOfTimeline reports x.com's answer once a scroll walks past the last
// bookmark: timeline entries came back, none of them a tweet, at least one a
// cursor. A bare payload (no entries, or entries that are not cursors) is what
// a rate limit looks like, so it stays on the failure path instead.
func (p timelinePage) endOfTimeline() bool {
	return len(p.Tweets) == 0 && p.Cursors > 0
}

// parseTimelineEntries walks the BookmarkTimeline instructions and returns every
// tweet object still paired with its entry-level sortIndex (see bookmark_meta.go),
// alongside what else the payload carried.
//
// Pure: no IO, no DB — the envelope shape is the only thing it knows.
func parseTimelineEntries(fullJSON json.RawMessage) (timelinePage, error) {
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
		return timelinePage{}, err
	}

	// Twitter sometimes splits data across different instruction types, but
	// "AddEntries" is the primary one for lists.
	page := timelinePage{}

	for _, inst := range resp.Data.BookmarkTimeline.Timeline.Instructions {
		page.Types = append(page.Types, inst.Type)
		if inst.Type != "TimelineAddEntries" {
			continue
		}
		for _, entryMsg := range inst.Entries {
			page.Entries++
			// Each entry might be a Tweet, a Promoted Tweet, or a Cursor.
			// We dig into content.itemContent.tweet_results.result to find the
			// actual tweet data; sortIndex rides alongside it and is the
			// timeline's own ordering value, captured here because nothing
			// further down can see it again.
			var entry struct {
				SortIndex string `json:"sortIndex"`
				EntryID   string `json:"entryId"`
				Content   struct {
					CursorType  string `json:"cursorType"`
					ItemContent struct {
						TweetResults struct {
							Result json.RawMessage `json:"result"`
						} `json:"tweet_results"`
					} `json:"itemContent"`
				} `json:"content"`
			}
			if err := json.Unmarshal(entryMsg, &entry); err != nil {
				continue
			}
			if entry.Content.CursorType != "" {
				page.Cursors++
			}
			if entry.Content.ItemContent.TweetResults.Result != nil {
				page.Tweets = append(page.Tweets, rawTweetEntry{
					Result:    entry.Content.ItemContent.TweetResults.Result,
					SortIndex: entry.SortIndex,
					EntryID:   entry.EntryID,
				})
			}
		}
	}
	return page, nil
}

// normalizeTweetEntries unwraps the tweet objects: sometimes the result IS the
// tweet (typename="Tweet"), sometimes it WRAPS it (typename=
// "TweetWithVisibilityResults"). Cursor and promoted entries fall out.
func normalizeTweetEntries(rawTweets []rawTweetEntry) []rawTweetEntry {
	var cleanTweets []rawTweetEntry
	for _, rt := range rawTweets {
		var wrap struct {
			Typename string          `json:"__typename"`
			Tweet    json.RawMessage `json:"tweet"`
		}
		if err := json.Unmarshal(rt.Result, &wrap); err != nil {
			continue
		}
		if wrap.Typename == "Tweet" {
			cleanTweets = append(cleanTweets, rt)
		} else if wrap.Tweet != nil {
			rt.Result = wrap.Tweet
			cleanTweets = append(cleanTweets, rt)
		}
	}
	return cleanTweets
}

// parseTweet converts a raw Twitter GraphQL JSON message into a structured TweetModel.
// This is a pure parsing function with no database side effects.
func parseTweet(res json.RawMessage) (*TweetModel, error) {
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

	// 4. Construct the TweetModel and MediaModels
	// Note: We store the raw JSON payload to allow for future re-processing or data recovery.
	tm := &TweetModel{
		ID:           tweetID,
		FullText:     fullestText(res, tweet.Legacy.FullText),
		Name:         name,
		ScreenName:   screenName,
		CreatedAt:    createdAt,
		PermanentURL: "https://x.com/" + screenName + "/status/" + tweetID,
		RawJSON:      string(res),
		SyncedAt:     time.Now(),
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

		tm.Media = append(tm.Media, MediaModel{
			ID:      m.IDStr,
			TweetID: tweetID,
			Index:   i,
			Type:    m.Type,
			URL:     mediaURL,
		})
	}

	return tm, nil
}

// processRawTweetResults iterates over individual tweet JSON objects, extracts metadata,
// checks for duplicates, and saves them to the database.
func processRawTweetResults(results []json.RawMessage) SyncResponse {
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
		tm, err := parseTweet(res)
		if err != nil {
			continue
		}

		tweetID := tm.ID

		// 3. Check for duplicates in the database.
		var exists int64
		DB.Model(&TweetModel{}).Where("id = ?", tweetID).Count(&exists)
		if exists > 0 {
			duplicateStreak++
			if duplicateStreak >= DUPLICATE_THRESHOLD {
				duplicateLimitReached = true
			}
			continue
		}
		duplicateStreak = 0

		if err := DB.Create(tm).Error; err == nil {
			savedCount++
			if duplicateLimitReached {
				// Filenames the worker is about to derive, so the report names
				// the files on disk rather than URLs nobody can grep for.
				var mediaFilenames []string
				mediaCount := len(tm.Media)
				for i, m := range tm.Media {
					if parsedURL, err := url.Parse(m.URL); err == nil {
						mediaFilenames = append(mediaFilenames, buildFilename(tm, i, mediaCount, parsedURL))
					}
				}
				belowCutoff = append(belowCutoff, belowCutoffEntry{
					URL:        tm.PermanentURL,
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
	PrintInfoF("Reparsing %d tweets...", len(ids))
	successCount := 0

	for _, id := range ids {
		var tweet TweetModel
		if err := DB.First(&tweet, "id = ?", id).Error; err != nil {
			PrintError(eris.Wrapf(err, "Tweet %s not found in DB", id))
			continue
		}

		tm, err := parseTweet(json.RawMessage(tweet.RawJSON))
		if err != nil {
			PrintError(eris.Wrapf(err, "Failed to parse RawJSON for tweet %s", id))
			continue
		}

		// Use a transaction to update tweet and refresh media records
		err = DB.Transaction(func(tx *gorm.DB) error {
			// 1. Update Tweet metadata (Upsert)
			if err := tx.Save(tm).Error; err != nil {
				return err
			}

			// 2. Clear and recreate media records (only for this tweet)
			// We delete the old ones and insert new ones to reflect any parsing changes (like high-res URLs).
			if err := tx.Unscoped().Delete(&MediaModel{}, "tweet_id = ?", id).Error; err != nil {
				return err
			}
			if len(tm.Media) > 0 {
				if err := tx.Create(&tm.Media).Error; err != nil {
					return err
				}
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

	var ids []string
	// Find IDs where raw_json contains video or animated_gif tags
	err := DB.Model(&TweetModel{}).
		Where("raw_json LIKE ?", "%\"type\":\"video\"%").
		Or("raw_json LIKE ?", "%\"type\":\"animated_gif\"%").
		Pluck("id", &ids).Error

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
