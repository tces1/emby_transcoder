package transcode_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"emby-transcoder/internal/transcode"
)

func TestManagerRejectsWhenSessionLimitReached(t *testing.T) {
	m := transcode.NewManager(transcode.Options{MaxSessions: 1, TempDir: t.TempDir()})
	t.Cleanup(m.Close)

	_, err := m.Ensure("one", transcode.Request{InputURL: "http://upstream/one"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.Ensure("two", transcode.Request{InputURL: "http://upstream/two"})
	if !errors.Is(err, transcode.ErrTooManySessions) {
		t.Fatalf("err = %v", err)
	}
}

func TestNewManagerStrictFailsWhenHardwareProbeFails(t *testing.T) {
	_, err := transcode.NewManagerStrict(transcode.Options{
		FFmpegPath:     "/usr/bin/ffmpeg",
		HardwareDecode: "vaapi",
		HardwareDevice: "/dev/dri/renderD128",
		HardwareProbe: func(string, transcode.FFmpegOptions) error {
			return errors.New("device not accessible")
		},
	})

	if err == nil {
		t.Fatal("expected hardware probe failure to stop startup")
	}
	if !strings.Contains(err.Error(), "hardware decode unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagerReusesExistingSession(t *testing.T) {
	m := transcode.NewManager(transcode.Options{MaxSessions: 1, TempDir: t.TempDir()})
	t.Cleanup(m.Close)

	first, err := m.Ensure("one", transcode.Request{InputURL: "http://upstream/one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("one", transcode.Request{InputURL: "http://upstream/one"})
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Fatal("expected same session pointer")
	}
	if first.Dir == "" || filepath.Base(first.Dir) != "one-g1" || first.GenerationID != 1 {
		t.Fatalf("session dir = %q", first.Dir)
	}
}

func TestManagerStopsIdleSessions(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions:  1,
		TempDir:      t.TempDir(),
		IdleTimeout:  20 * time.Millisecond,
		ReapInterval: 5 * time.Millisecond,
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	session, err := m.Ensure("one", transcode.Request{InputURL: "http://upstream/one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session.Dir); err != nil {
		t.Fatal(err)
	}

	m.ReapIdle()

	if stopped.Load() != 0 {
		t.Fatal("fresh session should not be stopped")
	}

	time.Sleep(35 * time.Millisecond)
	m.ReapIdle()

	if stopped.Load() != 1 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
	if _, err := os.Stat(session.Dir); !os.IsNotExist(err) {
		t.Fatalf("expected session dir removed, err=%v", err)
	}
}

func TestManagerRestartsSessionWhenInputURLChanges(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream?MediaSourceId=source1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream?MediaSourceId=source2"})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("expected a new session when the input URL changes")
	}
	if second.InputURL != "http://upstream/stream?MediaSourceId=source2" {
		t.Fatalf("input url = %q", second.InputURL)
	}
	if stopped.Load() != 1 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
}

func TestManagerReusesSessionWhenOnlyUpstreamPlaySessionIdChanges(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{
		InputURL:              "https://tv.example/videos/item123/original.mp4?DeviceId=dev1&MediaSourceId=source1&PlaySessionId=session-a&api_key=secret",
		UpstreamPlaySessionID: "session-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{
		InputURL:              "https://tv.example/videos/item123/original.mp4?DeviceId=dev1&MediaSourceId=source1&PlaySessionId=session-b&api_key=secret",
		UpstreamPlaySessionID: "session-b",
	})
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Fatal("expected the same session when only PlaySessionId changes")
	}
	if stopped.Load() != 0 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
	if second.UpstreamPlaySessionID != "session-a" {
		t.Fatalf("running upstream play session changed to %q", second.UpstreamPlaySessionID)
	}
}

func TestManagerRestartsWhenClientPlaySessionChanges(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", PlaySessionID: "client-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", PlaySessionID: "client-b"})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("expected a new transcode generation for a different client play session")
	}
	if first.GenerationID == second.GenerationID {
		t.Fatalf("generation id was reused: %d", second.GenerationID)
	}
	if second.PlaySessionID != "client-b" {
		t.Fatalf("client play session = %q", second.PlaySessionID)
	}
	if stopped.Load() != 1 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
}

func TestManagerRestartsSessionWhenStartTimeTicksChanges(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", StartTimeTicks: 0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", StartTimeTicks: 900000000})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("expected a new session when only StartTimeTicks changes")
	}
	if second.StartTimeTicks != 900000000 {
		t.Fatalf("start ticks = %d", second.StartTimeTicks)
	}
	if stopped.Load() != 1 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
}

