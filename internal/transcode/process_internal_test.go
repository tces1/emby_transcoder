package transcode

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"
)

func TestRequestFromHTTPUsesCurrentPlaySessionIDFallback(t *testing.T) {
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/segment_00000.ts?CurrentPlaySessionId=current-session", nil)

	request := requestFromHTTP("item123", "http://upstream/stream", req)

	if request.PlaySessionID != "current-session" {
		t.Fatalf("play session id = %q", request.PlaySessionID)
	}
}

func TestRequestFromHTTPParsesAudioStreamIndex(t *testing.T) {
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?AudioStreamIndex=2", nil)

	request := requestFromHTTP("item123", "http://upstream/stream", req)

	if request.AudioStreamIndex != 2 {
		t.Fatalf("audio stream index = %d", request.AudioStreamIndex)
	}
}

func TestRequestWithSegmentStartUsesRuntimeTicksWhenPresent(t *testing.T) {
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/segment_00067.ts?actualSegmentLengthTicks=40000000&runtimeTicks=2680123456&X-Emby-Token=abc", nil)

	segmentReq := requestWithSegmentStart(req, 67, 2*timeSecondTicks)
	query := segmentReq.URL.Query()

	if got := query.Get("StartTimeTicks"); got != "2680123456" {
		t.Fatalf("StartTimeTicks = %q", got)
	}
	if got := query.Get("runtimeTicks"); got != "" {
		t.Fatalf("runtimeTicks should not be forwarded upstream, got %q", got)
	}
	if got := query.Get("actualSegmentLengthTicks"); got != "" {
		t.Fatalf("actualSegmentLengthTicks should not be forwarded upstream, got %q", got)
	}
	if got := query.Get("X-Emby-Token"); got != "abc" {
		t.Fatalf("X-Emby-Token = %q", got)
	}
}

func TestRequestWithSegmentStartUsesConfiguredSegmentDuration(t *testing.T) {
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/segment_00067.ts?X-Emby-Token=abc", nil)

	segmentReq := requestWithSegmentStart(req, 67, 2*timeSecondTicks)

	if got := segmentReq.URL.Query().Get("StartTimeTicks"); got != "1340000000" {
		t.Fatalf("StartTimeTicks = %q", got)
	}
}

func TestSegmentInputCompatibleIgnoresPlaybackSessionOptions(t *testing.T) {
	existing := "https://upstream/emby/Videos/item123/stream?AudioStreamIndex=1&MediaSourceId=source1&StartTimeTicks=0&X-Emby-Token=abc"
	next := "https://upstream/emby/Videos/item123/stream?AllowAudioStreamCopy=false&AllowVideoStreamCopy=false&AudioStreamIndex=1&CurrentPlaySessionId=session2&EnableDirectPlay=false&EnableDirectStream=false&MediaSourceId=source1&PlaySessionId=session1&StartTimeTicks=0&SubtitleStreamIndex=-1&X-Emby-Token=abc"

	if !segmentInputCompatible(existing, next) {
		t.Fatal("expected compatible input when only playback-session options change")
	}
}

func TestSegmentInputCompatibleRejectsMediaSourceChange(t *testing.T) {
	existing := "https://upstream/emby/Videos/item123/stream?AudioStreamIndex=1&MediaSourceId=source1&X-Emby-Token=abc"
	next := "https://upstream/emby/Videos/item123/stream?AudioStreamIndex=1&MediaSourceId=source2&X-Emby-Token=abc"

	if segmentInputCompatible(existing, next) {
		t.Fatal("expected incompatible input when media source changes")
	}
}

func TestExecProcessStopWaitsForProcessExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	process := &execProcess{cmd: cmd, stdin: stdin, doneCh: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		process.done.Store(true)
		close(process.doneCh)
	}()

	if err := process.Stop(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if process.Done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Stop returned before process exit was observed")
}
