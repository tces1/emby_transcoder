package transcode

import "emby-transcoder/internal/logging"

func traceSwitch(format string, args ...any) {
	logging.Debugf("TRACE_SWITCH "+format, args...)
}
