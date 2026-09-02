package transcode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"emby-transcoder/internal/logging"
)

var ErrTooManySessions = errors.New("too many transcode sessions")

const defaultRestartGraceTimeout = 500 * time.Millisecond
const maxTranscodeWidth = 1920
const maxTranscodeHeight = 1080
const lowLatencyGOP = 25
const hlsInitTimeSeconds = 1

type Options struct {
	MaxSessions           int
	TempDir               string
	FFmpegPath            string
	HardwareDecode        string
	HardwareDevice        string
	InputProxy            InputProxy
	BufferPauseThreshold  time.Duration
	BufferResumeThreshold time.Duration
	SegmentDuration       time.Duration
	SegmentRetention      time.Duration
	BufferCheckInterval   time.Duration
	IdleTimeout           time.Duration
	ReapInterval          time.Duration
	RestartGraceTimeout   time.Duration
	Runner                Runner
	HardwareProbe         HardwareProbe
}

type Request struct {
	InputURL      string
	Headers       http.Header
	ItemID        string
	MediaSourceID string
	// PlaySessionID identifies the client playback lifecycle.
	PlaySessionID string
	// UpstreamPlaySessionID belongs to the media URL returned by Emby.
	UpstreamPlaySessionID   string
	AudioStreamIndex        int
	HasAudioStreamIndex     bool
	StartTimeTicks          int64
	RequestedStartTimeTicks int64
	SegmentStartIndex       int
	SegmentRequest          bool
	SegmentTicks            int64
	Media                   MediaInfo
}

type PlaybackEvent struct {
	ItemID        string
	MediaSourceID string
	PlaySessionID string
	PositionTicks int64
	IsPaused      bool
}

type Session struct {
	ID            string
	ItemID        string
	MediaSourceID string
	// PlaySessionID is the immutable client playback identity.
	PlaySessionID           string
	UpstreamPlaySessionID   string
	GenerationID            uint64
	AudioStreamIndex        int
	StartTimeTicks          int64
	RequestedStartTimeTicks int64
	SegmentStartIndex       int
	OldestSegmentKept       int
	HighestSegmentSeen      int
	ReadySegmentCount       int
	SegmentTicks            int64
	Media                   MediaInfo
	Dir                     string
	InputURL                string
	LastAccess              time.Time
	LastMediaAccess         time.Time
	LastProgress            time.Time
	PositionTicks           int64
	Paused                  bool
	bufferPaused            bool
	HardwarePipeline        string
	UploadedBytes           int64

	cancel  context.CancelFunc
	process Process

	uploadSampleBytes int64
	uploadSampleTime  time.Time
	uploadBPS         float64
}

type SessionStatus struct {
	ID                  string  `json:"id"`
	GenerationID        uint64  `json:"generation_id"`
	VideoName           string  `json:"video_name"`
	State               string  `json:"state"`
	HardwarePipeline    string  `json:"hardware_pipeline"`
	TranscodeSpeed      float64 `json:"transcode_speed"`
	UploadBPS           float64 `json:"upload_bps"`
	UploadedBytes       int64   `json:"uploaded_bytes"`
	PositionTicks       int64   `json:"position_ticks"`
	BufferSeconds       float64 `json:"buffer_seconds"`
	GeneratedSeconds    float64 `json:"generated_seconds"`
	RuntimeSeconds      float64 `json:"runtime_seconds"`
	BufferPauseSeconds  float64 `json:"buffer_pause_seconds"`
	BufferResumeSeconds float64 `json:"buffer_resume_seconds"`
}

type Process interface {
	Stop() error
}

type Runner interface {
	Start(ctx context.Context, session *Session, request Request) (Process, error)
}

type InputProxy interface {
	Register(rawURL string, headers http.Header) (string, func(), error)
}

type detailedInputProxy interface {
	RegisterSource(id string, name string, generation uint64, rawURL string, headers http.Header) (string, func(), error)
}

type Manager struct {
	mu                  sync.Mutex
	options             Options
	sessions            map[string]*Session
	media               map[string]MediaInfo
	vaapiEncodeFallback map[string]bool
	nextGeneration      atomic.Uint64
}

func NewManager(options Options) *Manager {
	options = normalizeManagerOptions(options)
	if options.Runner == nil && options.FFmpegPath != "" {
		ffmpegOptions := resolveHardwareDecodeOptions(options.FFmpegPath, FFmpegOptions{
			HardwareDecode: options.HardwareDecode,
			HardwareDevice: options.HardwareDevice,
		}, options.HardwareProbe)
		options.Runner = FFmpegRunner{
			Path:       options.FFmpegPath,
			Options:    ffmpegOptions,
			InputProxy: options.InputProxy,
		}
	}
	return &Manager{options: options, sessions: map[string]*Session{}, media: map[string]MediaInfo{}, vaapiEncodeFallback: map[string]bool{}}
}

func NewManagerStrict(options Options) (*Manager, error) {
	options = normalizeManagerOptions(options)
	if options.Runner == nil && options.FFmpegPath != "" {
		ffmpegOptions, err := resolveHardwareDecodeOptionsStrict(options.FFmpegPath, FFmpegOptions{
			HardwareDecode: options.HardwareDecode,
			HardwareDevice: options.HardwareDevice,
		}, options.HardwareProbe)
		if err != nil {
			return nil, err
		}
		if ffmpegOptions.HardwareDecode != "" {
			logging.Infof("hardware transcode enabled pipeline=%s", ffmpegOptionsSummary(ffmpegOptions))
		}
		options.Runner = FFmpegRunner{
			Path:       options.FFmpegPath,
			Options:    ffmpegOptions,
			InputProxy: options.InputProxy,
		}
	}
	return &Manager{options: options, sessions: map[string]*Session{}, media: map[string]MediaInfo{}, vaapiEncodeFallback: map[string]bool{}}, nil
}

func normalizeManagerOptions(options Options) Options {
	if options.MaxSessions <= 0 {
		options.MaxSessions = 2
	}
	if options.TempDir == "" {
		options.TempDir = filepath.Join(os.TempDir(), "emby-transcoder")
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = 60 * time.Second
	}
	if options.BufferPauseThreshold <= 0 {
		options.BufferPauseThreshold = 5 * time.Minute
	}
	if options.BufferResumeThreshold <= 0 {
		options.BufferResumeThreshold = 2 * time.Minute
	}
	if options.SegmentDuration <= 0 {
		options.SegmentDuration = 2 * time.Second
	}
	if options.SegmentRetention <= 0 {
		options.SegmentRetention = 5 * time.Minute
	}
	if options.BufferCheckInterval <= 0 {
		options.BufferCheckInterval = time.Second
	}
	if options.ReapInterval <= 0 {
		options.ReapInterval = options.IdleTimeout / 2
	}
	if options.ReapInterval <= 0 {
		options.ReapInterval = time.Second
	}
	if options.RestartGraceTimeout <= 0 {
		options.RestartGraceTimeout = defaultRestartGraceTimeout
	}
	options.HardwareDecode = strings.ToLower(strings.TrimSpace(options.HardwareDecode))
	options.HardwareDevice = strings.TrimSpace(options.HardwareDevice)
	return options
}

