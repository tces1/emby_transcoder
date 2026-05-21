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

	req := httptest.NewRequest("GET", "/emby/Items/item123/PlaybackInfo", nil)
	req.Header.Set("User-Agent", "Yamby TV")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if session, ok := srv.transcodeManager.Get("item123"); ok && session != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected playback info to prewarm a transcode session")
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
