package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rotisserie/eris"
)

func StartDownloadWorker() {
	PrintInfo("Download worker started")

	// Initial report on startup (force display)
	ReportWorkerStatus(true)

	ticker := time.NewTicker(5 * time.Second)
	statusTicker := time.NewTicker(10 * time.Minute)

	// A bookmarks sync arrives one page at a time, seconds apart, so the queue
	// empties and refills repeatedly over a single run. Firing on the first
	// empty tick would run the idle command several times per sync; requiring
	// the queue to stay empty for a while collapses the whole run into one.
	var (
		idleTicks int
		savedRun  int // downloads accumulated since the last time the hook ran
	)

	for {
		select {
		case <-ticker.C:
			attempted, saved := DownloadPendingMedia()
			savedRun += saved
			if attempted > 0 {
				idleTicks = 0
				continue
			}
			// Nothing left to do. Only worth telling anyone if this quiet
			// followed actual downloads rather than a queue that was already
			// empty, otherwise the hook would fire every minute forever.
			if savedRun == 0 {
				continue
			}
			idleTicks++
			if idleTicks >= idleTicksBeforeHook {
				// A round the hook refuses keeps its count, so media that
				// landed while an earlier run was still uploading is handed
				// off by the next quiet period instead of being dropped.
				if RunOnIdle(savedRun) {
					savedRun = 0
				}
				idleTicks = 0
			}
		case <-statusTicker.C:
			ReportWorkerStatus(false)
		}
	}
}

// How many consecutive empty ticks count as the queue having settled.
const idleTicksBeforeHook = 12 // 12 x 5s = one minute

var onIdleRunning atomic.Bool

// RunOnIdle hands off to whatever the user configured once downloads have
// settled. The command is deliberately opaque to this project: uploading to a
// particular photo server is not something tbd should promise to support, so
// the hook stays a generic "something new landed" signal and the choice of what
// that means lives outside the repository.
//
// It reports whether the batch was taken off the caller's hands, which is false
// only when a previous run is still going: that count has to survive so the
// media it stands for is not silently forgotten.
func RunOnIdle(saved int) bool {
	if config.OnIdleCmd == "" {
		return true
	}
	// Uploads outlast the interval that triggers them, so a second sync
	// finishing mid-upload must not start a competing run.
	if !onIdleRunning.CompareAndSwap(false, true) {
		PrintWarning("Idle command still running, deferring this round")
		return false
	}

	go func() {
		defer onIdleRunning.Store(false)

		PrintInfoF("%d new media settled, running idle command", saved)
		out, err := exec.Command("sh", "-c", config.OnIdleCmd).CombinedOutput()
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line != "" {
				PrintInfoF("  | %s", line)
			}
		}
		if err != nil {
			PrintError(eris.Wrap(err, "Idle command failed"))
			return
		}
		PrintInfo("Idle command finished")
	}()
	return true
}

func ReportWorkerStatus(force bool) {
	var pending int64
	var failed int64
	if err := DB.Model(&MediaModel{}).Where("downloaded = ? AND failed = ?", false, false).Count(&pending).Error; err != nil {
		PrintError(eris.Wrap(err, "Failed to count pending media"))
	}
	if err := DB.Model(&MediaModel{}).Where("failed = ?", true).Count(&failed).Error; err != nil {
		PrintError(eris.Wrap(err, "Failed to count failed media"))
	}

	if force || pending > 0 || failed > 0 {
		PrintInfoF("[Worker Status] Pending: %d | Failed: %d", pending, failed)
		if failed > 0 {
			PrintWarningF("  %d media items failed permanently after multiple retries.", failed)
		}
	}
}

// DownloadPendingMedia returns how many items it took off the queue and how
// many of those actually landed on disk. The caller needs both: attempted tells
// it whether the queue still has work, saved tells it whether anything new is
// worth acting on. An item that keeps failing stays pending until it is struck
// out, so it keeps attempted above zero and correctly reads as work in progress
// rather than an idle queue.
func DownloadPendingMedia() (attempted, saved int) {
	var mediaList []MediaModel
	// Find up to 5 pending downloads
	err := DB.Where("downloaded = ? AND failed = ?", false, false).Limit(5).Find(&mediaList).Error
	if err != nil {
		PrintError(eris.Wrap(err, "Failed to query pending media"))
		return 0, 0
	}

	if len(mediaList) == 0 {
		return 0, 0
	}

	for _, media := range mediaList {
		err := processMediaDownload(&media)
		if err != nil {
			PrintError(eris.Wrapf(err, "Media ID: %s", media.ID))
			media.RetryCount++
			if media.RetryCount >= 3 {
				media.Failed = true
			}
		} else {
			media.Downloaded = true
			saved++
		}
		if err := DB.Save(&media).Error; err != nil {
			PrintError(eris.Wrapf(err, "Failed to update media status for %s", media.ID))
		}
	}
	return len(mediaList), saved
}