func (m *Manager) segmentTicks() int64 {
	if m == nil {
		return defaultSegmentTicks
	}
	return segmentTicksFromDuration(m.options.SegmentDuration)
}

func segmentTicksFromDuration(duration time.Duration) int64 {
	ticks := durationToTicks(duration)
	if ticks <= 0 {
		return defaultSegmentTicks
	}
	return ticks
}

func sessionSegmentTicks(session *Session) int64 {
	if session == nil || session.SegmentTicks <= 0 {
		return defaultSegmentTicks
	}
	return session.SegmentTicks
}

func hlsTimeValue(segmentTicks int64) string {
	if segmentTicks <= 0 {
		segmentTicks = defaultSegmentTicks
	}
	if segmentTicks%timeSecondTicks == 0 {
		return strconv.FormatInt(segmentTicks/timeSecondTicks, 10)
	}
	return strconv.FormatFloat(ticksFloatSeconds(segmentTicks), 'f', 3, 64)
}

type MediaInfo struct {
	ItemID        string
	SourceID      string
	InputURL      string
	Name          string
	Path          string
	Container     string
	VideoCodec    string
	VideoProfile  string
	VideoPixFmt   string
	VideoBitDepth int
	Width         int
	Height        int
	AudioCodec    string
	AudioChannels int
	AudioTitle    string
	AudioStreams  []AudioStreamInfo
	Bitrate       int64
	RunTimeTicks  int64
}

type AudioStreamInfo struct {
	Index    int
	Ordinal  int
	Codec    string
	Channels int
	Title    string
}

func (info MediaInfo) Summary() string {
	var parts []string
	if info.Name != "" {
		parts = append(parts, fmt.Sprintf("name=%q", info.Name))
	}
	if info.Path != "" {
		parts = append(parts, fmt.Sprintf("path=%q", info.Path))
	}
	if info.Container != "" {
		parts = append(parts, "container="+info.Container)
	}
	if info.VideoCodec != "" || info.Width > 0 || info.Height > 0 {
		video := strings.TrimSpace(info.VideoCodec)
		if video == "" {
			video = "unknown"
		}
		if info.VideoProfile != "" {
			video += " " + info.VideoProfile
		}
		if info.VideoBitDepth > 0 {
			video += fmt.Sprintf(" %dbit", info.VideoBitDepth)
		}
		if info.VideoPixFmt != "" {
			video += " " + info.VideoPixFmt
		}
		if info.Width > 0 || info.Height > 0 {
			video += fmt.Sprintf(" %dx%d", info.Width, info.Height)
		}
		parts = append(parts, "video="+video)
	}
	if info.AudioCodec != "" || info.AudioChannels > 0 || info.AudioTitle != "" {
		audio := strings.TrimSpace(info.AudioCodec)
		if audio == "" {
			audio = "unknown"
		}
		if info.AudioTitle != "" {
			audio += fmt.Sprintf(" %q", info.AudioTitle)
		}
		if info.AudioChannels > 0 {
			audio += fmt.Sprintf(" channels=%d", info.AudioChannels)
		}
		parts = append(parts, "audio="+audio)
	}
	if len(info.AudioStreams) > 0 {
		parts = append(parts, fmt.Sprintf("audio_streams=%d", len(info.AudioStreams)))
	}
	if info.Bitrate > 0 {
		parts = append(parts, fmt.Sprintf("bitrate=%d", info.Bitrate))
	}
	if info.RunTimeTicks > 0 {
		parts = append(parts, "runtime="+formatTicks(info.RunTimeTicks))
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, " ")
}

func (info MediaInfo) IsZero() bool {
	return info.ItemID == "" &&
		info.SourceID == "" &&
		info.InputURL == "" &&
		info.Name == "" &&
		info.Path == "" &&
		info.Container == "" &&
		info.VideoCodec == "" &&
		info.VideoProfile == "" &&
		info.VideoPixFmt == "" &&
		info.VideoBitDepth == 0 &&
		info.Width == 0 &&
		info.Height == 0 &&
		info.AudioCodec == "" &&
		info.AudioChannels == 0 &&
		info.AudioTitle == "" &&
		len(info.AudioStreams) == 0 &&
		info.Bitrate == 0 &&
		info.RunTimeTicks == 0
}

func (m *Manager) RememberMedia(id string, info MediaInfo) {
	if strings.TrimSpace(id) == "" || info.IsZero() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if info.ItemID == "" {
		info.ItemID = id
	}
	m.media[id] = info
	if session, ok := m.sessions[id]; ok {
		session.Media = info
		if session.MediaSourceID == "" {
			session.MediaSourceID = info.SourceID
		}
		if session.ItemID == "" {
			session.ItemID = info.ItemID
		}
	}
}

func (m *Manager) MediaInfo(id string) (MediaInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, ok := m.media[id]
	return info, ok
}

