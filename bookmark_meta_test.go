package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// tweetJSON builds a minimal tweet object shaped like the GraphQL payload.
// created_at is rendered with RubyDate from a real time so the weekday and the
// day-of-month always agree (time.Parse rejects inconsistent pairs).
func tweetJSON(id string) string {
	created := time.Date(2026, 8, 31, 12, 19, 43, 0, time.UTC)
	return fmt.Sprintf(`{
  "legacy": {"id_str": %q, "full_text": "截断的短正文", "created_at": %q},
  "note_tweet": {"note_tweet_results": {"result": {"text": "note_tweet 里的完整长正文"}}},
  "core": {"user_results": {"result": {"legacy": {"screen_name": "alice"}}}}
}`, id, created.Format(time.RubyDate))
}

// tweetJSONNoNote is the same shape without a note_tweet wrapper.
func tweetJSONNoNote(id string) string {
	created := time.Date(2026, 8, 31, 12, 19, 43, 0, time.UTC)
	return fmt.Sprintf(`{
  "legacy": {"id_str": %q, "full_text": "只有 legacy 的正文", "created_at": %q},
  "core": {"user_results": {"result": {"legacy": {"screen_name": "bob"}}}}
}`, id, created.Format(time.RubyDate))
}

// resetMetaForTest isolates the package-level sidecar state (config.MediaDir,
// the in-memory map) from the developer's real media directory.
func resetMetaForTest(t *testing.T) {
	t.Helper()
	old := config.MediaDir
	config.MediaDir = t.TempDir()
	metaMu.Lock()
	metaStore = nil
	metaLoaded = false
	metaMu.Unlock()
	t.Cleanup(func() {
		config.MediaDir = old
		metaMu.Lock()
		metaStore = nil
		metaLoaded = false
		metaMu.Unlock()
	})
}

func metaLineCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(metaPath())
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func TestCompareSortIndex(t *testing.T) {
	// Variable-length BigInt strings: length decides first, so "900" < "1000".
	assert.Equal(t, -1, compareSortIndex("900", "1000"))
	assert.Equal(t, 1, compareSortIndex("1000", "900"))
	// Equal length falls back to lexicographic, which is numeric here.
	assert.Equal(t, -1, compareSortIndex("123", "124"))
	assert.Equal(t, 0, compareSortIndex("123", "123"))
	// Non-numeric sorts lowest (unusable either way).
	assert.Equal(t, -1, compareSortIndex("", "123"))
	assert.Equal(t, 1, compareSortIndex("123", "abc"))
	assert.Equal(t, 0, compareSortIndex("  ", ""))
}

func TestParseTweetMetaPrefersNoteTweetBody(t *testing.T) {
	meta, ok := parseTweetMeta(json.RawMessage(tweetJSON("111")), " 12345 ")
	assert.True(t, ok)
	assert.Equal(t, "111", meta.TweetID)
	assert.Equal(t, "12345", meta.SortIndex, "sortIndex is trimmed")
	assert.Equal(t, "note_tweet 里的完整长正文", meta.Text)
	assert.Equal(t, "alice", meta.ScreenName)
	assert.Equal(t, "2026-08-31T12:19:43Z", meta.CreatedAt)
}

func TestParseTweetMetaFallsBackToLegacyText(t *testing.T) {
	meta, ok := parseTweetMeta(json.RawMessage(tweetJSONNoNote("222")), "")
	assert.True(t, ok)
	assert.Equal(t, "只有 legacy 的正文", meta.Text)
	assert.Equal(t, "bob", meta.ScreenName)
	assert.Equal(t, "", meta.SortIndex)
}

func TestParseTweetMetaRejectsMissingID(t *testing.T) {
	_, ok := parseTweetMeta(json.RawMessage(`{"legacy":{"full_text":"no id"}}`), "1")
	assert.False(t, ok)
}

func TestParseTweetMetaUnparseableCreatedAtIsEmpty(t *testing.T) {
	// Must NOT fall back to time.Now(): a made-up capture time looks real
	// downstream, and the consumer checks bookmark time against publish time.
	raw := `{"legacy":{"id_str":"333","created_at":"not a date"}}`
	meta, ok := parseTweetMeta(json.RawMessage(raw), "1")
	assert.True(t, ok)
	assert.Equal(t, "", meta.CreatedAt)
}

func TestParseTweetMetaTruncatesLongText(t *testing.T) {
	long := strings.Repeat("字", bookmarkMetaTextMaxRunes+50)
	raw := fmt.Sprintf(`{"legacy":{"id_str":"444","full_text":%q}}`, long)
	meta, _ := parseTweetMeta(json.RawMessage(raw), "1")
	assert.Equal(t, bookmarkMetaTextMaxRunes, len([]rune(meta.Text)))
}

func TestParseTweetUsesNoteTweetBody(t *testing.T) {
	tm, err := parseTweet(json.RawMessage(tweetJSON("2094399734660382995")))
	assert.NoError(t, err)
	assert.Equal(t, "note_tweet 里的完整长正文", tm.FullText)
}

