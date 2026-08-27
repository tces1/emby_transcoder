package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/proxy"
)

func TestPlaybackInfoIsRewrittenForMatchedClient(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/emby/Items/item123/PlaybackInfo" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept-Encoding"); got != "" {
			t.Fatalf("Accept-Encoding should be stripped for rewriteable JSON requests, got %q", got)
		}
		return jsonResponse(`{"MediaSources":[{"Id":"source1","Name":"4K - 80 Mbps","Container":"mkv","SupportsDirectPlay":true,"DirectStreamUrl":"/emby/videos/item123/original.mkv?api_key=test-token","MediaStreams":[{"Type":"Video","Codec":"hevc","Width":3840,"Height":2160},{"Type":"Audio","Codec":"dts","Channels":6}]}]}`), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Server.PublicURL = "http://proxy.local"
	cfg.Clients = []config.ClientProfile{{Name: "yamby", Match: []string{"Yamby"}, Transcode: true}}

	srv, err := proxy.NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/emby/Items/item123/PlaybackInfo", nil)
	req.Header.Set("User-Agent", "Yamby TV")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/streambridge/transcode/") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	info, ok := srv.TranscodeMediaInfo("item123")
	if !ok {
		t.Fatal("expected playback info rewrite to register media info")
	}
	if info.VideoCodec != "hevc" || info.Width != 3840 || info.Height != 2160 || info.AudioCodec != "dts" {
		t.Fatalf("media info = %+v", info)
	}
	if info.InputURL != "http://upstream.local/emby/videos/item123/original.mkv?api_key=test-token" {
		t.Fatalf("media input url = %q", info.InputURL)
	}
}

func TestPlaybackInfoPassesThroughForUnmatchedClient(t *testing.T) {
	upstreamBody := `{"MediaSources":[{"Id":"source1","SupportsDirectPlay":true}]}`
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(upstreamBody), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Clients = []config.ClientProfile{{Name: "yamby", Match: []string{"Yamby"}, Transcode: true}}

	srv, err := proxy.NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/emby/Items/item123/PlaybackInfo", nil)
	req.Header.Set("User-Agent", "Plain Browser")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "/streambridge/transcode/") {
		t.Fatalf("body should not be rewritten: %s", rec.Body.String())
	}
}

func TestNormalRequestsAreProxied(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/emby/System/Info" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if r.Host != "upstream.local" {
			t.Fatalf("unexpected upstream host header: %s", r.Host)
		}
		return jsonResponse(`{"ServerName":"upstream"}`), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"

	srv, err := proxy.NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/emby/System/Info", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestPlaybackInfoCarriesHeaderTokenIntoRewrittenTranscodeURL(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(`{"MediaSources":[{"Id":"source1","SupportsDirectPlay":true,"MediaStreams":[{"Type":"Video","Codec":"hevc","Width":3840,"Height":2160},{"Type":"Audio","Codec":"dts","Channels":6}]}]}`), nil
	})

	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Server.PublicURL = "http://proxy.local"
	cfg.Clients = []config.ClientProfile{{Name: "yamby", Match: []string{"Yamby"}, Transcode: true}}

	srv, err := proxy.NewWithTransport(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/emby/Items/item123/PlaybackInfo?AutoOpenLiveStream=false&IsPlayback=false", nil)
	req.Header.Set("User-Agent", "Yamby TV")
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Emby for Android TV", Token="abc"`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "X-Emby-Token=abc") {
		t.Fatalf("rewritten body should carry header token: %s", rec.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
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
