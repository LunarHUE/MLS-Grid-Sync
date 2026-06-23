// Package applog is a thin, process-wide serialization layer over libs-go/log.
//
// The underlying logger appends to a shared in-memory buffer on every call
// without holding its own mutex for that append (see libs-go/log/file.go), so
// two goroutines logging concurrently is a data race. Most of the app logs
// from a single goroutine, but the pipelined init (sync.fetchProcessAndEnqueuePipelined)
// runs a fetch producer and a processor consumer at the same time and both log.
// Routing those concurrent log sites through these mutex-guarded wrappers makes
// them safe without changing the visible output.
//
// Note: the wrapper adds one stack frame, but the console format does not print
// the caller file:line (only the disabled file sink does), so attribution is
// unaffected for normal operation.
package applog

import (
	"sync"

	"github.com/lunarhue/libs-go/log"
)

var mu sync.Mutex

// Infof logs at INFO under the shared lock.
func Infof(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	log.Infof(format, args...)
}

// Errorf logs at ERROR under the shared lock.
func Errorf(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	log.Errorf(format, args...)
}