func TestManagerRestartsSessionWhenSegmentStartIndexChanges(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream?StartTimeTicks=0", SegmentStartIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream?StartTimeTicks=40000000", StartTimeTicks: 40000000, SegmentStartIndex: 1})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("expected a new session when segment start index changes")
	}
	if second.SegmentStartIndex != 1 {
		t.Fatalf("segment start index = %d", second.SegmentStartIndex)
	}
	if stopped.Load() != 1 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
}

func TestManagerRestartsSessionWhenAudioStreamIndexChanges(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", AudioStreamIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", AudioStreamIndex: 2})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("expected a new session when audio stream index changes")
	}
	if second.AudioStreamIndex != 2 {
		t.Fatalf("audio stream index = %d", second.AudioStreamIndex)
	}
	if stopped.Load() != 1 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
}

func TestManagerRestartsSessionWhenRequestedStartTimeTicksChangesWithinSameSegment(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{
		InputURL:                "http://upstream/stream?StartTimeTicks=5300000000",
		StartTimeTicks:          5300000000,
		RequestedStartTimeTicks: 5350000000,
		SegmentStartIndex:       133,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{
		InputURL:                "http://upstream/stream?StartTimeTicks=5300000000",
		StartTimeTicks:          5300000000,
		RequestedStartTimeTicks: 5390000000,
		SegmentStartIndex:       133,
	})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("expected a new session when requested seek time changes within the same segment")
	}
	if second.StartTimeTicks != 5300000000 {
		t.Fatalf("start ticks = %d", second.StartTimeTicks)
	}
	if stopped.Load() != 1 {
		t.Fatalf("stopped = %d", stopped.Load())
	}
}

func TestManagerPlaylistSeekStartsAtSegmentAndSurvivesReload(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			return noopProcess{}, nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{
		InputURL:                "http://upstream/stream",
		StartTimeTicks:          6418677540,
		RequestedStartTimeTicks: 6418677540,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.SegmentStartIndex != 320 || first.StartTimeTicks != 6400000000 {
		t.Fatalf("playlist seek window = segment %d ticks %d", first.SegmentStartIndex, first.StartTimeTicks)
	}

	reload, err := m.Ensure("item123", transcode.Request{
		InputURL:                "http://upstream/stream",
		StartTimeTicks:          6418677540,
		RequestedStartTimeTicks: 6418677540,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reload != first {
		t.Fatal("playlist reload restarted the seeked session")
	}

	segment, err := m.Ensure("item123", transcode.Request{
		InputURL:                "http://upstream/stream",
		StartTimeTicks:          6400000000,
		RequestedStartTimeTicks: 6418677540,
		SegmentStartIndex:       320,
		SegmentRequest:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if segment != first {
		t.Fatal("segment request restarted the playlist session")
	}
	if starts.Load() != 1 {
		t.Fatalf("starts = %d", starts.Load())
	}
}

func TestManagerRestartsSessionWhenProcessAlreadyExited(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			return doneProcess{}, nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", SegmentStartIndex: 32})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", SegmentStartIndex: 32})
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("expected a new session when the old process has exited")
	}
	if starts.Load() != 2 {
		t.Fatalf("starts = %d", starts.Load())
	}
}

func TestManagerRecordsPlaybackProgress(t *testing.T) {
	m := transcode.NewManager(transcode.Options{MaxSessions: 1, TempDir: t.TempDir()})
	t.Cleanup(m.Close)

	session, err := m.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream/stream",
		MediaSourceID: "source1",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	updated := m.RecordProgress(transcode.PlaybackEvent{
		ItemID:        "item123",
		MediaSourceID: "source1",
		PlaySessionID: "play-session-1",
		PositionTicks: 450000000,
		IsPaused:      true,
	})

	if updated != 1 {
		t.Fatalf("updated = %d", updated)
	}
	if session.PositionTicks != 450000000 {
		t.Fatalf("position ticks = %d", session.PositionTicks)
	}
	if !session.Paused {
		t.Fatal("expected paused state to be recorded")
	}
}

func TestManagerDeletesOldSegmentsBehindPlaybackPosition(t *testing.T) {
	tempDir := t.TempDir()
	m := transcode.NewManager(transcode.Options{
		MaxSessions:      1,
		TempDir:          tempDir,
		SegmentRetention: 3 * time.Second,
	})
	t.Cleanup(m.Close)

	session, err := m.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream/stream",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for segment := 0; segment <= 8; segment++ {
		path := filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", segment))
		if err := os.WriteFile(path, []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(session.Dir, "master.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.Dir, "ffmpeg.log"), []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.RecordProgress(transcode.PlaybackEvent{
		ItemID:        "item123",
		PlaySessionID: "play-session-1",
		PositionTicks: 7 * 10_000_000,
	})

	for _, segment := range []int{0, 1} {
		path := filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", segment))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected segment %d to be deleted, err=%v", segment, err)
		}
	}
	for _, segment := range []int{2, 3, 4, 5, 6, 7, 8} {
		path := filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", segment))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected segment %d to be kept: %v", segment, err)
		}
	}
	for _, name := range []string{"master.m3u8", "ffmpeg.log"} {
		if _, err := os.Stat(filepath.Join(session.Dir, name)); err != nil {
			t.Fatalf("expected %s to be kept: %v", name, err)
		}
	}
}

