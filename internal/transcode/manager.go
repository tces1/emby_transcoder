package transcode

import (
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
	"syscall"
	"time"

	"emby-transcoder/internal/logging"
)

var ErrTooManySessions = errors.New("too many transcode sessions")

const defaultRestartGraceTimeout = 500 * time.Millisecond

type Options struct {
	MaxSessions           int
	TempDir               string
	FFmpegPath            string
	HardwareDecode        string
	HardwareDevice        string
	BufferPauseThreshold  time.Duration
	BufferResumeThreshold time.Duration
	BufferCheckInterval   time.Duration
	IdleTimeout           time.Duration
	ReapInterval          time.Duration
	RestartGraceTimeout   time.Duration
	Runner                Runner
}

type Request struct {
	InputURL                string
	Headers                 http.Header
	ItemID                  string
	MediaSourceID           string
	PlaySessionID           string
	StartTimeTicks          int64
	RequestedStartTimeTicks int64
	SegmentStartIndex       int
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
	ID                      string
	ItemID                  string
	MediaSourceID           string
	PlaySessionID           string
	StartTimeTicks          int64
	RequestedStartTimeTicks int64
	SegmentStartIndex       int
	HighestSegmentSeen      int
	Media                   MediaInfo
	Dir                     string
	InputURL                string
	LastAccess              time.Time
	LastMediaAccess         time.Time
	LastProgress            time.Time
	PositionTicks           int64
	Paused                  bool
	bufferPaused            bool

	cancel  context.CancelFunc
	process Process
}

type Process interface {
	Stop() error
}

type Runner interface {
	Start(ctx context.Context, session *Session, request Request) (Process, error)
}

type Manager struct {
	mu       sync.Mutex
	options  Options
	sessions map[string]*Session
	media    map[string]MediaInfo
}

func NewManager(options Options) *Manager {
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
	if options.Runner == nil && options.FFmpegPath != "" {
		options.Runner = FFmpegRunner{
			Path: options.FFmpegPath,
			Options: FFmpegOptions{
				HardwareDecode: options.HardwareDecode,
				HardwareDevice: options.HardwareDevice,
			},
		}
	}
	return &Manager{options: options, sessions: map[string]*Session{}, media: map[string]MediaInfo{}}
}

