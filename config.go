package main

import (
	"os"
	"runtime/debug"
	"strings"
)

type Config struct {
	MediaDir string

	// Shell command run once a batch of downloads has settled. Empty by
	// default, which is the whole point: what to do with newly downloaded
	// media is a local matter, so the project ships the trigger and not the
	// destination.
	OnIdleCmd string
}

var isReleaseBuild bool

var config Config = Config{
	MediaDir:  "media",
	OnIdleCmd: os.Getenv("TBD_ON_IDLE_CMD"),
}

func init() {
	bi, ok := debug.ReadBuildInfo()
	if ok {
		for _, setting := range bi.Settings {
			if setting.Key == "-tags" && strings.Contains(setting.Value, "release") {
				isReleaseBuild = true
				break
			}
		}
	}
}