func TestRecordBookmarkMetaDedupesAndKeepsBiggerSortIndex(t *testing.T) {
	resetMetaForTest(t)
	entry := rawTweetEntry{Result: json.RawMessage(tweetJSON("111")), SortIndex: "100"}
	recordBookmarkMeta([]rawTweetEntry{entry})
	assert.Equal(t, 1, metaLineCount(t))

	// Same tweet, same sortIndex → nothing appended (every 30-minute reload
	// re-sends the same top of the timeline).
	recordBookmarkMeta([]rawTweetEntry{entry})
	assert.Equal(t, 1, metaLineCount(t))

	// Re-bookmarked: higher sortIndex → appended.
	recordBookmarkMeta([]rawTweetEntry{
		{Result: json.RawMessage(tweetJSON("111")), SortIndex: "200"},
	})
	assert.Equal(t, 2, metaLineCount(t))

	// Reloading keeps the higher value.
	metaMu.Lock()
	metaLoaded = false
	metaStore = nil
	loadMetaLocked()
	got := metaStore["111"]
	metaMu.Unlock()
	assert.Equal(t, "200", got.SortIndex)

	// A blank sortIndex on a later batch must not wipe the stored one.
	recordBookmarkMeta([]rawTweetEntry{
		{Result: json.RawMessage(tweetJSON("111")), SortIndex: ""},
	})
	metaMu.Lock()
	metaLoaded = false
	metaStore = nil
	loadMetaLocked()
	got = metaStore["111"]
	metaMu.Unlock()
	assert.Equal(t, "200", got.SortIndex)
}

func TestRecordBookmarkMetaRedumpsAfterFileDeleted(t *testing.T) {
	resetMetaForTest(t)
	recordBookmarkMeta([]rawTweetEntry{
		{Result: json.RawMessage(tweetJSON("111")), SortIndex: "100"},
	})
	assert.NoError(t, os.Remove(metaPath()))

	// The map still holds history; losing the file must not lose the rows.
	recordBookmarkMeta([]rawTweetEntry{
		{Result: json.RawMessage(tweetJSON("222")), SortIndex: "300"},
	})
	data, err := os.ReadFile(metaPath())
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"111"`)
	assert.Contains(t, string(data), `"222"`)
}

func TestLoadMetaToleratesTornLine(t *testing.T) {
	resetMetaForTest(t)
	assert.NoError(t, os.MkdirAll(config.MediaDir, 0o755))
	content := "{\"tweet_id\":\"1\",\"sort_index\":\"100\"}\n{\"tweet_id\":\"2\",\"sort_i"
	assert.NoError(t, os.WriteFile(metaPath(), []byte(content), 0o644))

	metaMu.Lock()
	loadMetaLocked()
	_, hasOne := metaStore["1"]
	_, hasTwo := metaStore["2"]
	metaMu.Unlock()
	assert.True(t, hasOne, "complete line survives a torn tail")
	assert.False(t, hasTwo, "torn line is skipped")
}

func TestRecordBookmarkMetaEmptyBatchIsNoop(t *testing.T) {
	resetMetaForTest(t)
	recordBookmarkMeta(nil)
	assert.Equal(t, 0, metaLineCount(t))
	assert.False(t, metaLoaded)
}

// ── entry 级 sortIndex 捕获（ProcessSyncRaw 的纯解析部分）────────────

func TestParseTimelineEntriesKeepsSortIndexPaired(t *testing.T) {
	envelope := `{"data":{"bookmark_timeline_v2":{"timeline":{"instructions":[
{"type":"TimelineCursor","entries":[]},
{"type":"TimelineAddEntries","entries":[
 {"entryId":"tweet-1","sortIndex":"900","content":{"itemContent":{"tweet_results":{"result":{"__typename":"Tweet","legacy":{"id_str":"1"}}}}}},
 {"entryId":"cursor-bottom-9","sortIndex":"1000","content":{"itemContent":{"cursorType":"Bottom"}}}
]}]}}}}`
	raw, types, err := parseTimelineEntries(json.RawMessage(envelope))
	assert.NoError(t, err)
	assert.Equal(t, []string{"TimelineCursor", "TimelineAddEntries"}, types)
	assert.Len(t, raw, 1, "cursor entry carries no tweet_results.result")
	assert.Equal(t, "900", raw[0].SortIndex)
	assert.Equal(t, "tweet-1", raw[0].EntryID)
}

func TestParseTimelineEntriesRejectsBadJSON(t *testing.T) {
	_, _, err := parseTimelineEntries(json.RawMessage("{not json"))
	assert.Error(t, err)
}

func TestNormalizeTweetEntriesUnwrapsWrappedTweet(t *testing.T) {
	raw := []rawTweetEntry{
		{Result: json.RawMessage(`{"__typename":"Tweet","legacy":{"id_str":"1"}}`), SortIndex: "100"},
		{Result: json.RawMessage(`{"__typename":"TweetWithVisibilityResults","tweet":{"legacy":{"id_str":"2"}}}`), SortIndex: "200"},
		{Result: json.RawMessage(`{"__typename":"TweetUnavailable"}`), SortIndex: "300"},
	}
	clean := normalizeTweetEntries(raw)
	assert.Len(t, clean, 2, "unavailable entry is dropped")
	assert.Equal(t, "100", clean[0].SortIndex)
	assert.Equal(t, "200", clean[1].SortIndex, "sortIndex survives the unwrap")
	assert.Contains(t, string(clean[1].Result), `"id_str":"2"`)
}