func (m *Manager) Ensure(id string, request Request) (*Session, error) {
	request = m.normalizedRequest(request)
	for {
		var stale *Session
		fastStop := false

		m.mu.Lock()
		now := time.Now()
		if existing, ok := m.sessions[id]; ok {
			if sessionProcessDone(existing) {
				delete(m.sessions, id)
				if shouldFallbackToVAAPIEncode(existing, m.options.Runner) {
					m.vaapiEncodeFallback[id] = true
					logging.Infof("hardware transcode fallback id=%s from=vaapi-full to=vaapi-encode reason=process_done", id)
				}
				stale = existing
				traceSwitch("manager_restart_done id=%s input=%s", id, redactURLString(existing.InputURL))
			} else if shouldRestart(existing, request) {
				delete(m.sessions, id)
				stale = existing
				fastStop = true
				traceSwitch("manager_restart id=%s old_input=%s new_input=%s old_start_ticks=%d new_start_ticks=%d old_segment_start=%d new_segment_start=%d", id, redactURLString(existing.InputURL), redactURLString(request.InputURL), existing.StartTimeTicks, request.StartTimeTicks, existing.SegmentStartIndex, request.SegmentStartIndex)
			} else {
				touchSession(existing, request, now, true)
				traceSwitch("manager_reuse id=%s dir=%s start_ticks=%d segment_start=%d", id, existing.Dir, existing.StartTimeTicks, existing.SegmentStartIndex)
				m.mu.Unlock()
				return existing, nil
			}
		}
		if stale != nil {
			m.mu.Unlock()
			if fastStop {
				_ = stopSessionWithGrace(stale, m.options.RestartGraceTimeout)
			} else {
				_ = stopSession(stale)
			}
			continue
		}
		done := m.removeDoneSessionsLocked(id)
		if len(done) > 0 {
			m.mu.Unlock()
			for _, session := range done {
				_ = stopSession(session)
			}
			continue
		}
		if len(m.sessions) >= m.options.MaxSessions {
			logging.Errorf("transcode limit id=%s active=%d max=%d", id, len(m.sessions), m.options.MaxSessions)
			m.mu.Unlock()
			return nil, ErrTooManySessions
		}
		if request.InputURL == "" {
			m.mu.Unlock()
			return nil, errors.New("input url is required")
		}

		generation := m.nextGeneration.Add(1)
		dir := filepath.Join(m.options.TempDir, fmt.Sprintf("%s-g%d", id, generation))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		session := &Session{
			ID:               id,
			GenerationID:     generation,
			Dir:              dir,
			InputURL:         request.InputURL,
			LastAccess:       now,
			LastMediaAccess:  now,
			uploadSampleTime: now,
			cancel:           cancel,
		}
		if runner, ok := m.options.Runner.(FFmpegRunner); ok {
			session.HardwarePipeline = effectiveFFmpegOptions(session, runner.Options).HardwarePipeline
		}
		session.SegmentTicks = m.segmentTicks()
		touchSession(session, request, now, true)
		if session.Media.IsZero() {
			session.Media = m.media[id]
		}
		session.HardwarePipeline = selectHardwarePipeline(session.Media, session.HardwarePipeline)
		if m.vaapiEncodeFallback[id] {
			session.HardwarePipeline = "vaapi-encode"
		}
		if session.ItemID == "" {
			session.ItemID = id
		}
		if session.MediaSourceID == "" {
			session.MediaSourceID = session.Media.SourceID
		}
		traceSwitch("manager_create id=%s generation=%d item=%s media_source=%s client_play_session=%s upstream_play_session=%s start_ticks=%d segment_start=%d dir=%s input=%s media=%s", id, session.GenerationID, session.ItemID, session.MediaSourceID, session.PlaySessionID, session.UpstreamPlaySessionID, session.StartTimeTicks, session.SegmentStartIndex, dir, redactURLString(request.InputURL), session.Media.Summary())

		if m.options.Runner != nil {
			process, err := m.options.Runner.Start(ctx, session, request)
			if err != nil {
				cancel()
				traceSwitch("manager_runner_error id=%s err=%v", id, err)
				m.mu.Unlock()
				return nil, err
			}
			session.process = process
		}

		m.sessions[id] = session
		m.mu.Unlock()
		return session, nil
	}
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if ok {
		now := time.Now()
		session.LastAccess = now
		session.LastMediaAccess = now
	}
	return session, ok
}

func (m *Manager) PlaylistWindow(id string) (start, ready int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return 0, 0
	}
	refreshReadySegments(session)
	return session.SegmentStartIndex, session.ReadySegmentCount
}

func (m *Manager) RecordSessionUpload(session *Session, bytes int64) {
	if session == nil || bytes <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.sessions[session.ID]; ok && current == session {
		current.UploadedBytes += bytes
	}
}

func (m *Manager) StatusSnapshot() []SessionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	statuses := make([]SessionStatus, 0, len(m.sessions))
	for _, session := range m.sessions {
		elapsed := now.Sub(session.uploadSampleTime).Seconds()
		if session.uploadSampleTime.IsZero() {
			elapsed = now.Sub(session.LastAccess).Seconds()
		}
		if elapsed >= 0.5 {
			delta := session.UploadedBytes - session.uploadSampleBytes
			if delta < 0 {
				delta = 0
			}
			session.uploadBPS = float64(delta) / elapsed
			session.uploadSampleBytes = session.UploadedBytes
			session.uploadSampleTime = now
		}
		state := "running"
		if session.bufferPaused {
			state = "paused"
		} else if sessionProcessDone(session) {
			state = "exited"
		}
		name := session.Media.Name
		if name == "" {
			name = session.ItemID
		}
		speed := 0.0
		if state == "running" {
			process, ok := session.process.(interface{ TranscodeSpeed() float64 })
			if ok {
				speed = process.TranscodeSpeed()
			}
		}
		generated, _, buffered := sessionBufferTicks(session)
		statuses = append(statuses, SessionStatus{
			ID:                  session.ID,
			GenerationID:        session.GenerationID,
			VideoName:           name,
			State:               state,
			HardwarePipeline:    session.HardwarePipeline,
			TranscodeSpeed:      speed,
			UploadBPS:           session.uploadBPS,
			UploadedBytes:       session.UploadedBytes,
			PositionTicks:       session.PositionTicks,
			BufferSeconds:       ticksFloatSeconds(buffered),
			GeneratedSeconds:    ticksFloatSeconds(generated),
			RuntimeSeconds:      ticksFloatSeconds(session.Media.RunTimeTicks),
			BufferPauseSeconds:  m.options.BufferPauseThreshold.Seconds(),
			BufferResumeSeconds: m.options.BufferResumeThreshold.Seconds(),
		})
	}
	return statuses
}

func (m *Manager) RecordSegmentRequest(id string, segmentIndex int) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	session.LastAccess = now
	session.LastMediaAccess = now
	if segmentIndex > session.HighestSegmentSeen {
		session.HighestSegmentSeen = segmentIndex
	}
	action, ok := m.bufferActionLocked(session)
	m.mu.Unlock()
	if ok {
		m.applyBufferAction(action)
	}
}

func (m *Manager) RecordSegmentReady(id string, segmentIndex int) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	if segmentIndex >= session.SegmentStartIndex {
		refreshReadySegments(session)
	}
	action, ok := m.bufferActionLocked(session)
	m.mu.Unlock()
	if ok {
		m.applyBufferAction(action)
	}
}

func (m *Manager) RecordProgress(event PlaybackEvent) int {
	m.mu.Lock()
	now := time.Now()
	count := 0
	var actions []bufferAction
	var cleanups []segmentCleanupTask
	for _, session := range m.sessions {
		if !matchesPlayback(session, event) {
			continue
		}
		if event.ItemID != "" {
			session.ItemID = event.ItemID
		}
		if event.MediaSourceID != "" {
			session.MediaSourceID = event.MediaSourceID
		}
		if event.PlaySessionID != "" && session.PlaySessionID == "" {
			session.PlaySessionID = event.PlaySessionID
		}
		session.LastAccess = now
		session.LastProgress = now
		session.PositionTicks = event.PositionTicks
		session.Paused = event.IsPaused
		if action, ok := m.bufferActionLocked(session); ok {
			actions = append(actions, action)
		}
		if cleanup, ok := m.segmentCleanupTaskLocked(session); ok {
			cleanups = append(cleanups, cleanup)
		}
		count++
	}
	m.mu.Unlock()
	for _, cleanup := range cleanups {
		cleanupOldSegments(cleanup)
	}
	for _, action := range actions {
		m.applyBufferAction(action)
	}
	return count
}

