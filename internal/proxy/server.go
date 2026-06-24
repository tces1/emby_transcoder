package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/emby"
	"emby-transcoder/internal/logging"
	"emby-transcoder/internal/policy"
	"emby-transcoder/internal/transcode"
)

type Server struct {
	cfg              config.Config
	upstream         *url.URL
	reverseProxy     *httputil.ReverseProxy
	client           *http.Client
	transcodeHandler http.Handler
	cancelReaper     context.CancelFunc
	transcodeManager *transcode.Manager
}

var embyTokenPattern = regexp.MustCompile(`(?i)\btoken\s*=\s*"?([^",\s]+)"?`)

func New(cfg config.Config) (*Server, error) {
	return NewWithTransport(cfg, http.DefaultTransport)
}

func NewWithTransport(cfg config.Config, transport http.RoundTripper) (*Server, error) {
	upstream, err := url.Parse(cfg.Upstream.URL)
	if err != nil {
		return nil, err
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("invalid upstream url: %s", cfg.Upstream.URL)
	}

	manager, err := transcode.NewManagerStrict(transcode.Options{
		MaxSessions:           cfg.Transcode.MaxSessions,
		TempDir:               cfg.Transcode.TempDir,
		FFmpegPath:            cfg.Transcode.FFmpegPath,
		HardwareDecode:        cfg.Transcode.HardwareDecode,
		HardwareDevice:        cfg.Transcode.HardwareDevice,
		BufferPauseThreshold:  cfg.Transcode.BufferPause,
		BufferResumeThreshold: cfg.Transcode.BufferResume,
		SegmentDuration:       cfg.Transcode.SegmentDuration,
		SegmentRetention:      cfg.Transcode.SegmentRetention,
		IdleTimeout:           cfg.Transcode.IdleTimeout,
	})
	if err != nil {
		return nil, err
	}
	logging.SetDebug(cfg.Server.Debug)
	handler := transcode.Handler{
		Manager: manager,
		InputURLForID: func(id string, r *http.Request) string {
			return transcodeInputURL(upstream, id, r)
		},
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = upstream.Host
	}
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
	client := &http.Client{Transport: transport}

	reaperCtx, cancelReaper := context.WithCancel(context.Background())
	go manager.StartReaper(reaperCtx)

	return &Server{
		cfg:              cfg,
		upstream:         upstream,
		reverseProxy:     proxy,
		client:           client,
		transcodeHandler: handler,
		cancelReaper:     cancelReaper,
		transcodeManager: manager,
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if normalizedPath, ok := normalizeTranscodePath(r.URL.Path); ok {
		logging.Debugf("transcode request method=%s path=%s remote=%s", r.Method, redactURLString(r.URL.String()), r.RemoteAddr)
		transcodeReq := r.Clone(r.Context())
		transcodeURL := *r.URL
		transcodeURL.Path = normalizedPath
		transcodeURL.RawPath = ""
		transcodeReq.URL = &transcodeURL
		s.transcodeHandler.ServeHTTP(w, transcodeReq)
		return
	}

	if kind, ok := playbackCheckInKind(r.URL.Path); ok {
		s.observePlaybackCheckIn(kind, r)
	}

	if isPlaybackInfoPath(r.URL.Path) {
		itemID := itemIDFromPlaybackPath(r.URL.Path)
		if !s.cfg.Transcode.Enabled {
			logging.Infof("playbackinfo passthrough item=%s reason=transcode_disabled", itemID)
			s.reverseProxy.ServeHTTP(w, r)
			return
		}
		result := policy.ShouldTranscode(r.Header, s.cfg.Clients)
		if result.Enabled {
			logging.Infof(
				"playbackinfo request item=%s profile=%s ua=%q query=%s token=%t",
				itemID,
				result.ProfileName,
				r.Header.Get("User-Agent"),
				redactURLString("?"+r.URL.RawQuery),
				embyTokenFromHeaders(r.Header) != "",
			)
			logging.Infof("playbackinfo rewrite item=%s profile=%s", itemID, result.ProfileName)
			s.handlePlaybackInfo(w, r)
			return
		}
		reason := "no_matching_profile"
		if result.ProfileName != "" {
			reason = "profile_transcode_disabled"
		}
		logging.Infof("playbackinfo passthrough item=%s profile=%s reason=%s", itemID, result.ProfileName, reason)
		logging.Debugf("playbackinfo passthrough detail item=%s remote=%s user_agent=%q", itemID, r.RemoteAddr, r.Header.Get("User-Agent"))
	}

	s.reverseProxy.ServeHTTP(w, r)
}

func (s *Server) observePlaybackCheckIn(kind string, r *http.Request) {
	body, err := readAndReplaceBody(r)
	if err != nil {
		logging.Errorf("playback checkin read error kind=%s err=%v", kind, err)
		return
	}
	event, ok, err := parsePlaybackEvent(body)
	if err != nil {
		logging.Errorf("playback checkin parse error kind=%s err=%v", kind, err)
		return
	}
	if !ok {
		logging.Debugf("playback checkin ignored kind=%s path=%s", kind, r.URL.Path)
		return
	}

	switch kind {
	case "stopped":
		stopped := s.transcodeManager.StopPlayback(event)
		logging.Infof("playback stopped item=%s stopped_sessions=%d", event.ItemID, stopped)
		logging.Debugf("playback stopped detail item=%s play_session=%s stopped_sessions=%d", event.ItemID, event.PlaySessionID, stopped)
	case "playing", "progress":
		updated := s.transcodeManager.RecordProgress(event)
		logging.Debugf("playback %s item=%s play_session=%s position_ticks=%d paused=%t updated_sessions=%d", kind, event.ItemID, event.PlaySessionID, event.PositionTicks, event.IsPaused, updated)
	}
}

func (s *Server) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	body, err := readAndReplaceBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, s.upstreamURL(r.URL), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	upstreamReq.Header = r.Header.Clone()
	upstreamReq.Header.Del("Accept-Encoding")

	resp, err := s.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	logging.Infof("playbackinfo upstream item=%s status=%d content_type=%q bytes=%d", itemIDFromPlaybackPath(r.URL.Path), resp.StatusCode, resp.Header.Get("Content-Type"), len(respBody))

	copyHeaders(w.Header(), resp.Header)
	itemID := itemIDFromPlaybackPath(r.URL.Path)
	publicURL := s.publicURL(r)
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") && itemID != "" {
		rewritten, changed, report, err := emby.RewritePlaybackInfoWithReport(respBody, itemID, publicURL, authAwareRawQuery(r))
		if err != nil {
			logging.Errorf("playbackinfo rewrite error item=%s err=%v", itemID, err)
		}
		if err == nil && changed {
			respBody = rewritten
			w.Header().Set("Content-Type", "application/json")
			w.Header().Del("Content-Length")
			logging.Infof("playbackinfo rewritten item=%s sources=%d", itemID, len(report.Sources))
			for _, source := range report.Sources {
				mediaInfo := transcode.MediaInfo{
					ItemID:        itemID,
					SourceID:      source.ID,
					Name:          source.Name,
					Path:          source.Path,
					Container:     source.Container,
					VideoCodec:    source.VideoCodec,
					VideoProfile:  source.VideoProfile,
					VideoPixFmt:   source.VideoPixelFormat,
					VideoBitDepth: source.VideoBitDepth,
					Width:         source.Width,
					Height:        source.Height,
					AudioCodec:    source.AudioCodec,
					AudioChannels: source.AudioChannels,
					AudioTitle:    source.AudioTitle,
					Bitrate:       source.Bitrate,
					RunTimeTicks:  source.RunTimeTicks,
				}
				for _, audio := range source.AudioStreams {
					mediaInfo.AudioStreams = append(mediaInfo.AudioStreams, transcode.AudioStreamInfo{
						Index:    audio.Index,
						Ordinal:  audio.Ordinal,
						Codec:    audio.Codec,
						Channels: audio.Channels,
						Title:    audio.Title,
					})
				}
				s.transcodeManager.RememberMedia(source.SessionID, mediaInfo)
				logging.Infof(
					"playbackinfo source item=%s session=%s index=%d source_id=%q direct=%t transcode=%t after=%s",
					itemID,
					source.SessionID,
					source.Index,
					source.ID,
					source.BeforeSupportsDirectPlay,
					source.BeforeSupportsTranscode,
					redactURLString(source.AfterTranscodingURL),
				)
				logging.Debugf(
					"playbackinfo source item=%s session=%s index=%d source_id=%q before_direct=%t before_transcode=%t had_direct_stream_url=%t had_transcoding_url=%t after=%s media=%s",
					itemID,
					source.SessionID,
					source.Index,
					source.ID,
					source.BeforeSupportsDirectPlay,
					source.BeforeSupportsTranscode,
					source.BeforeDirectStreamURL != "",
					source.BeforeTranscodingURL != "",
					redactURLString(source.AfterTranscodingURL),
					mediaInfo.Summary(),
				)
			}
			s.prewarmPlaybackInfoTranscode(r, report)
		}
		if err == nil && !changed {
			logging.Infof("playbackinfo unchanged item=%s reason=no_media_sources", itemID)
		}
	} else {
		logging.Infof("playbackinfo unchanged item=%s reason=non_json_or_missing_item", itemID)
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (s *Server) upstreamURL(in *url.URL) string {
	u := *s.upstream
	u.Path = singleJoiningSlash(s.upstream.Path, in.Path)
	u.RawQuery = in.RawQuery
	return u.String()
}

func (s *Server) prewarmPlaybackInfoTranscode(r *http.Request, report emby.RewriteReport) {
	if s.transcodeManager == nil || len(report.Sources) == 0 {
		return
	}
	itemID := report.ItemID
	source := report.Sources[0]
	audioStreamIndex := preferredAudioStreamIndex(source.AudioStreams)
	if raw := r.URL.Query().Get("AudioStreamIndex"); raw != "" {
		audioStreamIndex = int(int64Query(raw))
	}
	inputURL := prewarmTranscodeInputURL(s.upstream, itemID, source.ID, audioStreamIndex, r)
	request := transcode.Request{
		InputURL:                inputURL,
		Headers:                 r.Header.Clone(),
		ItemID:                  itemID,
		MediaSourceID:           source.ID,
		PlaySessionID:           source.SessionID,
		AudioStreamIndex:        audioStreamIndex,
		StartTimeTicks:          int64Query(r.URL.Query().Get("StartTimeTicks")),
		RequestedStartTimeTicks: int64Query(r.URL.Query().Get("StartTimeTicks")),
	}
	if info, ok := s.transcodeManager.MediaInfo(source.SessionID); ok {
		request.Media = info
	}
	logging.Infof(
		"playbackinfo prewarm request item=%s source=%s query=%s input=%s",
		itemID,
		source.ID,
		redactURLString("?"+r.URL.RawQuery),
		redactURLString(inputURL),
	)
	started := time.Now()
	go func() {
		if _, ok := s.transcodeManager.Get(source.SessionID); ok {
			logging.Debugf("playbackinfo prewarm skipped item=%s source=%s reason=session_exists", itemID, source.ID)
			return
		}
		session, err := s.transcodeManager.Ensure(source.SessionID, request)
		if err != nil {
			logging.Debugf("playbackinfo prewarm skipped item=%s source=%s err=%v", itemID, source.ID, err)
			return
		}
		logging.Infof("playbackinfo prewarm item=%s source=%s elapsed=%s", itemID, source.ID, time.Since(started))
		logging.Debugf("playbackinfo prewarm detail item=%s source=%s input=%s session_dir=%s", itemID, source.ID, redactURLString(inputURL), session.Dir)
	}()
}

func int64Query(raw string) int64 {
	value, _ := strconv.ParseInt(raw, 10, 64)
	return value
}

func preferredAudioStreamIndex(streams []emby.AudioStreamReport) int {
	if len(streams) == 0 {
		return 0
	}
	return streams[0].Index
}

func prewarmTranscodeInputURL(upstream *url.URL, id string, mediaSourceID string, audioStreamIndex int, r *http.Request) string {
	u := *upstream
	u.Path = singleJoiningSlash(upstream.Path, path.Join("/emby/Videos", id, "stream"))
	query := authAwareQuery(r)
	query.Del("reqformat")
	query.Set("AutoOpenLiveStream", "true")
	query.Set("IsPlayback", "true")
	if mediaSourceID != "" {
		query.Set("MediaSourceId", mediaSourceID)
	}
	if audioStreamIndex > 0 {
		query.Set("AudioStreamIndex", strconv.Itoa(audioStreamIndex))
	}
	u.RawQuery = query.Encode()
	return u.String()
}

func transcodeInputURL(upstream *url.URL, id string, r *http.Request) string {
	u := *upstream
	u.Path = singleJoiningSlash(upstream.Path, path.Join("/emby/Videos", id, "stream"))
	query := authAwareQuery(r)
	query.Del("reqformat")
	u.RawQuery = query.Encode()
	return u.String()
}

func authAwareRawQuery(r *http.Request) string {
	return authAwareQuery(r).Encode()
}

func authAwareQuery(r *http.Request) url.Values {
	query := r.URL.Query()
	if query.Get("X-Emby-Token") == "" {
		if token := embyTokenFromHeaders(r.Header); token != "" {
			query.Set("X-Emby-Token", token)
		}
	}
	return query
}

func embyTokenFromHeaders(headers http.Header) string {
	for _, key := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return value
		}
	}
	for _, key := range []string{"X-Emby-Authorization", "Authorization"} {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			if token := extractEmbyToken(value); token != "" {
				return token
			}
		}
	}
	return ""
}

