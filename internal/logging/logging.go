package logging

import (
	"log"
	"sync/atomic"
)

var debugEnabled atomic.Bool

func SetDebug(enabled bool) {
	debugEnabled.Store(enabled)
}

func DebugEnabled() bool {
	return debugEnabled.Load()
}

func Infof(format string, args ...any) {
	log.Printf(format, args...)
}

func Debugf(format string, args ...any) {
	if debugEnabled.Load() {
		log.Printf(format, args...)
	}
}

func Errorf(format string, args ...any) {
	log.Printf(format, args...)
}