func (m *Manager) StopPlayback(event PlaybackEvent) int {
	m.mu.Lock()
	var sessions []*Session
	for id, session := range m.sessions {
		if matchesPlayback(session, event) {
			delete(m.sessions, id)
			sessions = append(sessions, session)
		}
	}
	m.mu.Unlock()

	for _, session := range sessions {
		logging.Infof("transcode stop playback id=%s", session.ID)
		_ = stopSession(session)
	}
	return len(sessions)
}

func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	logging.Infof("transcode stop id=%s", id)
	return stopSession(session)
}

func (m *Manager) Close() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for id, session := range m.sessions {
		delete(m.sessions, id)
		sessions = append(sessions, session)
	}
	m.mu.Unlock()

	for _, session := range sessions {
		_ = stopSession(session)
	}
}

func (m *Manager) StartReaper(ctx context.Context) {
	reapTicker := time.NewTicker(m.options.ReapInterval)
	bufferTicker := time.NewTicker(m.options.BufferCheckInterval)
	defer reapTicker.Stop()
	defer bufferTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reapTicker.C:
			m.ReapIdle()
		case <-bufferTicker.C:
			m.ReconcileBuffers()
		}
	}
}

func (m *Manager) ReapIdle() {
	m.mu.Lock()
	now := time.Now()
	var expired []string
	for id, session := range m.sessions {
		if sessionProcessDone(session) || now.Sub(sessionIdleReference(session)) > m.options.IdleTimeout {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()

	for _, id := range expired {
		_ = m.Stop(id)
	}
}

func (m *Manager) removeDoneSessionsLocked(exceptID string) []*Session {
	var done []*Session
	for id, session := range m.sessions {
		if id == exceptID || !sessionProcessDone(session) {
			continue
		}
		delete(m.sessions, id)
		done = append(done, session)
		logging.Infof("transcode reap done id=%s", id)
	}
	return done
}

func (m *Manager) ReconcileBuffers() {
	m.mu.Lock()
	var actions []bufferAction
	for _, session := range m.sessions {
		if action, ok := m.bufferActionLocked(session); ok {
			actions = append(actions, action)
		}
	}
	m.mu.Unlock()

	for _, action := range actions {
		m.applyBufferAction(action)
	}
}

func shouldRestart(session *Session, request Request) bool {
	if request.SegmentRequest && session.OldestSegmentKept > session.SegmentStartIndex && request.SegmentStartIndex < session.OldestSegmentKept {
		return true
	}
	if request.PlaySessionID != "" && session.PlaySessionID != "" && request.PlaySessionID != session.PlaySessionID {
		return true
	}
	if request.AudioStreamIndex != session.AudioStreamIndex {
		return true
	}
	if request.InputURL != "" && session.InputURL != "" && !segmentInputCompatible(session.InputURL, request.InputURL) {
		return true
	}
	if !request.SegmentRequest {
		return playlistSeekChanged(session, request)
	}
	if request.StartTimeTicks != session.StartTimeTicks {
		return true
	}
	if request.RequestedStartTimeTicks != session.RequestedStartTimeTicks {
		return true
	}
	if request.SegmentStartIndex != session.SegmentStartIndex {
		return true
	}
	return false
}

func playlistSeekChanged(session *Session, request Request) bool {
	requested := request.RequestedStartTimeTicks
	if requested == 0 {
		requested = request.StartTimeTicks
	}
	current := session.RequestedStartTimeTicks
	if current == 0 {
		current = session.StartTimeTicks
	}
	return requested != current
}

func (m *Manager) normalizedRequest(request Request) Request {
	if request.RequestedStartTimeTicks == 0 {
		request.RequestedStartTimeTicks = request.StartTimeTicks
	}
	if request.SegmentRequest || request.SegmentStartIndex != 0 || request.StartTimeTicks <= 0 {
		return request
	}
	segmentTicks := m.segmentTicks()
	request.SegmentStartIndex = int(request.StartTimeTicks / segmentTicks)
	request.StartTimeTicks = int64(request.SegmentStartIndex) * segmentTicks
	return request
}

func touchSession(session *Session, request Request, now time.Time, mediaAccess bool) {
	session.LastAccess = now
	if mediaAccess {
		session.LastMediaAccess = now
	}
	if request.InputURL != "" && session.InputURL == "" {
		session.InputURL = request.InputURL
	}
	if request.ItemID != "" {
		session.ItemID = request.ItemID
	}
	if request.MediaSourceID != "" {
		session.MediaSourceID = request.MediaSourceID
	}
	if request.PlaySessionID != "" && session.PlaySessionID == "" {
		session.PlaySessionID = request.PlaySessionID
	}
	if request.UpstreamPlaySessionID != "" && session.UpstreamPlaySessionID == "" {
		session.UpstreamPlaySessionID = request.UpstreamPlaySessionID
	}
	session.AudioStreamIndex = request.AudioStreamIndex
	session.StartTimeTicks = request.StartTimeTicks
	session.RequestedStartTimeTicks = request.RequestedStartTimeTicks
	session.SegmentStartIndex = request.SegmentStartIndex
	if request.SegmentTicks > 0 {
		session.SegmentTicks = request.SegmentTicks
	}
	if session.SegmentTicks <= 0 {
		session.SegmentTicks = defaultSegmentTicks
	}
	if session.OldestSegmentKept < request.SegmentStartIndex {
		session.OldestSegmentKept = request.SegmentStartIndex
	}
	if session.HighestSegmentSeen < request.SegmentStartIndex {
		session.HighestSegmentSeen = request.SegmentStartIndex
	}
	if !request.Media.IsZero() {
		session.Media = request.Media
	}
}

type pausableProcess interface {
	Pause() error
	Resume() error
}

type bufferAction struct {
	sessionID   string
	process     pausableProcess
	pause       bool
	bufferTicks int64
}

type segmentCleanupTask struct {
	sessionID     string
	dir           string
	beforeSegment int
}

func (m *Manager) segmentCleanupTaskLocked(session *Session) (segmentCleanupTask, bool) {
	if session == nil || session.Dir == "" || m.options.SegmentRetention <= 0 {
		return segmentCleanupTask{}, false
	}
	retentionTicks := durationToTicks(m.options.SegmentRetention)
	if retentionTicks <= 0 || session.PositionTicks <= retentionTicks {
		return segmentCleanupTask{}, false
	}
	cutoffTicks := session.PositionTicks - retentionTicks
	beforeSegment := int(cutoffTicks / sessionSegmentTicks(session))
	if beforeSegment <= session.OldestSegmentKept {
		return segmentCleanupTask{}, false
	}
	session.OldestSegmentKept = beforeSegment
	return segmentCleanupTask{sessionID: session.ID, dir: session.Dir, beforeSegment: beforeSegment}, true
}

func cleanupOldSegments(task segmentCleanupTask) {
	entries, err := os.ReadDir(task.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logging.Errorf("transcode cleanup error id=%s err=%v", task.sessionID, err)
		}
		return
	}

	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		segmentIndex, ok := segmentIndexFromName(entry.Name())
		if !ok || segmentIndex >= task.beforeSegment {
			continue
		}
		path := filepath.Join(task.dir, entry.Name())
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				logging.Errorf("transcode cleanup error id=%s file=%s err=%v", task.sessionID, entry.Name(), err)
			}
			continue
		}
		deleted++
	}
	if deleted > 0 {
		logging.Infof("transcode cleanup id=%s deleted_segments=%d before_segment=%d", task.sessionID, deleted, task.beforeSegment)
	}
}

