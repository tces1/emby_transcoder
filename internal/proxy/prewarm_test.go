package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/transcode"
)

func TestPlaybackInfoPrewarmsTranscodeSessionForMatchedClient(t *testing.T) {
	transport := prewarmRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"MediaSources":[{"Id":"source1","Name":"4K - 80 Mbps","Container":"mkv","SupportsDirectPlay":true,"MediaStreams":[{"Type":"Video","Codec":"hevc","Width":3840,"Height":2160},{"Type":"Audio","Codec":"dts","Channels":6}]}]}`), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Server.PublicURL = "http://proxy.local"
	cfg.Transcode.FFmpegPath = ""
	cfg.Transcode.TempDir = t.TempDir()
	cfg.Clients = []config.ClientProfile{{Name: "yamby", Match: []string{"Yamby"}, Transcode: true}}

	srv, err := NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/emby/Items/item123/PlaybackInfo?AudioStreamIndex=1&AutoOpenLiveStream=false&IsPlayback=false&X-Emby-Token=abc", nil)
	req.Header.Set("User-Agent", "Yamby TV")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if session, ok := srv.transcodeManager.Get("item123"); ok && session != nil {
			if session.AudioStreamIndex != 1 {
				t.Fatalf("expected prewarmed audio stream index 1, got %d", session.AudioStreamIndex)
			}
			if !strings.Contains(session.InputURL, "MediaSourceId=source1") {
				t.Fatalf("expected prewarm input URL to include media source, got %s", session.InputURL)
			}
			if !strings.Contains(session.InputURL, "AutoOpenLiveStream=true") || !strings.Contains(session.InputURL, "IsPlayback=true") {
				t.Fatalf("expected prewarm input URL to use playback stream flags, got %s", session.InputURL)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected playback info to prewarm a transcode session")
}

func TestPlaybackInfoPrewarmDoesNotRestartExistingSession(t *testing.T) {
	transport := prewarmRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"MediaSources":[{"Id":"source1","Name":"4K - 80 Mbps","Container":"mkv","SupportsDirectPlay":true,"MediaStreams":[{"Type":"Video","Codec":"hevc","Width":3840,"Height":2160},{"Type":"Audio","Index":1,"Codec":"dts","Channels":6}]}]}`), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Server.PublicURL = "http://proxy.local"
	cfg.Transcode.FFmpegPath = ""
	cfg.Transcode.TempDir = t.TempDir()
	cfg.Clients = []config.ClientProfile{{Name: "yamby", Match: []string{"Yamby"}, Transcode: true}}

	srv, err := NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := srv.transcodeManager.Ensure("item123", transcode.Request{
		InputURL:         "http://upstream.local/emby/Videos/item123/stream?MediaSourceId=source1&AudioStreamIndex=1&StartTimeTicks=13030000000",
		MediaSourceID:    "source1",
		AudioStreamIndex: 1,
		StartTimeTicks:   13030000000,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/emby/Items/item123/PlaybackInfo?AudioStreamIndex=1", nil)
	req.Header.Set("User-Agent", "Yamby TV")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	session, ok := srv.transcodeManager.Get("item123")
	if !ok {
		t.Fatal("expected existing session to remain")
	}
	if session != existing {
		t.Fatal("prewarm should not replace an existing transcode session")
	}
	if session.StartTimeTicks != 13030000000 {
		t.Fatalf("start ticks = %d", session.StartTimeTicks)
	}
}

type prewarmRoundTripFunc func(*http.Request) (*http.Response, error)

func (f prewarmRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{URL: &url.URL{Scheme: "http", Host: "upstream.local"}},
	}
}
