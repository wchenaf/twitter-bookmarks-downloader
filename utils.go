package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/fatih/color"
	"github.com/rotisserie/eris"
)

var (
	errorColor   = color.New(color.FgRed)
	warningColor = color.New(color.FgYellow)
	infoColor    = color.New(color.FgGreen)
	fatalColor   = color.New(color.FgRed, color.Bold)
	clockColor   = color.New(color.FgHiBlack)

	logFile *os.File
)

func InitLogger(logPath string) error {
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	logFile = file
	return nil
}

func CloseLogger() {
	if logFile == nil {
		return
	}
	if err := logFile.Close(); err != nil {
		// FatalError would re-enter this function from its cleanup path; the
		// log file is unusable either way, so a bare stderr line has to do.
		fmt.Fprintln(os.Stderr, "Failed to close log file:", err)
	}
	logFile = nil
}

func logToFile(t time.Time, level string, message string) {
	if logFile == nil {
		return
	}
	_, err := fmt.Fprintln(logFile, t.Format("2006-01-02 15:04:05"), level, message)
	if err != nil {
		// FatalError logs through emit and thus back through here: a failing
		// log write escalated to fatal would recurse until the stack died.
		fmt.Fprintln(os.Stderr, "Failed to write log file:", err)
	}
}

// emit writes one line to the console and one to the log file, both stamped
// from the same instant.
//
// The console gets a bare clock while the file also carries the date: a
// terminal session is implicitly today, whereas the log accumulates across days
// and a time with no date cannot be lined up against anything. Dimming the
// clock keeps it out of the way during a manual run, which is why the stamp was
// left out to begin with; a process that runs for weeks needs it regardless,
// since an undated line is worthless the moment something has to be traced.
func emit(w io.Writer, tag *color.Color, level string, message string) {
	now := time.Now()
	fmt.Fprintln(w, clockColor.Sprint(now.Format("15:04:05")), tag.Sprint(level), message)
	logToFile(now, level, message)
}

func PrintError(err error) {
	emit(os.Stderr, errorColor, "[ERROR]", eris.ToString(err, !isReleaseBuild))
}

func PrintWarning(message ...any) {
	emit(os.Stdout, warningColor, "[WARN]", fmt.Sprint(message...))
}

func PrintWarningF(format string, args ...any) {
	emit(os.Stdout, warningColor, "[WARN]", fmt.Sprintf(format, args...))
}

func PrintInfo(message ...any) {
	emit(os.Stdout, infoColor, "[INFO]", fmt.Sprint(message...))
}

func PrintInfoF(format string, args ...any) {
	emit(os.Stdout, infoColor, "[INFO]", fmt.Sprintf(format, args...))
}

func FatalError(err error) {
	emit(os.Stderr, fatalColor, "[ERROR]", eris.ToString(err, !isReleaseBuild))
	// os.Exit skips every deferred cleanup in main, so a fatal exit runs the
	// same cleanup here. Both closers only warn on failure: escalating from
	// inside the shutdown path would recurse into FatalError.
	CloseDB()
	CloseLogger()
	os.Exit(1)
}

func CloseResource(closer io.Closer) {
	err := closer.Close()
	if err != nil {
		FatalError(err)
	}
}