func (m *Manager) bufferActionLocked(session *Session) (bufferAction, bool) {
	if session == nil || session.process == nil {
		return bufferAction{}, false
	}
	process, ok := session.process.(pausableProcess)
	if !ok {
		return bufferAction{}, false
	}
	_, _, bufferTicks := sessionBufferTicks(session)

	if session.bufferPaused {
		if bufferTicks < durationToTicks(m.options.BufferResumeThreshold) {
			session.bufferPaused = false
			return bufferAction{sessionID: session.ID, process: process, bufferTicks: bufferTicks}, true
		}
		return bufferAction{}, false
	}
	if bufferTicks > durationToTicks(m.options.BufferPauseThreshold) {
		session.bufferPaused = true
		return bufferAction{sessionID: session.ID, process: process, pause: true, bufferTicks: bufferTicks}, true
	}
	return bufferAction{}, false
}

func sessionBufferTicks(session *Session) (generatedTicks, playedTicks, bufferTicks int64) {
	if session == nil {
		return 0, 0, 0
	}
	refreshReadySegments(session)
	baseTicks := session.StartTimeTicks
	if baseTicks < 0 {
		baseTicks = 0
	}
	generatedTicks = baseTicks + int64(session.ReadySegmentCount)*sessionSegmentTicks(session)
	playedTicks = session.PositionTicks
	if playedTicks < baseTicks {
		playedTicks = baseTicks
	}
	if requestTicks := int64(session.HighestSegmentSeen) * sessionSegmentTicks(session); requestTicks > playedTicks {
		playedTicks = requestTicks
	}
	bufferTicks = generatedTicks - playedTicks
	if bufferTicks < 0 {
		bufferTicks = 0
	}
	return generatedTicks, playedTicks, bufferTicks
}

const maxReadySegmentScan = 256

func sessionSegmentPath(session *Session, index int) string {
	return filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", index))
}

func refreshReadySegments(session *Session) {
	if session == nil || session.Dir == "" {
		return
	}
	next := session.SegmentStartIndex + session.ReadySegmentCount
	for scanned := 0; scanned < maxReadySegmentScan; scanned++ {
		if !fileExists(sessionSegmentPath(session, next)) {
			return
		}
		session.ReadySegmentCount++
		next++
	}
}

func (m *Manager) applyBufferAction(action bufferAction) {
	if action.process == nil {
		return
	}
	if action.pause {
		if err := action.process.Pause(); err != nil {
			logging.Errorf("transcode buffer pause error id=%s err=%v", action.sessionID, err)
			return
		}
		logging.Infof("transcode buffer pause id=%s buffered=%s", action.sessionID, formatTicks(action.bufferTicks))
		return
	}
	if err := action.process.Resume(); err != nil {
		logging.Errorf("transcode buffer resume error id=%s err=%v", action.sessionID, err)
		return
	}
	logging.Infof("transcode buffer resume id=%s buffered=%s", action.sessionID, formatTicks(action.bufferTicks))
}

func matchesPlayback(session *Session, event PlaybackEvent) bool {
	if event.PlaySessionID != "" && session.PlaySessionID != "" && event.PlaySessionID == session.PlaySessionID {
		return true
	}
	if event.ItemID == "" {
		return false
	}
	if event.ItemID != session.ID && event.ItemID != session.ItemID {
		return false
	}
	if event.MediaSourceID != "" && session.MediaSourceID != "" && event.MediaSourceID != session.MediaSourceID {
		return false
	}
	return true
}

func sessionIdleReference(session *Session) time.Time {
	if session.Paused && !session.LastMediaAccess.IsZero() {
		return session.LastMediaAccess
	}
	ref := session.LastAccess
	if session.LastMediaAccess.After(ref) {
		ref = session.LastMediaAccess
	}
	if session.LastProgress.After(ref) {
		ref = session.LastProgress
	}
	return ref
}

func stopSession(session *Session) error {
	return stopSessionWithGrace(session, 5*time.Second)
}

type gracefulProcess interface {
	StopWithGrace(time.Duration) error
}

func stopSessionWithGrace(session *Session, grace time.Duration) error {
	var err error
	if session.process != nil {
		if process, ok := session.process.(gracefulProcess); ok {
			err = process.StopWithGrace(grace)
		} else {
			err = session.process.Stop()
		}
	}
	if session.cancel != nil {
		session.cancel()
	}
	if session.Dir != "" {
		if removeErr := os.RemoveAll(session.Dir); err == nil {
			err = removeErr
		}
	}
	return err
}

type doneAwareProcess interface {
	Done() bool
}

func sessionProcessDone(session *Session) bool {
	if session == nil || session.process == nil {
		return false
	}
	process, ok := session.process.(doneAwareProcess)
	return ok && process.Done()
}

func shouldFallbackToVAAPIEncode(session *Session, runner Runner) bool {
	if session == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(session.HardwarePipeline), "vaapi-encode") {
		return false
	}
	ffmpegRunner, ok := runner.(FFmpegRunner)
	if !ok {
		return false
	}
	return hardwarePipeline(ffmpegRunner.Options) == "vaapi-full"
}

func formatTicks(ticks int64) string {
	if ticks <= 0 {
		return "0s"
	}
	return (time.Duration(ticks) * 100).String()
}

func durationToTicks(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Nanoseconds() / 100
}

type FFmpegRunner struct {
	Path       string
	Options    FFmpegOptions
	InputProxy InputProxy
}

type FFmpegOptions struct {
	HardwareDecode   string
	HardwareDevice   string
	HardwarePipeline string
	InputBitDepth    int
}

type HardwareProbe func(ffmpegPath string, options FFmpegOptions) error

func resolveHardwareDecodeOptions(ffmpegPath string, options FFmpegOptions, probe HardwareProbe) FFmpegOptions {
	resolved, err := resolveHardwareDecodeOptionsStrict(ffmpegPath, options, probe)
	if err != nil {
		logging.Infof("hardware transcode unavailable mode=%s device=%s reason=%v fallback=software", options.HardwareDecode, options.HardwareDevice, err)
		return FFmpegOptions{}
	}
	logging.Infof("hardware transcode enabled pipeline=%s", ffmpegOptionsSummary(resolved))
	return resolved
}

