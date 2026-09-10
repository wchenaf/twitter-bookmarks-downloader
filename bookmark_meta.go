package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Bookmark timeline metadata sidecar.
//
// The downloaded media filenames carry the tweet's *publish* time, which is not
// the order x.com shows bookmarks in: the bookmarks timeline is ordered by each
// entry's `sortIndex` (the bookmark's own position — newer bookmark, higher
// value). That value lives only in the GraphQL envelope, next to the tweet it
// wraps, so it is dropped the moment this process stops looking at the raw
// response — it is not in the tweet object that ends up in `tweets.raw_json`.
//
// This file preserves it, plus the fullest available body text (long posts keep
// only a truncated `legacy.full_text`; the complete text is in `note_tweet`).
// Consumers read the sidecar to sort bookmarks the way x.com does and to show
// the post text on an album card.
//
// ⛔ The filename and field names are a contract with the consumer
// (tg-hgreport-v2 `src/hgreport/media/xmeta.py`) — change both sides together.
const bookmarkMetaFilename = "bookmark_meta.jsonl"

// Captured text is capped so a pathological post cannot bloat the sidecar; the
// consumer truncates further for display.
const bookmarkMetaTextMaxRunes = 500

// BookmarkMeta is one row of the sidecar (one line of JSONL per tweet).
type BookmarkMeta struct {
	TweetID    string `json:"tweet_id"`
	SortIndex  string `json:"sort_index"`
	ScreenName string `json:"screen_name"`
	Text       string `json:"text"`
	CreatedAt  string `json:"created_at"`
	CapturedAt string `json:"captured_at"`
}

var (
	metaMu      sync.Mutex
	metaStore   map[string]BookmarkMeta
	metaLoaded  bool
)

func metaPath() string {
	return filepath.Join(config.MediaDir, bookmarkMetaFilename)
}

// isDigits returns the trimmed string and whether it is a non-empty run of
// ASCII digits.
func isDigits(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false
		}
	}
	return s, true
}

// compareSortIndex orders two sortIndex values the way x.com does.
//
// They are variable-length decimal BigInts rendered as strings, so a plain
// string compare is wrong ("900" > "1000"). Equal length falls back to
// lexicographic order, which is numeric order for equal-length digit strings.
// Non-numeric values order lowest (they are unusable either way).
func compareSortIndex(a, b string) int {
	as, aok := isDigits(a)
	bs, bok := isDigits(b)
	switch {
	case aok && bok:
		if len(as) != len(bs) {
			if len(as) < len(bs) {
				return -1
			}
			return 1
		}
		return strings.Compare(as, bs)
	case aok:
		return 1
	case bok:
		return -1
	default:
		return 0
	}
}

// noteTweetText returns the long-form body of a "note tweet" (x.com's long
// posts), or "" when the tweet has none.
//
// This is a second small unmarshal of the same payload rather than a field on
// parseTweet's struct: the shape is defined once and shared by both readers.
func noteTweetText(res json.RawMessage) string {
	var w struct {
		NoteTweet struct {
			NoteTweetResults struct {
				Result struct {
					Text string `json:"text"`
				} `json:"result"`
			} `json:"note_tweet_results"`
		} `json:"note_tweet"`
	}
	if err := json.Unmarshal(res, &w); err != nil {
		return ""
	}
	return w.NoteTweet.NoteTweetResults.Result.Text
}

