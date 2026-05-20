package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/transcode"
)

func TestPlaybackProgressUpdatesLocalSessionAndForwardsUpstream(t *testing.T) {
	payload := `{"ItemId":"item123","MediaSourceId":"source1","PlaySessionId":"play-session-1","PositionTicks":450000000,"IsPaused":true}`
	var upstreamBody string
	transport := lifecycleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		upstreamBody = string(body)
		if r.URL.Path != "/emby/Sessions/Playing/Progress" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		return lifecycleResponse(http.StatusNoContent, ""), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Transcode.TempDir = t.TempDir()
	cfg.Transcode.FFmpegPath = ""
	srv, err := NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	session, err := srv.transcodeManager.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream.local/emby/Videos/item123/stream",
		MediaSourceID: "source1",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/emby/Sessions/Playing/Progress", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if upstreamBody != payload {
		t.Fatalf("upstream body = %q", upstreamBody)
	}
	if session.PositionTicks != 450000000 {
		t.Fatalf("position ticks = %d", session.PositionTicks)
	}
	if !session.Paused {
		t.Fatal("expected paused state to be recorded")
	}
}

func TestPlaybackStoppedStopsLocalTranscodeAndForwardsUpstream(t *testing.T) {
	payload := `{"ItemId":"item123","MediaSourceId":"source1","PlaySessionId":"play-session-1","PositionTicks":450000000}`
	var upstreamCalled bool
	transport := lifecycleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamCalled = true
		if r.URL.Path != "/emby/Sessions/Playing/Stopped" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		return lifecycleResponse(http.StatusNoContent, ""), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Transcode.TempDir = t.TempDir()
	cfg.Transcode.FFmpegPath = ""
	srv, err := NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	_, err = srv.transcodeManager.Ensure("item123", transcode.Request{
		InputURL:      "http://upstream.local/emby/Videos/item123/stream",
		MediaSourceID: "source1",
		PlaySessionID: "play-session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/emby/Sessions/Playing/Stopped", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !upstreamCalled {
		t.Fatal("expected playback stopped check-in to be forwarded upstream")
	}
	if _, ok := srv.transcodeManager.Get("item123"); ok {
		t.Fatal("expected local transcode session to be stopped")
	}
}

type lifecycleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f lifecycleRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func lifecycleResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
