// Package applog is a thin, process-wide serialization layer over libs-go/log.
//
// The underlying logger appends to a shared in-memory buffer on every call
// without holding its own mutex for that append (see libs-go/log/file.go), so
// two goroutines logging concurrently is a data race. Most of the app logs
// from a single goroutine, but the pipelined init (sync.fetchProcessAndEnqueuePipelined)
// runs a fetch producer and a processor consumer at the same time and both log.
// Routing those concurrent log sites through these wrappers makes them safe
// without changing the visible output.
//
// Each call hands its formatted message to progress.Log, which (a) when live
// progress bars are on screen prints the line ABOVE the bars via mpb's writer —
// the only safe way to emit while bars render — and (b) otherwise routes to the
// underlying logger under a shared lock, the race guard the concurrent
// producer/consumer logging needs.
//
// Note: the wrapper adds one stack frame, but the console format does not print
// the caller file:line (only the disabled file sink does), so attribution is
// unaffected for normal operation.
package applog

import (
	"fmt"

	"github.com/LunarHUE/MLS-Grid-Sync/internal/progress"
)

// Infof logs at INFO, coordinated with any live progress bars.
func Infof(format string, args ...any) {
	progress.Log(progress.LevelInfo, fmt.Sprintf(format, args...))
}

// Errorf logs at ERROR, coordinated with any live progress bars.
func Errorf(format string, args ...any) {
	progress.Log(progress.LevelError, fmt.Sprintf(format, args...))
}

// Debugf logs at DEBUG, coordinated with any live progress bars. Used for the
// high-volume per-page/per-chunk lines that are demoted out of the default
// INFO stream (concurrent fetch workers log these, so they still need the
// shared lock at DEBUG level).
func Debugf(format string, args ...any) {
	progress.Log(progress.LevelDebug, fmt.Sprintf(format, args...))
}
