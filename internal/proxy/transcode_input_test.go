package proxy

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTranscodeInputURLPreservesStartTimeTicks(t *testing.T) {
	upstream, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?StartTimeTicks=900000000&MediaSourceId=source1&X-Emby-Token=abc", nil)

	got := transcodeInputURL(upstream, "item123", req)

	if !strings.Contains(got, "StartTimeTicks=900000000") {
		t.Fatalf("input url should preserve StartTimeTicks for the upstream stream endpoint: %s", got)
	}
	if !strings.Contains(got, "MediaSourceId=source1") || !strings.Contains(got, "X-Emby-Token=abc") {
		t.Fatalf("input url should preserve non-seek query params: %s", got)
	}
	if !strings.HasPrefix(got, "http://upstream.local/emby/Videos/item123/stream?") {
		t.Fatalf("input url = %s", got)
	}
}

func TestTranscodeInputURLPreservesAudioStreamIndexForEmbySelection(t *testing.T) {
	upstream, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?AudioStreamIndex=2&MediaSourceId=source1&X-Emby-Token=abc", nil)

	got := transcodeInputURL(upstream, "item123", req)

	if !strings.Contains(got, "AudioStreamIndex=2") {
		t.Fatalf("input url should forward AudioStreamIndex so Emby returns the selected audio track: %s", got)
	}
	if !strings.Contains(got, "MediaSourceId=source1") || !strings.Contains(got, "X-Emby-Token=abc") {
		t.Fatalf("input url should preserve non-audio query params: %s", got)
	}
}

func TestTranscodeInputURLStripsReqFormatFromStreamRequest(t *testing.T) {
	upstream, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?reqformat=json&StartTimeTicks=900000000&MediaSourceId=source1&AudioStreamIndex=2", nil)

	got := transcodeInputURL(upstream, "item123", req)

	if strings.Contains(got, "reqformat=") {
		t.Fatalf("input url should not forward reqformat to the stream endpoint: %s", got)
	}
	if !strings.Contains(got, "StartTimeTicks=900000000") || !strings.Contains(got, "MediaSourceId=source1") {
		t.Fatalf("input url should keep stream parameters: %s", got)
	}
}

func TestTranscodeInputURLUsesHeaderTokenWhenQueryIsMissing(t *testing.T) {
	upstream, err := url.Parse("http://upstream.local")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/streambridge/transcode/item123/master.m3u8?MediaSourceId=source1", nil)
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Emby for Android TV", Token="abc"`)

	got := transcodeInputURL(upstream, "item123", req)

	if !strings.Contains(got, "X-Emby-Token=abc") {
		t.Fatalf("input url should carry token from auth header: %s", got)
	}
}