func TestManagerDoesNotRestartPlaylistRequestAfterSegmentsWerePruned(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions:      1,
		TempDir:          t.TempDir(),
		SegmentRetention: 3 * time.Second,
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			return noopProcess{}, nil
		}),
	})
	t.Cleanup(m.Close)

	first, err := m.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream/stream",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	m.RecordProgress(transcode.PlaybackEvent{
		ItemID:        "item123",
		PlaySessionID: "play-session-1",
		PositionTicks: 7 * 10_000_000,
	})

	second, err := m.Ensure("item123", transcode.Request{
		InputURL: "http://upstream/stream",
	})
	if err != nil {
		t.Fatal(err)
	}

	if second != first {
		t.Fatal("expected playlist-style request to reuse the active session")
	}
	if starts.Load() != 1 {
		t.Fatalf("starts = %d", starts.Load())
	}
}

func TestManagerPausesAndResumesBufferedProcess(t *testing.T) {
	process := &pausingProcess{}
	m := transcode.NewManager(transcode.Options{
		MaxSessions:           1,
		TempDir:               t.TempDir(),
		BufferPauseThreshold:  5 * time.Second,
		BufferResumeThreshold: 2 * time.Second,
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return process, nil
		}),
	})
	t.Cleanup(m.Close)

	session, err := m.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream/stream",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	m.RecordProgress(transcode.PlaybackEvent{
		ItemID:        "item123",
		PlaySessionID: "play-session-1",
		PositionTicks: 0,
	})
	writeReadySegments(t, session.Dir, 0, 5)
	m.RecordSegmentRequest("item123", 0)

	if process.pauseCount.Load() != 1 {
		t.Fatalf("pause count = %d", process.pauseCount.Load())
	}
	if !process.paused.Load() {
		t.Fatal("expected process to be paused")
	}

	m.RecordProgress(transcode.PlaybackEvent{
		ItemID:        "item123",
		PlaySessionID: "play-session-1",
		PositionTicks: 11 * 10_000_000,
	})

	if process.resumeCount.Load() != 1 {
		t.Fatalf("resume count = %d", process.resumeCount.Load())
	}
	if process.paused.Load() {
		t.Fatal("expected process to be resumed")
	}
}

func TestManagerDoesNotPauseForRequestedButUnreadySegments(t *testing.T) {
	process := &pausingProcess{}
	m := transcode.NewManager(transcode.Options{
		MaxSessions:           1,
		TempDir:               t.TempDir(),
		BufferPauseThreshold:  5 * time.Second,
		BufferResumeThreshold: 2 * time.Second,
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return process, nil
		}),
	})
	t.Cleanup(m.Close)

	_, err := m.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream/stream",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	for segment := 0; segment <= 5; segment++ {
		m.RecordSegmentRequest("item123", segment)
	}

	if process.pauseCount.Load() != 0 {
		t.Fatalf("pause count = %d", process.pauseCount.Load())
	}
	if process.paused.Load() {
		t.Fatal("expected in-flight segments not to count as buffer")
	}
}

