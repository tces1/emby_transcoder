package transcode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"emby-transcoder/internal/logging"
)

const segmentForwardWindow = 6

type Handler struct {
	Manager       *Manager
	InputURLForID func(id string, r *http.Request) string
	StartupWait   time.Duration
	RoutePrefix   string
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := h.RoutePrefix
	if prefix == "" {
		prefix = "/streambridge/transcode/"
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}

	id := parts[0]
	name := filepath.Base(parts[1])
	if name == "." || name == "/" {
		http.NotFound(w, r)
		return
	}
	requestStarted := time.Now()

	var session *Session
	var ok bool
	if name == "master.m3u8" {
		logging.Infof("playlist request id=%s start_ticks=%d", id, startTimeTicksFromRawQuery(r.URL.RawQuery))
		traceSwitch("playlist_request id=%s start_ticks=%d query=%s elapsed=%s", id, startTimeTicksFromRawQuery(r.URL.RawQuery), redactURLString("?"+r.URL.RawQuery), time.Since(requestStarted))
		if info, known := h.Manager.MediaInfo(id); known && info.RunTimeTicks > 0 {
			session, err := h.ensureFromRequest(id, r)
			if err != nil {
				h.writeEnsureError(w, id, err)
				return
			}
			start, ready := h.Manager.PlaylistWindow(id)
			if session != nil && ready == 0 {
				firstSegment := filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", start))
				wait := h.startupWait()
				if !waitForFile(r.Context(), firstSegment, wait) {
					h.logWaitFailure(r.Context(), "first_segment", id, filepath.Base(firstSegment), wait, requestStarted)
					http.Error(w, "first segment is not ready", http.StatusGatewayTimeout)
					return
				}
				h.Manager.RecordSegmentReady(id, start)
				start, ready = h.Manager.PlaylistWindow(id)
			}
			if playlist, ok := GrowingMediaPlaylist(info, h.Manager.segmentTicks(), r.URL.RawQuery, start, ready); ok {
				logging.Infof("playlist growing id=%s duration=%s start=%d ready=%d", id, formatTicks(info.RunTimeTicks), start, ready)
				traceSwitch("playlist_growing id=%s duration=%s start=%d ready=%d start_ticks=%d query=%s media=%s elapsed=%s", id, formatTicks(info.RunTimeTicks), start, ready, startTimeTicksFromRawQuery(r.URL.RawQuery), redactURLString("?"+r.URL.RawQuery), info.Summary(), time.Since(requestStarted))
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write([]byte(playlist))
				return
			}
		}
		inputURL := ""
		if h.InputURLForID != nil {
			inputURL = h.InputURLForID(id, r)
		}
		var err error
		session, err = h.Manager.Ensure(id, requestFromHTTP(id, inputURL, r))
		if err != nil {
			logging.Errorf("transcode start error id=%s err=%v", id, err)
			if errors.Is(err, ErrTooManySessions) {
				http.Error(w, err.Error(), http.StatusTooManyRequests)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		logging.Infof("playlist session id=%s elapsed=%s", id, time.Since(requestStarted))
		traceSwitch("playlist_session_ready id=%s dir=%s input=%s elapsed=%s", id, session.Dir, redactURLString(inputURL), time.Since(requestStarted))
	} else if segmentIndex, isSegment := segmentIndexFromName(name); isSegment {
		var err error
		session, err = h.sessionForSegment(id, segmentIndex, name, r, requestStarted)
		if err != nil {
			logging.Errorf("transcode segment error id=%s file=%s err=%v", id, name, err)
			if errors.Is(err, ErrTooManySessions) {
				http.Error(w, err.Error(), http.StatusTooManyRequests)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	} else {
		session, ok = h.Manager.Get(id)
		if !ok {
			inputURL := ""
			if h.InputURLForID != nil {
				inputURL = h.InputURLForID(id, r)
			}
			logging.Debugf("transcode session miss id=%s file=%s input=%s", id, name, redactURLString(inputURL))
			var err error
			session, err = h.Manager.Ensure(id, requestFromHTTP(id, inputURL, r))
			if err != nil {
				logging.Errorf("transcode start error id=%s err=%v", id, err)
				if errors.Is(err, ErrTooManySessions) {
					http.Error(w, err.Error(), http.StatusTooManyRequests)
					return
				}
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			logging.Debugf("transcode session started id=%s dir=%s", id, session.Dir)
		} else {
			logging.Debugf("transcode session hit id=%s file=%s", id, name)
		}
	}

	filePath := filepath.Join(session.Dir, name)
	if name == "master.m3u8" {
		wait := h.startupWait()
		if !waitForFile(r.Context(), filePath, wait) {
			h.logWaitFailure(r.Context(), "playlist", id, name, wait, requestStarted)
			http.Error(w, "playlist is not ready", http.StatusGatewayTimeout)
			return
		}
		traceSwitch("playlist_file_ready id=%s path=%s elapsed=%s", id, filePath, time.Since(requestStarted))
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else if strings.HasSuffix(name, ".ts") {
		wait := h.startupWait()
		if !waitForFile(r.Context(), filePath, wait) {
			h.logWaitFailure(r.Context(), "segment", id, name, wait, requestStarted)
			http.Error(w, "segment is not ready", http.StatusGatewayTimeout)
			return
		}
		if index, ok := segmentIndexFromName(name); ok {
			h.Manager.RecordSegmentReady(id, index)
		}
		traceSwitch("segment_file_ready id=%s file=%s path=%s elapsed=%s", id, name, filePath, time.Since(requestStarted))
		w.Header().Set("Content-Type", "video/mp2t")
	}

	logging.Debugf("transcode serve file id=%s file=%s path=%s", id, name, filePath)
	counted := &countingResponseWriter{
		ResponseWriter: w,
		record: func(bytes int64) {
			h.Manager.RecordSessionUpload(session, bytes)
		},
	}
	http.ServeFile(counted, r, filePath)
}

type countingResponseWriter struct {
	http.ResponseWriter
	record func(int64)
}

func (w *countingResponseWriter) Write(data []byte) (int, error) {
	written, err := w.ResponseWriter.Write(data)
	if w.record != nil && written > 0 {
		w.record(int64(written))
	}
	return written, err
}

func (h Handler) sessionForSegment(id string, segmentIndex int, name string, r *http.Request, requestStarted time.Time) (*Session, error) {
	segmentReq := requestWithSegmentStart(r, segmentIndex, h.Manager.segmentTicks())
	inputURL := ""
	if h.InputURLForID != nil {
		inputURL = h.InputURLForID(id, segmentReq)
	}
	request := requestFromHTTP(id, inputURL, segmentReq)
	request.SegmentStartIndex = segmentIndex
	request.SegmentRequest = true
	request.SegmentTicks = h.Manager.segmentTicks()
	request.RequestedStartTimeTicks = int64Query(r.URL.Query().Get("StartTimeTicks"))
	if request.RequestedStartTimeTicks == 0 {
		request.RequestedStartTimeTicks = request.StartTimeTicks
	}
	if info, ok := h.Manager.MediaInfo(id); ok {
		request.Media = info
	}
	traceSwitch("segment_request id=%s file=%s segment=%d request_start_ticks=%d runtime_ticks=%s elapsed=%s query=%s", id, name, segmentIndex, request.StartTimeTicks, r.URL.Query().Get("runtimeTicks"), time.Since(requestStarted), redactURLString("?"+r.URL.RawQuery))

	if session, ok := h.Manager.Get(id); ok {
		switch {
		case sessionProcessDone(session):
			traceSwitch("segment_decision id=%s file=%s segment=%d decision=restart reason=process_done old_segment_start=%d old_start_ticks=%d elapsed=%s", id, name, segmentIndex, session.SegmentStartIndex, session.StartTimeTicks, time.Since(requestStarted))
		case !segmentInputCompatible(session.InputURL, inputURL):
			traceSwitch("segment_decision id=%s file=%s segment=%d decision=restart reason=input_changed old_input=%s new_input=%s elapsed=%s", id, name, segmentIndex, redactURLString(session.InputURL), redactURLString(inputURL), time.Since(requestStarted))
		case session.AudioStreamIndex != request.AudioStreamIndex:
			traceSwitch("segment_decision id=%s file=%s segment=%d decision=restart reason=audio_changed old_audio=%d new_audio=%d elapsed=%s", id, name, segmentIndex, session.AudioStreamIndex, request.AudioStreamIndex, time.Since(requestStarted))
		case segmentReusable(session, segmentIndex, name):
			traceSwitch("segment_decision id=%s file=%s segment=%d decision=hit session_start=%d dir=%s elapsed=%s", id, name, segmentIndex, session.SegmentStartIndex, session.Dir, time.Since(requestStarted))
			h.Manager.RecordSegmentRequest(id, segmentIndex)
			return session, nil
		default:
			traceSwitch("segment_decision id=%s file=%s segment=%d decision=restart reason=window_miss old_segment_start=%d old_start_ticks=%d elapsed=%s", id, name, segmentIndex, session.SegmentStartIndex, session.StartTimeTicks, time.Since(requestStarted))
		}
	}

	session, err := h.Manager.Ensure(id, request)
	if err != nil {
		return nil, err
	}
	h.Manager.RecordSegmentRequest(id, segmentIndex)
	traceSwitch("segment_ready id=%s file=%s segment=%d start_ticks=%d dir=%s input=%s elapsed=%s", id, name, segmentIndex, request.StartTimeTicks, session.Dir, redactURLString(inputURL), time.Since(requestStarted))
	return session, nil
}

func (h Handler) startupWait() time.Duration {
	if h.StartupWait > 0 {
		return h.StartupWait
	}
	return 20 * time.Second
}

func (h Handler) ensureFromRequest(id string, r *http.Request) (*Session, error) {
	inputURL := ""
	if h.InputURLForID != nil {
		inputURL = h.InputURLForID(id, r)
	}
	if inputURL == "" {
		return nil, nil
	}
	return h.Manager.Ensure(id, requestFromHTTP(id, inputURL, r))
}

func (h Handler) writeEnsureError(w http.ResponseWriter, id string, err error) {
	logging.Errorf("transcode start error id=%s err=%v", id, err)
	if errors.Is(err, ErrTooManySessions) {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func (h Handler) logWaitFailure(ctx context.Context, kind, id, name string, wait time.Duration, started time.Time) {
	reason := "timeout"
	if ctx.Err() != nil {
		reason = "canceled"
	}
	elapsed := time.Since(started)
	if name == "" {
		logging.Errorf("transcode %s %s id=%s wait=%s elapsed=%s", kind, reason, id, wait, elapsed)
	} else {
		logging.Errorf("transcode %s %s id=%s file=%s wait=%s elapsed=%s", kind, reason, id, name, wait, elapsed)
	}
	traceSwitch("%s_%s id=%s file=%s wait=%s elapsed=%s", kind, reason, id, name, wait, elapsed)
}

func segmentReusable(session *Session, segmentIndex int, name string) bool {
	if segmentIndex < session.OldestSegmentKept {
		return false
	}
	if segmentIndex < session.SegmentStartIndex {
		return false
	}
	if fileExists(filepath.Join(session.Dir, name)) {
		return true
	}
	highest := session.HighestSegmentSeen
	if highest < session.SegmentStartIndex {
		highest = session.SegmentStartIndex
	}
	return segmentIndex <= highest+segmentForwardWindow
}

func segmentInputCompatible(existing string, next string) bool {
	if existing == "" || next == "" {
		return true
	}
	return segmentInputFingerprint(existing) == segmentInputFingerprint(next)
}

func segmentInputFingerprint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	removeLocalAndPlaybackOnlyQuery(query)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func requestWithSegmentStart(r *http.Request, segmentIndex int, segmentTicks int64) *http.Request {
	req := r.Clone(r.Context())
	u := *r.URL
	query := u.Query()
	query.Set("StartTimeTicks", strconv.FormatInt(segmentStartTicksFromRequest(r, segmentIndex, segmentTicks), 10))
	query.Del("runtimeTicks")
	query.Del("actualSegmentLengthTicks")
	u.RawQuery = query.Encode()
	req.URL = &u
	return req
}

func segmentStartTicksFromRequest(r *http.Request, segmentIndex int, segmentTicks int64) int64 {
	if raw := r.URL.Query().Get("runtimeTicks"); raw != "" {
		return int64Query(raw)
	}
	return segmentStartTicks(segmentIndex, segmentTicks)
}

func segmentStartTicks(segmentIndex int, segmentTicks int64) int64 {
	if segmentTicks <= 0 {
		segmentTicks = defaultSegmentTicks
	}
	return int64(segmentIndex) * segmentTicks
}

func removeLocalAndPlaybackOnlyQuery(query url.Values) {
	for _, key := range []string{
		"StartTimeTicks",
		"runtimeTicks",
		"actualSegmentLengthTicks",
		"PlaySessionId",
		"CurrentPlaySessionId",
		"AllowAudioStreamCopy",
		"AllowVideoStreamCopy",
		"EnableDirectPlay",
		"EnableDirectStream",
		"SubtitleStreamIndex",
		"IsPlayback",
		"AutoOpenLiveStream",
		"reqformat",
	} {
		query.Del(key)
	}
}

func segmentIndexFromName(name string) (int, bool) {
	if !strings.HasPrefix(name, "segment_") || !strings.HasSuffix(name, ".ts") {
		return 0, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, "segment_"), ".ts")
	index, err := strconv.Atoi(raw)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func requestFromHTTP(id string, inputURL string, r *http.Request) Request {
	query := r.URL.Query()
	playSessionID := query.Get("PlaySessionId")
	if playSessionID == "" {
		playSessionID = query.Get("CurrentPlaySessionId")
	}
	return Request{
		InputURL:                inputURL,
		Headers:                 r.Header,
		ItemID:                  id,
		MediaSourceID:           query.Get("MediaSourceId"),
		PlaySessionID:           playSessionID,
		UpstreamPlaySessionID:   playSessionIDFromURL(inputURL),
		AudioStreamIndex:        int(int64Query(query.Get("AudioStreamIndex"))),
		HasAudioStreamIndex:     query.Has("AudioStreamIndex"),
		StartTimeTicks:          int64Query(query.Get("StartTimeTicks")),
		RequestedStartTimeTicks: int64Query(query.Get("StartTimeTicks")),
	}
}

func playSessionIDFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Query().Get("PlaySessionId")
}

func int64Query(raw string) int64 {
	value, _ := strconv.ParseInt(raw, 10, 64)
	return value
}

func waitForFile(ctx context.Context, path string, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		if info, err := filepath.Abs(path); err == nil && info != "" {
			if fileExists(path) {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

func fileExists(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && !stat.IsDir() && stat.Size() > 0
}