func resolveHardwareDecodeOptionsStrict(ffmpegPath string, options FFmpegOptions, probe HardwareProbe) (FFmpegOptions, error) {
	options.HardwareDecode = strings.ToLower(strings.TrimSpace(options.HardwareDecode))
	options.HardwareDevice = strings.TrimSpace(options.HardwareDevice)
	switch options.HardwareDecode {
	case "", "none", "off", "false":
		return FFmpegOptions{}, nil
	case "vaapi":
		if strings.TrimSpace(ffmpegPath) == "" {
			return FFmpegOptions{}, errors.New("ffmpeg path is required for hardware decode")
		}
		if options.HardwareDevice == "" {
			options.HardwareDevice = "/dev/dri/renderD128"
		}
	default:
		return FFmpegOptions{}, fmt.Errorf("hardware decode unavailable mode=%s reason=unsupported", options.HardwareDecode)
	}

	if probe == nil {
		if err := probeVAAPIBase(ffmpegPath, options); err != nil {
			return FFmpegOptions{}, fmt.Errorf("hardware decode unavailable mode=%s device=%s: %w", options.HardwareDecode, options.HardwareDevice, err)
		}
	} else if err := probe(ffmpegPath, options); err != nil {
		return FFmpegOptions{}, fmt.Errorf("hardware decode unavailable mode=%s device=%s: %w", options.HardwareDecode, options.HardwareDevice, err)
	}
	options.HardwarePipeline = "vaapi-full"
	return options, nil
}

func resolveDefaultHardwareDecodeOptions(ffmpegPath string, options FFmpegOptions) (FFmpegOptions, error) {
	switch options.HardwareDecode {
	case "vaapi":
		if err := probeVAAPIBase(ffmpegPath, options); err != nil {
			return FFmpegOptions{}, fmt.Errorf("hardware decode unavailable mode=%s device=%s: %w", options.HardwareDecode, options.HardwareDevice, err)
		}
		options.HardwarePipeline = "vaapi-full"
		return options, nil
	default:
		return FFmpegOptions{}, fmt.Errorf("unsupported hardware decode mode %q", options.HardwareDecode)
	}
}

func defaultHardwareProbe(ffmpegPath string, options FFmpegOptions) error {
	if strings.TrimSpace(ffmpegPath) == "" {
		return errors.New("ffmpeg path is required")
	}
	switch options.HardwareDecode {
	case "vaapi":
		return probeVAAPIBase(ffmpegPath, options)
	default:
		return fmt.Errorf("unsupported hardware decode mode %q", options.HardwareDecode)
	}
}

func probeVAAPIBase(ffmpegPath string, options FFmpegOptions) error {
	if err := probeFFmpegHWAccel(ffmpegPath, "vaapi"); err != nil {
		return err
	}
	if err := probeFFmpegEncoder(ffmpegPath, "h264_vaapi"); err != nil {
		return err
	}
	if err := probeDevice(options.HardwareDevice); err != nil {
		return err
	}
	return probeFFmpegVAAPIDeviceInit(ffmpegPath, options.HardwareDevice)
}

func probeFFmpegHWAccel(ffmpegPath, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-hwaccels").CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("ffmpeg hwaccel probe timed out")
	}
	if err != nil {
		return fmt.Errorf("ffmpeg hwaccel probe failed: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), name) {
			return nil
		}
	}
	return fmt.Errorf("ffmpeg does not list %s hwaccel", name)
}

func probeFFmpegEncoder(ffmpegPath, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders").CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("ffmpeg encoder probe timed out")
	}
	if err != nil {
		return fmt.Errorf("ffmpeg encoder probe failed: %w", err)
	}
	if ffmpegListContains(output, name) {
		return nil
	}
	return fmt.Errorf("ffmpeg does not list %s encoder", name)
}

func probeFFmpegFilter(ffmpegPath, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-filters").CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("ffmpeg filter probe timed out")
	}
	if err != nil {
		return fmt.Errorf("ffmpeg filter probe failed: %w", err)
	}
	if ffmpegListContains(output, name) {
		return nil
	}
	return fmt.Errorf("ffmpeg does not list %s filter", name)
}

func ffmpegListContains(output []byte, name string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(name)) {
			return true
		}
	}
	return false
}

func probeFFmpegVAAPIDeviceInit(ffmpegPath, device string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-init_hw_device", "vaapi=probe:"+device,
		"-f", "lavfi",
		"-i", "nullsrc=s=16x16:d=0.1",
		"-frames:v", "1",
		"-f", "null",
		"-",
	).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("vaapi device init probe timed out")
	}
	if err != nil {
		if message := strings.TrimSpace(string(output)); message != "" {
			return fmt.Errorf("vaapi device init probe failed: %w: %s", err, message)
		}
		return fmt.Errorf("vaapi device init probe failed: %w", err)
	}
	return nil
}

func probeFFmpegVAAPIFullPipeline(ffmpegPath, device string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-init_hw_device", "vaapi=probe:"+device,
		"-filter_hw_device", "probe",
		"-f", "lavfi",
		"-i", "nullsrc=s=64x64:d=0.1",
		"-vf", "format=nv12,hwupload,"+vaapiScaleFilter(64, 64),
		"-frames:v", "1",
		"-c:v", "h264_vaapi",
		"-f", "null",
		"-",
	).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("vaapi full pipeline probe timed out")
	}
	if err != nil {
		if message := strings.TrimSpace(string(output)); message != "" {
			return fmt.Errorf("vaapi full pipeline probe failed: %w: %s", err, message)
		}
		return fmt.Errorf("vaapi full pipeline probe failed: %w", err)
	}
	return nil
}

func probeFFmpegVAAPIEncodePipeline(ffmpegPath, device string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-vaapi_device", device,
		"-f", "lavfi",
		"-i", "nullsrc=s=64x64:d=0.1",
		"-vf", softwareScaleFilter(64, 64)+",format=nv12,hwupload",
		"-frames:v", "1",
		"-c:v", "h264_vaapi",
		"-f", "null",
		"-",
	).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("vaapi encode pipeline probe timed out")
	}
	if err != nil {
		if message := strings.TrimSpace(string(output)); message != "" {
			return fmt.Errorf("vaapi encode pipeline probe failed: %w: %s", err, message)
		}
		return fmt.Errorf("vaapi encode pipeline probe failed: %w", err)
	}
	return nil
}

func probeDevice(device string) error {
	if strings.TrimSpace(device) == "" {
		return errors.New("hardware device is required")
	}
	file, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open hardware device: %w", err)
	}
	return file.Close()
}