func TestManagerDoesNotPauseImmediatelyForSeekedSession(t *testing.T) {
	process := &pausingProcess{}
	m := transcode.NewManager(transcode.Options{
		MaxSessions:           1,
		TempDir:               t.TempDir(),
		BufferPauseThreshold:  5 * time.Second,
		BufferResumeThreshold: 2 * time.Second,
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return process, nil
		}),
	})
	t.Cleanup(m.Close)

	_, err := m.Ensure("item123", transcode.Request{
		InputURL:          "http://upstream/stream?StartTimeTicks=4380000000",
		StartTimeTicks:    4380000000,
		SegmentStartIndex: 438,
	})
	if err != nil {
		t.Fatal(err)
	}

	m.RecordProgress(transcode.PlaybackEvent{
		ItemID:        "item123",
		PlaySessionID: "",
		PositionTicks: 0,
	})
	m.RecordSegmentRequest("item123", 438)

	if process.pauseCount.Load() != 0 {
		t.Fatalf("pause count = %d", process.pauseCount.Load())
	}
	if process.paused.Load() {
		t.Fatal("expected seeked session to keep running")
	}
}

func TestManagerUsesRememberedMediaInfoForNewSession(t *testing.T) {
	var captured transcode.MediaInfo
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			captured = session.Media
			return noopProcess{}, nil
		}),
	})
	t.Cleanup(m.Close)

	m.RememberMedia("item123", transcode.MediaInfo{
		ItemID:       "item123",
		SourceID:     "source1",
		Name:         "4K - 80 Mbps",
		Path:         "/media/Movie.mkv",
		Container:    "mkv",
		VideoCodec:   "hevc",
		Width:        3840,
		Height:       2160,
		AudioCodec:   "dts",
		AudioTitle:   "DTS 5.1",
		Bitrate:      80000000,
		RunTimeTicks: 72000000000,
	})

	session, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream?StartTimeTicks=900000000"})
	if err != nil {
		t.Fatal(err)
	}

	if session.Media.Name != "4K - 80 Mbps" {
		t.Fatalf("session media = %+v", session.Media)
	}
	if captured.VideoCodec != "hevc" || captured.Width != 3840 || captured.Height != 2160 {
		t.Fatalf("captured media = %+v", captured)
	}
	if got := captured.Summary(); !strings.Contains(got, "name=\"4K - 80 Mbps\"") || !strings.Contains(got, "video=hevc 3840x2160") {
		t.Fatalf("summary = %q", got)
	}
}

func TestManagerStopsMatchingPlaybackSessions(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 2,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	_, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/item123", PlaySessionID: "play-session-1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Ensure("other", transcode.Request{InputURL: "http://upstream/other", PlaySessionID: "play-session-2"})
	if err != nil {
		t.Fatal(err)
	}

	count := m.StopPlayback(transcode.PlaybackEvent{ItemID: "item123"})

	if count != 1 {
		t.Fatalf("stopped sessions = %d", count)
	}
	if stopped.Load() != 1 {
		t.Fatalf("process stop count = %d", stopped.Load())
	}
	if _, ok := m.Get("item123"); ok {
		t.Fatal("expected item123 session to be removed")
	}
	if _, ok := m.Get("other"); !ok {
		t.Fatal("expected other session to remain")
	}
}

func TestManagerStopsPlaybackByItemWhenPlaySessionDiffers(t *testing.T) {
	var stopped atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 2,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			return stopFunc(func() error {
				stopped.Add(1)
				return nil
			}), nil
		}),
	})
	t.Cleanup(m.Close)

	_, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/item123", ItemID: "item123", PlaySessionID: "prewarm-session"})
	if err != nil {
		t.Fatal(err)
	}

	count := m.StopPlayback(transcode.PlaybackEvent{ItemID: "item123", PlaySessionID: "emby-play-session"})

	if count != 1 {
		t.Fatalf("stopped sessions = %d", count)
	}
	if stopped.Load() != 1 {
		t.Fatalf("process stop count = %d", stopped.Load())
	}
	if _, ok := m.Get("item123"); ok {
		t.Fatal("expected item session to be removed")
	}
}

func TestManagerReapsDoneSessionsBeforeLimit(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 2,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			if starts.Add(1) == 1 {
				return doneProcess{}, nil
			}
			return noopProcess{}, nil
		}),
	})
	t.Cleanup(m.Close)

	if _, err := m.Ensure("done", transcode.Request{InputURL: "http://upstream/done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure("active", transcode.Request{InputURL: "http://upstream/active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure("new", transcode.Request{InputURL: "http://upstream/new"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("done"); ok {
		t.Fatal("expected done session to be reaped")
	}
	if _, ok := m.Get("active"); !ok {
		t.Fatal("expected active session to remain")
	}
	if _, ok := m.Get("new"); !ok {
		t.Fatal("expected new session to start")
	}
}

