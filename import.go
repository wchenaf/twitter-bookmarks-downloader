package main

import (
	"context"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/rotisserie/eris"

	"twitter-bookmarks-downloader/ent"
	"twitter-bookmarks-downloader/ent/tweet"
)

func ImportLegacyData(tweetsDir string) error {
	ctx := context.Background()
	PrintInfoF("Starting legacy import from: %s", tweetsDir)

	files, err := os.ReadDir(tweetsDir)
	if err != nil {
		return eris.Wrap(err, "failed to read tweets directory")
	}

	count := 0
	skipped := 0

	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}

		filePath := filepath.Join(tweetsDir, file.Name())
		imported, err := processLegacyFile(ctx, filePath)
		if err != nil {
			PrintError(eris.Wrapf(err, "Failed to import %s", file.Name()))
			continue
		}
		if imported {
			count++
		} else {
			skipped++
		}
	}

	PrintInfoF("Legacy import complete. Processed: %d, Skipped (existing): %d", count, skipped)
	return nil
}

// processLegacyFile imports one serialized twitter-scraper JSON file through
// the deriver's legacy branch. Existing tweets are skipped outright: the web
// capture's metadata is newer and more complete, so an import never overwrites
// what a sync has written.
func processLegacyFile(ctx context.Context, filePath string) (imported bool, err error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return false, eris.Wrap(err, "failed to read file")
	}

	d, err := DeriveTweet(content)
	if err != nil {
		return false, err
	}

	exists, err := DB.Tweet.Query().Where(tweet.ID(d.ID)).Exist(ctx)
	if err != nil {
		return false, eris.Wrap(err, "failed to check tweet existence")
	}
	if exists {
		return false, nil
	}

	err = WithTx(ctx, func(tx *ent.Tx) error {
		// Imported tweets were bookmarks once but are no longer on the list,
		// hence bookmarked=formerly (REBUILD_NOTES 5.3). The seeding rule
		// (4.3) applies to them all the same: a past bookmark is a cheap
		// positive judgment, so rating starts at the lowest positive step.
		err := tx.Tweet.Create().
			SetID(d.ID).
			SetName(d.Name).
			SetScreenName(d.ScreenName).
			SetFullText(d.FullText).
			SetCreatedAt(d.CreatedAt).
			SetPermanentURL(d.PermanentURL).
			SetRawJSON(d.RawJSON).
			SetSyncedAt(time.Now()).
			SetBookmarked(tweet.BookmarkedFormerly).
			SetRating(1).
			Exec(ctx)
		if err != nil {
			return err
		}

		builders := mediaCreates(tx, d)
		for i, m := range d.Media {
			// Self-healing: media already on disk needs no download.
			if onDisk, err := checkMediaFileExists(d, i, len(d.Media), m.URL); err == nil && onDisk {
				builders[i].SetDownloaded(true)
			}
		}
		if len(builders) == 0 {
			return nil
		}
		return tx.Media.CreateBulk(builders...).Exec(ctx)
	})
	if err != nil {
		return false, eris.Wrap(err, "db create failed")
	}

	return true, nil
}

func checkMediaFileExists(d *DerivedTweet, index int, total int, mediaURL string) (bool, error) {
	parsedURL, err := url.Parse(mediaURL)
	if err != nil {
		return false, err
	}

	filename := buildFilename(d.ScreenName, d.ID, d.CreatedAt, index, total, parsedURL)
	fullPath := path.Join(config.MediaDir, filename)

	if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
		// File exists
		// Optional: We could check file size if we had Content-Length, but we don't.
		// Assuming existence is enough for legacy migration.
		return true, nil
	}

	return false, nil
}