func (r FFmpegRunner) Start(ctx context.Context, session *Session, request Request) (Process, error) {
	if r.Path == "" {
		return nil, errors.New("ffmpeg path is required")
	}

	options := effectiveFFmpegOptions(session, r.Options)
	ffmpegRequest := request
	releaseInput := func() {}
	if r.InputProxy != nil {
		var localURL string
		var release func()
		var err error
		if detailed, ok := r.InputProxy.(detailedInputProxy); ok {
			localURL, release, err = detailed.RegisterSource(session.ID, session.Media.Name, session.GenerationID, request.InputURL, request.Headers)
		} else {
			localURL, release, err = r.InputProxy.Register(request.InputURL, request.Headers)
		}
		if err != nil {
			logging.Errorf("accelerated input unavailable id=%s err=%v fallback=direct", session.ID, err)
		} else {
			ffmpegRequest.InputURL = localURL
			ffmpegRequest.Headers = nil
			releaseInput = release
			logging.Infof("accelerated input enabled id=%s source=%s", session.ID, redactURLString(request.InputURL))
		}
	}
	args := buildFFmpegArgs(session, ffmpegRequest, options)
	playlist := filepath.Join(session.Dir, "master.m3u8")
	logPath := filepath.Join(session.Dir, "ffmpeg.log")
	logging.Infof("transcode start id=%s segment=%d decode=%s audio_stream_index=%d audio_map=%s audio=optional-aac log=%s", session.ID, session.SegmentStartIndex, ffmpegOptionsSummary(options), request.AudioStreamIndex, audioMapArg(session, request), logPath)
	logging.Debugf("ffmpeg start id=%s item=%s media_source=%s start_ticks=%d segment_start=%d path=%s input=%s playlist=%s media=%s args=%s", session.ID, session.ItemID, session.MediaSourceID, session.StartTimeTicks, session.SegmentStartIndex, r.Path, redactURLString(request.InputURL), playlist, session.Media.Summary(), redactFFmpegArgs(args))
	cmd := exec.CommandContext(ctx, r.Path, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		releaseInput()
		return nil, fmt.Errorf("open ffmpeg stdin: %w", err)
	}
	progressOutput, err := cmd.StdoutPipe()
	if err != nil {
		releaseInput()
		return nil, fmt.Errorf("open ffmpeg progress output: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		releaseInput()
		return nil, fmt.Errorf("open ffmpeg log: %w", err)
	}
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		releaseInput()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	logging.Infof("ffmpeg started id=%s pid=%d decode=%s", session.ID, cmd.Process.Pid, ffmpegOptionsSummary(options))
	process := &execProcess{cmd: cmd, logFile: logFile, stdin: stdin, doneCh: make(chan struct{})}
	go process.readProgress(progressOutput)
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		releaseInput()
		if err != nil {
			logging.Infof("transcode exit id=%s err=%v", session.ID, err)
			if archivedPath, archiveErr := archiveFailedTranscodeLog(session, logPath); archiveErr != nil {
				logging.Errorf("transcode log archive failed id=%s err=%v", session.ID, archiveErr)
			} else {
				logging.Infof("transcode log archived id=%s path=%s", session.ID, archivedPath)
			}
			logging.Debugf("ffmpeg exited id=%s err=%v log=%s", session.ID, err, logPath)
			process.done.Store(true)
			close(process.doneCh)
			return
		}
		logging.Infof("transcode exit id=%s", session.ID)
		logging.Debugf("ffmpeg exited id=%s err=nil log=%s", session.ID, logPath)
		process.done.Store(true)
		close(process.doneCh)
	}()
	return process, nil
}

func effectiveFFmpegOptions(session *Session, options FFmpegOptions) FFmpegOptions {
	if session == nil {
		return options
	}
	if pipeline := strings.TrimSpace(session.HardwarePipeline); pipeline != "" {
		options.HardwarePipeline = pipeline
	}
	if session.Media.VideoBitDepth > 0 {
		options.InputBitDepth = session.Media.VideoBitDepth
	}
	return options
}

func selectHardwarePipeline(info MediaInfo, current string) string {
	pipeline := strings.ToLower(strings.TrimSpace(current))
	if pipeline != "vaapi-full" {
		return current
	}
	if needsVAAPIEncodeFallback(info) {
		return "vaapi-encode"
	}
	if needsVAAPIHybridPipeline(info) {
		return "vaapi-hybrid"
	}
	return current
}

func needsVAAPIHybridPipeline(info MediaInfo) bool {
	codec := strings.ToLower(strings.TrimSpace(info.VideoCodec))
	profile := strings.ToLower(strings.TrimSpace(info.VideoProfile))
	pixFmt := strings.ToLower(strings.TrimSpace(info.VideoPixFmt))
	if codec != "hevc" && codec != "h265" {
		return false
	}
	if info.VideoBitDepth > 8 {
		return true
	}
	if strings.Contains(profile, "10") || strings.Contains(pixFmt, "p10") || strings.Contains(pixFmt, "p010") {
		return true
	}
	return false
}

func needsVAAPIEncodeFallback(info MediaInfo) bool {
	codec := strings.ToLower(strings.TrimSpace(info.VideoCodec))
	if codec != "hevc" && codec != "h265" {
		return false
	}
	if needsVAAPIHybridPipeline(info) {
		return false
	}
	return info.Width > maxTranscodeWidth || info.Height > maxTranscodeHeight
}

func archiveFailedTranscodeLog(session *Session, logPath string) (string, error) {
	if session == nil {
		return "", errors.New("session is required")
	}
	if strings.TrimSpace(logPath) == "" {
		return "", errors.New("log path is required")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "", err
	}
	logsDir := filepath.Join(filepath.Dir(session.Dir), "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return "", err
	}
	archivedPath := filepath.Join(logsDir, fmt.Sprintf("%s-%s.log", session.ID, time.Now().UTC().Format("20060102T150405.000Z")))
	if err := os.WriteFile(archivedPath, data, 0o644); err != nil {
		return "", err
	}
	return archivedPath, nil
}

func ffmpegOptionsSummary(options FFmpegOptions) string {
	mode := strings.ToLower(strings.TrimSpace(options.HardwareDecode))
	if mode == "" || mode == "none" || mode == "off" || mode == "false" {
		return "software"
	}
	if pipeline := strings.TrimSpace(options.HardwarePipeline); pipeline != "" {
		mode = pipeline
	} else if mode == "vaapi" {
		mode = "vaapi-full"
	}
	device := strings.TrimSpace(options.HardwareDevice)
	if device == "" {
		return mode
	}
	return mode + ":" + device
}

func redactFFmpegArgs(args []string) string {
	redacted := append([]string(nil), args...)
	for i := 0; i < len(redacted)-1; i++ {
		switch redacted[i] {
		case "-i":
			redacted[i+1] = redactURLString(redacted[i+1])
		case "-headers":
			redacted[i+1] = "REDACTED"
		}
	}
	return strings.Join(redacted, " ")
}