func extractEmbyToken(value string) string {
	match := embyTokenPattern.FindStringSubmatch(value)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.Server.PublicURL != "" {
		return s.cfg.Server.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host
}

func (s *Server) TranscodeMediaInfo(id string) (transcode.MediaInfo, bool) {
	return s.transcodeManager.MediaInfo(id)
}

func isPlaybackInfoPath(p string) bool {
	parts := splitPath(p)
	for i := 0; i+2 < len(parts); i++ {
		if strings.EqualFold(parts[i], "Items") && strings.EqualFold(parts[i+2], "PlaybackInfo") {
			return true
		}
	}
	return false
}

func itemIDFromPlaybackPath(p string) string {
	parts := splitPath(p)
	for i := 0; i+2 < len(parts); i++ {
		if strings.EqualFold(parts[i], "Items") && strings.EqualFold(parts[i+2], "PlaybackInfo") {
			return parts[i+1]
		}
	}
	return ""
}

func playbackCheckInKind(p string) (string, bool) {
	parts := splitPath(p)
	for i := 0; i+1 < len(parts); i++ {
		if !strings.EqualFold(parts[i], "Sessions") || !strings.EqualFold(parts[i+1], "Playing") {
			continue
		}
		if i+2 >= len(parts) {
			return "playing", true
		}
		switch {
		case strings.EqualFold(parts[i+2], "Progress"):
			return "progress", true
		case strings.EqualFold(parts[i+2], "Stopped"):
			return "stopped", true
		default:
			return "playing", true
		}
	}
	return "", false
}

func parsePlaybackEvent(body []byte) (transcode.PlaybackEvent, bool, error) {
	var raw map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return transcode.PlaybackEvent{}, false, err
		}
	}
	event := transcode.PlaybackEvent{
		ItemID:        stringField(raw, "ItemId", "ItemID", "itemId"),
		MediaSourceID: stringField(raw, "MediaSourceId", "MediaSourceID", "mediaSourceId"),
		PlaySessionID: stringField(raw, "PlaySessionId", "PlaySessionID", "playSessionId"),
		PositionTicks: int64Field(raw, "PositionTicks", "positionTicks"),
		IsPaused:      boolField(raw, "IsPaused", "isPaused"),
	}
	return event, event.ItemID != "" || event.PlaySessionID != "", nil
}

