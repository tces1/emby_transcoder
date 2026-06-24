package emby_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"emby-transcoder/internal/emby"
)

func TestRewritePlaybackInfoInjectsTranscodingURL(t *testing.T) {
	input := []byte(`{"MediaSources":[{"Id":"source1","SupportsDirectPlay":true,"SupportsDirectStream":true}]}`)

	out, changed, err := emby.RewritePlaybackInfo(input, "item123", "http://proxy.local")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected rewrite to change response")
	}
	if !bytes.Contains(out, []byte(`/streambridge/transcode/`)) {
		t.Fatalf("missing local transcode url: %s", out)
	}
	if !bytes.Contains(out, []byte(`/streambridge/transcode/item123/master.m3u8`)) {
		t.Fatalf("transcode url should use the item id as session id: %s", out)
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	source := decoded["MediaSources"].([]any)[0].(map[string]any)
	if source["SupportsDirectPlay"] != false {
		t.Fatalf("SupportsDirectPlay = %v", source["SupportsDirectPlay"])
	}
	if source["SupportsTranscoding"] != true {
		t.Fatalf("SupportsTranscoding = %v", source["SupportsTranscoding"])
	}
	if source["TranscodingContainer"] != "ts" {
		t.Fatalf("TranscodingContainer = %v", source["TranscodingContainer"])
	}
	if source["TranscodingSubProtocol"] != "hls" {
		t.Fatalf("TranscodingSubProtocol = %v", source["TranscodingSubProtocol"])
	}
	if source["Protocol"] != "Http" {
		t.Fatalf("Protocol = %v", source["Protocol"])
	}
}

func TestRewritePlaybackInfoPreservesQueryString(t *testing.T) {
	input := []byte(`{"MediaSources":[{"Id":"source1","SupportsDirectPlay":true}]}`)

	out, changed, err := emby.RewritePlaybackInfo(input, "item123", "http://proxy.local", "X-Emby-Token=abc")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected rewrite to change response")
	}
	if !bytes.Contains(out, []byte(`X-Emby-Token=abc`)) {
		t.Fatalf("missing query string: %s", out)
	}
	if !bytes.Contains(out, []byte(`MediaSourceId=source1`)) {
		t.Fatalf("missing media source id: %s", out)
	}
}

func TestRewritePlaybackInfoAddsSourceParametersWhenClientOmitsThem(t *testing.T) {
	input := []byte(`{"MediaSources":[{"Id":"source1","SupportsDirectPlay":true,"MediaStreams":[{"Type":"Audio","Index":1,"Codec":"dts"}]}]}`)

	out, changed, err := emby.RewritePlaybackInfo(input, "item123", "http://proxy.local", "AutoOpenLiveStream=false&IsPlayback=false")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected rewrite to change response")
	}
	if !bytes.Contains(out, []byte(`MediaSourceId=source1`)) {
		t.Fatalf("missing media source id: %s", out)
	}
	if !bytes.Contains(out, []byte(`AudioStreamIndex=1`)) {
		t.Fatalf("missing audio stream index: %s", out)
	}
}

func TestRewritePlaybackInfoWithReportDescribesSources(t *testing.T) {
	input := []byte(`{"MediaSources":[{"Id":"source1","Name":"4K - 80 Mbps","Path":"/media/Movie.mkv","Container":"mkv","Bitrate":80000000,"RunTimeTicks":72000000000,"SupportsDirectPlay":true,"DirectStreamUrl":"/Videos/1/stream","MediaStreams":[{"Type":"Video","Index":0,"Codec":"hevc","Profile":"Main 10","BitDepth":10,"PixelFormat":"yuv420p10le","Width":3840,"Height":2160},{"Type":"Audio","Index":1,"Codec":"dts","Channels":6,"DisplayTitle":"DTS 5.1"},{"Type":"Audio","Index":2,"Codec":"aac","Channels":2,"DisplayTitle":"AAC 2.0"}]}]}`)

	_, changed, report, err := emby.RewritePlaybackInfoWithReport(input, "item123", "http://proxy.local")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected rewrite to change response")
	}
	if report.ItemID != "item123" {
		t.Fatalf("item id = %q", report.ItemID)
	}
	if len(report.Sources) != 1 {
		t.Fatalf("sources = %+v", report.Sources)
	}
	source := report.Sources[0]
	if source.ID != "source1" || !source.BeforeSupportsDirectPlay {
		t.Fatalf("source = %+v", source)
	}
	if source.BeforeDirectStreamURL != "/Videos/1/stream" {
		t.Fatalf("direct stream url = %q", source.BeforeDirectStreamURL)
	}
	if source.AfterTranscodingURL != "/streambridge/transcode/item123/master.m3u8?AudioStreamIndex=1&MediaSourceId=source1" {
		t.Fatalf("after url = %q", source.AfterTranscodingURL)
	}
	if source.SessionID != "item123" {
		t.Fatalf("session id = %q", source.SessionID)
	}
	if source.Name != "4K - 80 Mbps" {
		t.Fatalf("name = %q", source.Name)
	}
	if source.Path != "/media/Movie.mkv" {
		t.Fatalf("path = %q", source.Path)
	}
	if source.Container != "mkv" {
		t.Fatalf("container = %q", source.Container)
	}
	if source.VideoCodec != "hevc" || source.VideoProfile != "Main 10" || source.VideoBitDepth != 10 || source.Width != 3840 || source.Height != 2160 {
		t.Fatalf("video = %s %s %dbit %dx%d", source.VideoCodec, source.VideoProfile, source.VideoBitDepth, source.Width, source.Height)
	}
	if source.AudioCodec != "dts" || source.AudioChannels != 6 || source.AudioTitle != "DTS 5.1" {
		t.Fatalf("audio = %s channels=%d title=%q", source.AudioCodec, source.AudioChannels, source.AudioTitle)
	}
	if len(source.AudioStreams) != 2 {
		t.Fatalf("audio streams = %+v", source.AudioStreams)
	}
	if source.AudioStreams[0].Index != 1 || source.AudioStreams[0].Ordinal != 0 || source.AudioStreams[0].Codec != "dts" {
		t.Fatalf("first audio stream = %+v", source.AudioStreams[0])
	}
	if source.AudioStreams[1].Index != 2 || source.AudioStreams[1].Ordinal != 1 || source.AudioStreams[1].Codec != "aac" {
		t.Fatalf("second audio stream = %+v", source.AudioStreams[1])
	}
	if source.Bitrate != 80000000 || source.RunTimeTicks != 72000000000 {
		t.Fatalf("bitrate/runtime = %d/%d", source.Bitrate, source.RunTimeTicks)
	}
}

func TestRewritePlaybackInfoLeavesNonMediaSourceJSONAlone(t *testing.T) {
	input := []byte(`{"Name":"not playback info"}`)

	out, changed, err := emby.RewritePlaybackInfo(input, "item123", "http://proxy.local")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected unchanged response")
	}
	if string(out) != string(input) {
		t.Fatalf("out = %s", out)
	}
}