// fullestText prefers the note tweet body and falls back to legacy.full_text,
// which x.com truncates for long posts.
func fullestText(res json.RawMessage, legacyFullText string) string {
	if t := noteTweetText(res); t != "" {
		return t
	}
	return legacyFullText
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// parseTweetMeta extracts one sidecar row from a raw tweet object plus the
// sortIndex that came with it. Pure: no IO, no DB.
//
// created_at is left empty when unparseable — deliberately NOT time.Now(), which
// would look like a real capture time downstream.
func parseTweetMeta(res json.RawMessage, sortIndex string) (BookmarkMeta, bool) {
	var t struct {
		Legacy struct {
			IDStr     string `json:"id_str"`
			FullText  string `json:"full_text"`
			CreatedAt string `json:"created_at"`
		} `json:"legacy"`
		Core struct {
			UserResults struct {
				Result struct {
					Core struct {
						ScreenName string `json:"screen_name"`
					} `json:"core"`
					Legacy struct {
						ScreenName string `json:"screen_name"`
					} `json:"legacy"`
				} `json:"result"`
			} `json:"user_results"`
		} `json:"core"`
	}
	if err := json.Unmarshal(res, &t); err != nil {
		return BookmarkMeta{}, false
	}
	tweetID := strings.TrimSpace(t.Legacy.IDStr)
	if tweetID == "" {
		return BookmarkMeta{}, false
	}

	screenName := t.Core.UserResults.Result.Legacy.ScreenName
	if screenName == "" {
		screenName = t.Core.UserResults.Result.Core.ScreenName
	}
	if screenName == "" {
		if m := screenNameRegex.FindStringSubmatch(string(res)); len(m) > 1 {
			screenName = m[1]
		}
	}

	createdAt := ""
	if ts, err := time.Parse(time.RubyDate, t.Legacy.CreatedAt); err == nil {
		createdAt = ts.UTC().Format(time.RFC3339)
	}

	return BookmarkMeta{
		TweetID:    tweetID,
		SortIndex:  strings.TrimSpace(sortIndex),
		ScreenName: screenName,
		Text:       truncateRunes(fullestText(res, t.Legacy.FullText), bookmarkMetaTextMaxRunes),
		CreatedAt:  createdAt,
	}, true
}

// mergeBookmarkMeta combines a stored row with a freshly seen one, field by
// field: never overwrite a value with an empty one, keep the higher sortIndex,
// keep the longer text.
func mergeBookmarkMeta(old, next BookmarkMeta) BookmarkMeta {
	out := next
	if compareSortIndex(old.SortIndex, next.SortIndex) > 0 {
		out.SortIndex = old.SortIndex
	}
	if len(old.Text) > len(out.Text) {
		out.Text = old.Text
	}
	if out.ScreenName == "" {
		out.ScreenName = old.ScreenName
	}
	if out.CreatedAt == "" {
		out.CreatedAt = old.CreatedAt
	}
	if out.CapturedAt == "" {
		out.CapturedAt = old.CapturedAt
	}
	return out
}

// loadMetaLocked restores the in-memory map from the sidecar. Torn or malformed
// lines are skipped: the file is append-only, so a killed process can leave a
// partial last line, and everything before it is still good.
func loadMetaLocked() {
	metaStore = map[string]BookmarkMeta{}
	data, err := os.ReadFile(metaPath())
	if err != nil {
		return // no file yet — first run
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m BookmarkMeta
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.TweetID == "" {
			continue
		}
		metaStore[m.TweetID] = mergeBookmarkMeta(metaStore[m.TweetID], m)
	}
}

// writeAllLocked rewrites the whole sidecar from the in-memory map. Only used to
// recover after the file was deleted underneath us (the map still has history).
func writeAllLocked() error {
	lines := make([][]byte, 0, len(metaStore))
	for _, m := range metaStore {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		lines = append(lines, b)
	}
	if err := os.MkdirAll(config.MediaDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(metaPath(), joinLines(lines), 0o644)
}

func joinLines(lines [][]byte) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.Write(l)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// appendMetaLines appends the given rows in one write. Serialized by the caller's
// lock; a single Write keeps concurrent POSTs from interleaving half-lines.
func appendMetaLines(rows []BookmarkMeta) error {
	lines := make([][]byte, 0, len(rows))
	for _, m := range rows {
		// Marshal rather than build the line by hand: post text carries
		// newlines, quotes and emoji.
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		lines = append(lines, b)
	}
	if err := os.MkdirAll(config.MediaDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(metaPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(joinLines(lines))
	return err
}

// recordBookmarkMeta persists the metadata of one sync batch.
//
// Called on every GraphQL sync response, including the periodic reloads that
// save nothing: dedup happens later in processRawTweetResults, so an all-
// duplicate batch still carries the current top of the timeline — which is
// exactly the part whose order and text we want to keep fresh.
//
// ⛔ Failure here must never disturb the download path: everything is wrapped,
// reported as a warning, and otherwise ignored. The DB is the source of truth
// for tweets; this file is derived and self-heals (deleted → rewritten from the
// in-memory map).
func recordBookmarkMeta(entries []rawTweetEntry) {
	defer func() {
		if r := recover(); r != nil {
			PrintWarningF("bookmark meta: recovered panic: %v", r)
		}
	}()
	if len(entries) == 0 {
		return
	}

	metaMu.Lock()
	defer metaMu.Unlock()

	if !metaLoaded {
		loadMetaLocked()
		metaLoaded = true
	} else if _, err := os.Stat(metaPath()); os.IsNotExist(err) {
		if err := writeAllLocked(); err != nil {
			PrintWarningF("bookmark meta: redump failed: %v", err)
		}
	}

	nowISO := time.Now().UTC().Format(time.RFC3339)
	var pending []BookmarkMeta
	updates := make(map[string]BookmarkMeta, len(entries))
	for _, e := range entries {
		m, ok := parseTweetMeta(e.Result, e.SortIndex)
		if !ok {
			continue
		}
		m.CapturedAt = nowISO
		old, exists := metaStore[m.TweetID]
		merged := mergeBookmarkMeta(old, m)
		if exists && merged == old {
			continue
		}
		updates[m.TweetID] = merged
		pending = append(pending, merged)
	}
	if len(pending) == 0 {
		return
	}
	if err := appendMetaLines(pending); err != nil {
		// Keep the in-memory map unchanged so the next batch retries.
		PrintWarningF("bookmark meta: append failed: %v", err)
		return
	}
	for id, m := range updates {
		metaStore[id] = m
	}
	PrintInfoF("bookmark meta: +%d rows (total %d)", len(pending), len(metaStore))
}