func stringField(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if text, ok := value.(string); ok {
				return text
			}
		}
	}
	return ""
}

func int64Field(raw map[string]any, keys ...string) int64 {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return int64(typed)
		case string:
			parsed, _ := strconv.ParseInt(typed, 10, 64)
			return parsed
		}
	}
	return 0
}

func boolField(raw map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			return strings.EqualFold(typed, "true")
		}
	}
	return false
}

func splitPath(p string) []string {
	raw := strings.Split(p, "/")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func readAndReplaceBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func normalizeTranscodePath(path string) (string, bool) {
	const transcodePrefix = "/streambridge/transcode/"
	if strings.HasPrefix(path, transcodePrefix) {
		return path, true
	}
	if strings.HasPrefix(path, "/emby"+transcodePrefix) {
		return strings.TrimPrefix(path, "/emby"), true
	}
	if index := strings.Index(path, transcodePrefix); index >= 0 {
		return path[index:], true
	}
	return "", false
}

func redactURLString(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.RawQuery == "" {
		return parsed.String()
	}
	values := parsed.Query()
	for key := range values {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "auth") {
			values.Set(key, "REDACTED")
		}
	}
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.cancelReaper != nil {
		s.cancelReaper()
	}
	if s.transcodeManager != nil {
		s.transcodeManager.Close()
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}