func TestHandlerStartsSessionAndServesPlaylist(t *testing.T) {
	var capturedInput string
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			capturedInput = request.InputURL
			return noopProcess{}, os.WriteFile(filepath.Join(session.Dir, "master.m3u8"), []byte("#EXTM3U\n"), 0o644)
		}),
	})
	t.Cleanup(m.Close)

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "http://upstream.local/emby/Videos/" + id + "/stream?" + r.URL.RawQuery
		},
		StartupWait: 50,
	}

	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?X-Emby-Token=abc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "#EXTM3U\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if capturedInput != "http://upstream.local/emby/Videos/item123/stream?X-Emby-Token=abc" {
		t.Fatalf("input = %q", capturedInput)
	}
}

func TestHandlerServesEmptyGrowingPlaylistWhenDurationIsKnown(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			return noopProcess{}, nil
		}),
	})
	m.RememberMedia("item123", transcode.MediaInfo{RunTimeTicks: 10_500_0000})
	t.Cleanup(m.Close)

	handler := transcode.Handler{Manager: m}
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?X-Emby-Token=abc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if starts.Load() != 0 {
		t.Fatalf("ffmpeg starts = %d", starts.Load())
	}
	if !strings.Contains(rec.Body.String(), "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "segment_") || strings.Contains(rec.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("unready playlist advertised media: %s", rec.Body.String())
	}
}

func TestHandlerStartsTranscodeBeforeServingGrowingPlaylist(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			go func() {
				time.Sleep(25 * time.Millisecond)
				_ = os.WriteFile(filepath.Join(session.Dir, "segment_00000.ts"), []byte("ts"), 0o644)
			}()
			return noopProcess{}, nil
		}),
	})
	m.RememberMedia("item123", transcode.MediaInfo{RunTimeTicks: 10_500_0000})
	t.Cleanup(m.Close)

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "http://upstream.local/emby/Videos/" + id + "/stream?" + r.URL.RawQuery
		},
		StartupWait: time.Second,
	}
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?X-Emby-Token=abc", nil)
	rec := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if starts.Load() != 1 {
		t.Fatalf("ffmpeg starts = %d", starts.Load())
	}
	if time.Since(started) < 20*time.Millisecond {
		t.Fatal("playlist returned before the first segment was ready")
	}
	if !strings.Contains(rec.Body.String(), "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "segment_00000.ts") {
		t.Fatalf("ready first segment was not advertised: %s", rec.Body.String())
	}
}

func TestHandlerStartsNewGenerationWhenClientPlaySessionIdChanges(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			return noopProcess{}, os.WriteFile(filepath.Join(session.Dir, "segment_00000.ts"), []byte("ts"), 0o644)
		}),
	})
	m.RememberMedia("item123", transcode.MediaInfo{RunTimeTicks: 10_500_0000})
	t.Cleanup(m.Close)

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "https://tv.example/videos/" + id + "/original.mp4?MediaSourceId=source1&PlaySessionId=" + r.URL.Query().Get("PlaySessionId")
		},
	}

	first := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?PlaySessionId=session-a", nil)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)

	second := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?PlaySessionId=session-b", nil)
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, second)

	if firstRec.Code != http.StatusOK || secondRec.Code != http.StatusOK {
		t.Fatalf("status first=%d second=%d", firstRec.Code, secondRec.Code)
	}
	if starts.Load() != 2 {
		t.Fatalf("ffmpeg starts = %d", starts.Load())
	}
}