func buildFFmpegArgs(session *Session, request Request, options ...FFmpegOptions) []string {
	playlist := filepath.Join(session.Dir, "master.m3u8")
	segmentPattern := filepath.Join(session.Dir, "segment_%05d.ts")
	ffmpegOptions := FFmpegOptions{}
	if len(options) > 0 {
		ffmpegOptions = options[0]
	}
	args := []string{
		"-hide_banner",
		"-loglevel", "info",
		"-progress", "pipe:1",
		"-nostats",
	}
	if headerText := ffmpegHeaders(request.Headers); headerText != "" {
		args = append(args, "-headers", headerText)
	}
	args = appendHardwareDecodeArgs(args, ffmpegOptions)
	if session.StartTimeTicks > 0 {
		args = append(args, "-ss", ticksSeconds(session.StartTimeTicks))
	}
	args = append(args,
		"-i", request.InputURL,
		"-map", "0:v:0",
		"-map", audioMapArg(session, request),
	)
	args = appendVideoTranscodeArgs(args, ffmpegOptions)
	args = append(args,
		"-c:a", "aac",
		"-b:a", "160k",
		"-ac", "2",
		"-muxdelay", "0",
		"-muxpreload", "0",
	)
	if session.StartTimeTicks > 0 {
		args = append(args, "-output_ts_offset", ticksSeconds(session.StartTimeTicks))
	}
	args = append(args,
		"-f", "hls",
		"-hls_init_time", strconv.Itoa(hlsInitTimeSeconds),
		"-hls_time", hlsTimeValue(sessionSegmentTicks(session)),
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-start_number", strconv.Itoa(session.SegmentStartIndex),
		"-hls_segment_filename", segmentPattern,
		playlist,
	)
	return args
}

func appendVideoTranscodeArgs(args []string, options FFmpegOptions) []string {
	switch hardwarePipeline(options) {
	case "vaapi-full":
		return append(args,
			"-vf", vaapiFormatFilter(),
			"-c:v", "h264_vaapi",
			"-low_power", "1",
			"-g", strconv.Itoa(lowLatencyGOP),
			"-keyint_min", strconv.Itoa(lowLatencyGOP),
			"-bf", "0",
			"-qp", "23",
		)
	case "vaapi-hybrid":
		return append(args,
			"-vf", vaapiHybridFilter(options),
			"-c:v", "h264_vaapi",
			"-low_power", "1",
			"-level", "4.1",
			"-g", strconv.Itoa(lowLatencyGOP),
			"-keyint_min", strconv.Itoa(lowLatencyGOP),
			"-bf", "0",
			"-qp", "23",
		)
	case "vaapi-encode":
		return append(args,
			"-vf", softwareScaleFilter(maxTranscodeWidth, maxTranscodeHeight)+",format=nv12,hwupload",
			"-c:v", "h264_vaapi",
			"-low_power", "1",
			"-level", "4.1",
			"-g", strconv.Itoa(lowLatencyGOP),
			"-keyint_min", strconv.Itoa(lowLatencyGOP),
			"-bf", "0",
			"-qp", "23",
		)
	}
	return append(args,
		"-vf", softwareScaleFilter(maxTranscodeWidth, maxTranscodeHeight),
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-profile:v", "high",
		"-level", "4.1",
		"-g", strconv.Itoa(lowLatencyGOP),
		"-keyint_min", strconv.Itoa(lowLatencyGOP),
		"-sc_threshold", "0",
		"-bf", "0",
		"-pix_fmt", "yuv420p",
	)
}

func vaapiScaleFilter(width, height int) string {
	return fmt.Sprintf("scale_vaapi=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2:format=nv12", width, height)
}

func vaapiFormatFilter() string {
	return "scale_vaapi=format=nv12"
}

func softwareScaleFilter(width, height int) string {
	return fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2", width, height)
}

func vaapiHybridFilter(options FFmpegOptions) string {
	downloadFormat := "nv12"
	if options.InputBitDepth > 8 {
		downloadFormat = "p010le"
	}
	return "hwdownload,format=" + downloadFormat + "," + softwareScaleFilter(maxTranscodeWidth, maxTranscodeHeight) + ",format=nv12,hwupload"
}

func audioMapArg(session *Session, request Request) string {
	if (request.HasAudioStreamIndex || request.AudioStreamIndex != 0) && session != nil {
		for _, audio := range session.Media.AudioStreams {
			if audio.Index == request.AudioStreamIndex {
				return fmt.Sprintf("0:a:%d?", audio.Ordinal)
			}
		}
	}
	return "0:a:0?"
}

func appendHardwareDecodeArgs(args []string, options FFmpegOptions) []string {
	switch hardwarePipeline(options) {
	case "vaapi-full", "vaapi-hybrid":
		args = append(args, "-hwaccel", "vaapi")
		if device := strings.TrimSpace(options.HardwareDevice); device != "" {
			args = append(args, "-hwaccel_device", device)
		}
		args = append(args, "-hwaccel_output_format", "vaapi")
		return args
	case "vaapi-encode":
		if device := strings.TrimSpace(options.HardwareDevice); device != "" {
			args = append(args, "-vaapi_device", device)
		}
		return args
	default:
		return args
	}
}

func isVAAPITranscode(options FFmpegOptions) bool {
	return strings.EqualFold(strings.TrimSpace(options.HardwareDecode), "vaapi")
}

func hardwarePipeline(options FFmpegOptions) string {
	if !isVAAPITranscode(options) {
		return ""
	}
	if pipeline := strings.ToLower(strings.TrimSpace(options.HardwarePipeline)); pipeline != "" {
		return pipeline
	}
	return "vaapi-full"
}

func ticksSeconds(ticks int64) string {
	return strconv.FormatFloat(float64(ticks)/10_000_000, 'f', 6, 64)
}

type execProcess struct {
	cmd     *exec.Cmd
	logFile *os.File
	stdin   io.WriteCloser
	doneCh  chan struct{}
	done    atomic.Bool
	paused  atomic.Bool
	speed   atomic.Int64
}

func (p *execProcess) Stop() error {
	return p.StopWithGrace(5 * time.Second)
}

func (p *execProcess) Done() bool {
	return p.done.Load()
}

func (p *execProcess) TranscodeSpeed() float64 {
	return float64(p.speed.Load()) / 1000
}

func (p *execProcess) readProgress(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok || key != "speed" {
			continue
		}
		value = strings.TrimSuffix(strings.TrimSpace(value), "x")
		speed, err := strconv.ParseFloat(value, 64)
		if err != nil || speed < 0 {
			continue
		}
		p.speed.Store(int64(speed * 1000))
	}
}

func (p *execProcess) waitForExit(timeout time.Duration) bool {
	if p.Done() {
		return true
	}
	if p.doneCh == nil {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.doneCh:
		return true
	case <-timer.C:
		return p.Done()
	}
}

func ffmpegHeaders(headers http.Header) string {
	var lines []string
	for _, key := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "User-Agent"} {
		value := headers.Get(key)
		if value != "" {
			lines = append(lines, key+": "+value)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\r\n") + "\r\n"
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