type MediaInfo struct {
	ItemID        string
	SourceID      string
	Name          string
	Path          string
	Container     string
	VideoCodec    string
	Width         int
	Height        int
	AudioCodec    string
	AudioChannels int
	AudioTitle    string
	Bitrate       int64
	RunTimeTicks  int64
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
	return info == MediaInfo{}
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
	for {
		var stale *Session
		fastStop := false

		m.mu.Lock()
		now := time.Now()
		if existing, ok := m.sessions[id]; ok {
			if sessionProcessDone(existing) {
				delete(m.sessions, id)
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
		if len(m.sessions) >= m.options.MaxSessions {
			logging.Errorf("transcode limit id=%s active=%d max=%d", id, len(m.sessions), m.options.MaxSessions)
			m.mu.Unlock()
			return nil, ErrTooManySessions
		}
		if request.InputURL == "" {
			m.mu.Unlock()
			return nil, errors.New("input url is required")
		}

		dir := filepath.Join(m.options.TempDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		session := &Session{ID: id, Dir: dir, InputURL: request.InputURL, LastAccess: now, LastMediaAccess: now, cancel: cancel}
		touchSession(session, request, now, true)
		if session.Media.IsZero() {
			session.Media = m.media[id]
		}
		if session.ItemID == "" {
			session.ItemID = id
		}
		if session.MediaSourceID == "" {
			session.MediaSourceID = session.Media.SourceID
		}
		traceSwitch("manager_create id=%s item=%s media_source=%s play_session=%s start_ticks=%d segment_start=%d dir=%s input=%s media=%s", id, session.ItemID, session.MediaSourceID, session.PlaySessionID, session.StartTimeTicks, session.SegmentStartIndex, dir, redactURLString(request.InputURL), session.Media.Summary())

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

func (m *Manager) RecordProgress(event PlaybackEvent) int {
	m.mu.Lock()
	now := time.Now()
	count := 0
	var actions []bufferAction
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
		if event.PlaySessionID != "" {
			session.PlaySessionID = event.PlaySessionID
		}
		session.LastAccess = now
		session.LastProgress = now
		session.PositionTicks = event.PositionTicks
		session.Paused = event.IsPaused
		if action, ok := m.bufferActionLocked(session); ok {
			actions = append(actions, action)
		}
		count++
	}
	m.mu.Unlock()
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
		if now.Sub(sessionIdleReference(session)) > m.options.IdleTimeout {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()

	for _, id := range expired {
		_ = m.Stop(id)
	}
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
	if request.StartTimeTicks != session.StartTimeTicks {
		return true
	}
	if request.RequestedStartTimeTicks != session.RequestedStartTimeTicks {
		return true
	}
	if request.SegmentStartIndex != session.SegmentStartIndex {
		return true
	}
	return request.InputURL != "" && session.InputURL != "" && request.InputURL != session.InputURL
}

func touchSession(session *Session, request Request, now time.Time, mediaAccess bool) {
	session.LastAccess = now
	if mediaAccess {
		session.LastMediaAccess = now
	}
	if request.InputURL != "" {
		session.InputURL = request.InputURL
	}
	if request.ItemID != "" {
		session.ItemID = request.ItemID
	}
	if request.MediaSourceID != "" {
		session.MediaSourceID = request.MediaSourceID
	}
	if request.PlaySessionID != "" {
		session.PlaySessionID = request.PlaySessionID
	}
	session.StartTimeTicks = request.StartTimeTicks
	session.RequestedStartTimeTicks = request.RequestedStartTimeTicks
	session.SegmentStartIndex = request.SegmentStartIndex
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

func (m *Manager) bufferActionLocked(session *Session) (bufferAction, bool) {
	if session == nil || session.process == nil {
		return bufferAction{}, false
	}
	process, ok := session.process.(pausableProcess)
	if !ok {
		return bufferAction{}, false
	}
	baseTicks := session.StartTimeTicks
	if baseTicks < 0 {
		baseTicks = 0
	}
	generatedTicks := baseTicks
	if session.HighestSegmentSeen >= session.SegmentStartIndex {
		generatedTicks += int64(session.HighestSegmentSeen-session.SegmentStartIndex+1) * defaultSegmentTicks
	}
	playedTicks := session.PositionTicks
	if playedTicks < baseTicks {
		playedTicks = baseTicks
	}
	bufferTicks := generatedTicks - playedTicks
	if bufferTicks < 0 {
		bufferTicks = 0
	}

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
	Path    string
	Options FFmpegOptions
}

type FFmpegOptions struct {
	HardwareDecode string
	HardwareDevice string
}

func (r FFmpegRunner) Start(ctx context.Context, session *Session, request Request) (Process, error) {
	if r.Path == "" {
		return nil, errors.New("ffmpeg path is required")
	}

	args := buildFFmpegArgs(session, request, r.Options)
	playlist := filepath.Join(session.Dir, "master.m3u8")
	logPath := filepath.Join(session.Dir, "ffmpeg.log")
	logging.Infof("transcode start id=%s segment=%d", session.ID, session.SegmentStartIndex)
	logging.Debugf("ffmpeg start id=%s item=%s media_source=%s start_ticks=%d segment_start=%d path=%s input=%s playlist=%s media=%s", session.ID, session.ItemID, session.MediaSourceID, session.StartTimeTicks, session.SegmentStartIndex, r.Path, redactURLString(request.InputURL), playlist, session.Media.Summary())
	cmd := exec.CommandContext(ctx, r.Path, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open ffmpeg stdin: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open ffmpeg log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	logging.Debugf("ffmpeg started id=%s pid=%d", session.ID, cmd.Process.Pid)
	process := &execProcess{cmd: cmd, logFile: logFile, stdin: stdin, doneCh: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		process.done.Store(true)
		close(process.doneCh)
		_ = logFile.Close()
		if err != nil {
			logging.Infof("transcode exit id=%s err=%v", session.ID, err)
			logging.Debugf("ffmpeg exited id=%s err=%v log=%s", session.ID, err, logPath)
			return
		}
		logging.Infof("transcode exit id=%s", session.ID)
		logging.Debugf("ffmpeg exited id=%s err=nil log=%s", session.ID, logPath)
	}()
	return process, nil
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
		"-map", "0:a:0?",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-profile:v", "high",
		"-level", "4.1",
		"-pix_fmt", "yuv420p",
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
		"-hls_time", "1",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-start_number", strconv.Itoa(session.SegmentStartIndex),
		"-hls_segment_filename", segmentPattern,
		playlist,
	)
	return args
}

func appendHardwareDecodeArgs(args []string, options FFmpegOptions) []string {
	switch strings.ToLower(strings.TrimSpace(options.HardwareDecode)) {
	case "", "none", "off", "false":
		return args
	case "vaapi":
		args = append(args, "-hwaccel", "vaapi")
		if device := strings.TrimSpace(options.HardwareDevice); device != "" {
			args = append(args, "-hwaccel_device", device)
		}
		return args
	default:
		return args
	}
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
}

func (p *execProcess) Stop() error {
	return p.StopWithGrace(5 * time.Second)
}

func (p *execProcess) StopWithGrace(grace time.Duration) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if p.Done() {
		return nil
	}
	if p.paused.Load() && p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGCONT)
		p.paused.Store(false)
	}
	if p.stdin != nil {
		_, _ = io.WriteString(p.stdin, "q\n")
		_ = p.stdin.Close()
	}
	if p.waitForExit(grace) {
		return nil
	}
	err := p.cmd.Process.Kill()
	if p.waitForExit(2 * time.Second) {
		return nil
	}
	return err
}

func (p *execProcess) Done() bool {
	return p.done.Load()
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

func (p *execProcess) Pause() error {
	if p.cmd == nil || p.cmd.Process == nil || p.Done() || p.paused.Load() {
		return nil
	}
	if err := p.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		return err
	}
	p.paused.Store(true)
	return nil
}

func (p *execProcess) Resume() error {
	if p.cmd == nil || p.cmd.Process == nil || p.Done() || !p.paused.Load() {
		return nil
	}
	if err := p.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		return err
	}
	p.paused.Store(false)
	return nil
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