func TestHandlerVirtualPlaylistSeekDoesNotRestartOnSegmentAndReload(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			name := fmt.Sprintf("segment_%05d.ts", request.SegmentStartIndex)
			return noopProcess{}, os.WriteFile(filepath.Join(session.Dir, name), []byte("ts"), 0o644)
		}),
	})
	m.RememberMedia("item123", transcode.MediaInfo{RunTimeTicks: 3600 * 10_000_000})
	t.Cleanup(m.Close)

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "http://upstream.local/emby/Videos/" + id + "/stream?" + r.URL.RawQuery
		},
		StartupWait: 50 * time.Millisecond,
	}

	playlist := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?StartTimeTicks=6418677540", nil)
	playlistRec := httptest.NewRecorder()
	handler.ServeHTTP(playlistRec, playlist)
	if playlistRec.Code != http.StatusOK {
		t.Fatalf("playlist status = %d body=%s", playlistRec.Code, playlistRec.Body.String())
	}
	if !strings.Contains(playlistRec.Body.String(), "#EXT-X-MEDIA-SEQUENCE:320") ||
		!strings.Contains(playlistRec.Body.String(), "segment_00320.ts") {
		t.Fatalf("seek playlist = %s", playlistRec.Body.String())
	}

	segment := httptest.NewRequest("GET", "/streambridge/transcode/item123/segment_00320.ts?StartTimeTicks=6418677540&runtimeTicks=6400000000", nil)
	segmentRec := httptest.NewRecorder()
	handler.ServeHTTP(segmentRec, segment)
	if segmentRec.Code != http.StatusOK {
		t.Fatalf("segment status = %d body=%s", segmentRec.Code, segmentRec.Body.String())
	}

	reload := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?StartTimeTicks=6418677540", nil)
	reloadRec := httptest.NewRecorder()
	handler.ServeHTTP(reloadRec, reload)
	if reloadRec.Code != http.StatusOK {
		t.Fatalf("reload status = %d body=%s", reloadRec.Code, reloadRec.Body.String())
	}
	if starts.Load() != 1 {
		t.Fatalf("ffmpeg starts = %d", starts.Load())
	}
}

func TestHandlerStartsSegmentSessionFromRequestedSegmentIndex(t *testing.T) {
	var captured transcode.Request
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			captured = request
			return noopProcess{}, os.WriteFile(filepath.Join(session.Dir, "segment_00003.ts"), []byte("ts"), 0o644)
		}),
	})
	m.RememberMedia("item123", transcode.MediaInfo{RunTimeTicks: 120_000_0000})
	t.Cleanup(m.Close)

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "http://upstream.local/emby/Videos/" + id + "/stream?" + r.URL.RawQuery
		},
		StartupWait: 50 * time.Millisecond,
	}

	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/segment_00003.ts?X-Emby-Token=abc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if captured.StartTimeTicks != 60_000_000 {
		t.Fatalf("start ticks = %d", captured.StartTimeTicks)
	}
	if captured.SegmentStartIndex != 3 {
		t.Fatalf("segment start index = %d", captured.SegmentStartIndex)
	}
	if !strings.Contains(captured.InputURL, "StartTimeTicks=60000000") {
		t.Fatalf("input url = %s", captured.InputURL)
	}
}

func TestHandlerRestartsReusableSegmentSessionWhenInputURLChanges(t *testing.T) {
	var starts atomic.Int32
	var captured transcode.Request
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			start := starts.Add(1)
			captured = request
			if start >= 2 {
				return noopProcess{}, os.WriteFile(filepath.Join(session.Dir, "segment_00032.ts"), []byte("ts"), 0o644)
			}
			return noopProcess{}, nil
		}),
	})
	m.RememberMedia("item123", transcode.MediaInfo{RunTimeTicks: 120_000_0000})
	t.Cleanup(m.Close)

	_, err := m.Ensure("item123", transcode.Request{
		InputURL:          "http://upstream.local/emby/Videos/item123/stream?StartTimeTicks=1280000000",
		StartTimeTicks:    1280000000,
		SegmentStartIndex: 32,
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "http://upstream.local/emby/Videos/" + id + "/stream?" + r.URL.RawQuery
		},
		StartupWait: 50 * time.Millisecond,
	}

	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/segment_00032.ts?MediaSourceId=source1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if starts.Load() != 2 {
		t.Fatalf("starts = %d", starts.Load())
	}
	if !strings.Contains(captured.InputURL, "MediaSourceId=source1") {
		t.Fatalf("input url = %s", captured.InputURL)
	}
}

func TestHandlerKeepsSequentialSegmentSessionPastInitialWindow(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			return noopProcess{}, nil
		}),
	})
	t.Cleanup(m.Close)

	_, err := m.Ensure("item123", transcode.Request{
		InputURL:          "http://upstream.local/emby/Videos/item123/stream?StartTimeTicks=14560000000",
		StartTimeTicks:    14560000000,
		SegmentStartIndex: 364,
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "http://upstream.local/emby/Videos/" + id + "/stream?" + r.URL.RawQuery
		},
		StartupWait: time.Nanosecond,
	}

	for segment := 365; segment <= 377; segment++ {
		runtimeTicks := int64(segment) * 10_000_000
		path := fmt.Sprintf("/streambridge/transcode/item123/segment_%05d.ts?runtimeTicks=%d", segment, runtimeTicks)
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("segment %d status = %d body = %s", segment, rec.Code, rec.Body.String())
		}
		if starts.Load() != 1 {
			t.Fatalf("segment %d unexpectedly restarted session; starts = %d", segment, starts.Load())
		}
	}
}