func processMediaDownload(media *MediaModel) error {
	var tweet TweetModel
	if err := DB.First(&tweet, "id = ?", media.TweetID).Error; err != nil {
		return eris.Wrap(err, "Tweet not found for media")
	}

	parsedURL, err := url.Parse(media.URL)
	if err != nil {
		return err
	}

	// 规则 5: Adjust URL for high quality photos
	if media.Type == "photo" {
		params := parsedURL.Query()
		params.Set("name", "orig")
		parsedURL.RawQuery = params.Encode()
	}

	// Determine total media count for this tweet
	var mediaCount int64
	if err := DB.Model(&MediaModel{}).Where("tweet_id = ?", tweet.ID).Count(&mediaCount).Error; err != nil {
		return eris.Wrap(err, "Failed to count tweet media")
	}

	filename := buildFilename(&tweet, media.Index, int(mediaCount), parsedURL)
	outputPath := path.Join(config.MediaDir, filename)

	if err := downloadFile(parsedURL.String(), outputPath, tweet.CreatedAt); err != nil {
		return eris.Wrapf(err, "URL: %s (from Tweet: %s)", media.URL, tweet.PermanentURL)
	}

	return nil
}

// 规则 1, 2, 3, 4: 严格遵循原版文件名规则
func buildFilename(tweet *TweetModel, index int, total int, url *url.URL) string {
	fileExt := strings.ToLower(path.Ext(url.Path))
	filenameBase := fmt.Sprintf("twitter-@%s-%s-%s",
		tweet.ScreenName,
		tweet.CreatedAt.In(time.Local).Format("20060102-150405"),
		tweet.ID,
	)

	if total > 1 {
		return fmt.Sprintf("%s-%d%s", filenameBase, index, fileExt)
	}
	return fmt.Sprintf("%s%s", filenameBase, fileExt)
}

func downloadFile(urlStr string, outputPath string, modTime time.Time) error {
	if _, err := os.Stat(config.MediaDir); os.IsNotExist(err) {
		if err := os.MkdirAll(config.MediaDir, 0o755); err != nil {
			return eris.Wrap(err, "failed to create media directory")
		}
	}

	// 像原版一样校验文件大小
	if fileInfo, err := os.Stat(outputPath); err == nil {
		resp, err := http.Head(urlStr)
		if err != nil {
			PrintWarning("Failed to check remote file size, force downloading")
		} else {
			if resp.ContentLength > 0 {
				if fileInfo.Size() == resp.ContentLength {
					if err := resp.Body.Close(); err != nil {
						PrintWarningF("Failed to close HEAD response body: %v", err)
					}
					PrintInfoF("  Skipped: %s", outputPath)
					return nil
				}
			}
			if err := resp.Body.Close(); err != nil {
				PrintWarningF("Failed to close HEAD response body: %v", err)
			}
		}
	}

	resp, err := http.Get(urlStr)
	if err != nil {
		return eris.Wrap(err, "failed to download file from URL")
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			PrintWarningF("Failed to close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return eris.Errorf("failed to download file, status code: %d", resp.StatusCode)
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return eris.Wrap(err, "failed to create local file")
	}

	written, copyErr := io.Copy(out, resp.Body)

	// Explicitly close file to ensure flush and release lock
	closeErr := out.Close()

	if copyErr != nil {
		return eris.Wrap(copyErr, "failed to copy content to local file")
	}
	if closeErr != nil {
		return eris.Wrap(closeErr, "failed to close local file")
	}

	// Verify download completeness
	if resp.ContentLength > 0 {
		if written != resp.ContentLength {
			return eris.Errorf("download incomplete, expected %d bytes, got %d bytes", resp.ContentLength, written)
		}
	} else {
		PrintWarning("Content-Length header not provided by the server.")
	}

	// Restore strict error check for Chtimes as per legacy code
	if err := os.Chtimes(outputPath, time.Now(), modTime); err != nil {
		return eris.Wrap(err, "failed to set modified time")
	}

	PrintInfoF("  Downloaded: %s", outputPath)
	return nil
}