func TestHandlerRestartsWhenRequestedSegmentWasPruned(t *testing.T) {
	var starts atomic.Int32
	m := transcode.NewManager(transcode.Options{
		MaxSessions:      1,
		TempDir:          t.TempDir(),
		SegmentRetention: 3 * time.Second,
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			starts.Add(1)
			return noopProcess{}, os.WriteFile(filepath.Join(session.Dir, "segment_00000.ts"), []byte("ts"), 0o644)
		}),
	})
	t.Cleanup(m.Close)

	session, err := m.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream.local/emby/Videos/item123/stream",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for segment := 1; segment <= 8; segment++ {
		path := filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", segment))
		if err := os.WriteFile(path, []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m.RecordProgress(transcode.PlaybackEvent{
		ItemID:        "item123",
		PlaySessionID: "play-session-1",
		PositionTicks: 7 * 10_000_000,
	})

	handler := transcode.Handler{
		Manager: m,
		InputURLForID: func(id string, r *http.Request) string {
			return "http://upstream.local/emby/Videos/" + id + "/stream?" + r.URL.RawQuery
		},
		StartupWait: 50 * time.Millisecond,
	}

	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/segment_00000.ts?runtimeTicks=0", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if starts.Load() != 2 {
		t.Fatalf("starts = %d", starts.Load())
	}
}

func TestManagerUsesShortGraceWhenRestartingSession(t *testing.T) {
	var first *graceProcess
	m := transcode.NewManager(transcode.Options{
		MaxSessions: 1,
		TempDir:     t.TempDir(),
		Runner: runnerFunc(func(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
			process := &graceProcess{}
			if first == nil {
				first = process
			}
			return process, nil
		}),
	})
	t.Cleanup(m.Close)

	if _, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", SegmentStartIndex: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure("item123", transcode.Request{InputURL: "http://upstream/stream", StartTimeTicks: 900_000_000, SegmentStartIndex: 22}); err != nil {
		t.Fatal(err)
	}

	if first.stops.Load() != 1 {
		t.Fatalf("stops = %d", first.stops.Load())
	}
	if grace := time.Duration(first.grace.Load()); grace <= 0 || grace > time.Second {
		t.Fatalf("restart grace = %s", grace)
	}
}

func writeReadySegments(t *testing.T, dir string, from, to int) {
	t.Helper()
	for segment := from; segment <= to; segment++ {
		path := filepath.Join(dir, fmt.Sprintf("segment_%05d.ts", segment))
		if err := os.WriteFile(path, []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

type runnerFunc func(context.Context, *transcode.Session, transcode.Request) (transcode.Process, error)

func (f runnerFunc) Start(ctx context.Context, session *transcode.Session, request transcode.Request) (transcode.Process, error) {
	return f(ctx, session, request)
}

type noopProcess struct{}

func (noopProcess) Stop() error {
	return nil
}

type stopFunc func() error

func (f stopFunc) Stop() error {
	return f()
}

type graceProcess struct {
	stops atomic.Int32
	grace atomic.Int64
}

func (p *graceProcess) Stop() error {
	p.stops.Add(1)
	p.grace.Store(int64(5 * time.Second))
	return nil
}

func (p *graceProcess) StopWithGrace(grace time.Duration) error {
	p.stops.Add(1)
	p.grace.Store(int64(grace))
	return nil
}

type doneProcess struct{}

func (doneProcess) Stop() error {
	return nil
}

func (doneProcess) Done() bool {
	return true
}

type pausingProcess struct {
	pauseCount  atomic.Int32
	resumeCount atomic.Int32
	paused      atomic.Bool
}

func (p *pausingProcess) Stop() error {
	return nil
}

func (p *pausingProcess) Pause() error {
	p.pauseCount.Add(1)
	p.paused.Store(true)
	return nil
}

func (p *pausingProcess) Resume() error {
	p.resumeCount.Add(1)
	p.paused.Store(false)
	return nil
}
