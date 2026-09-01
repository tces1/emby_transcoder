package transcode

import (
	"strings"
	"testing"
)

func TestGrowingMediaPlaylistAdvertisesNoSegmentBeforeFirstIsReady(t *testing.T) {
	playlist, ok := GrowingMediaPlaylist(MediaInfo{RunTimeTicks: 10_500_0000}, defaultSegmentTicks, "X-Emby-Token=abc", 0, 0)
	if !ok {
		t.Fatal("expected growing playlist")
	}

	if !strings.Contains(playlist, "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Fatalf("playlist = %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-TARGETDURATION:2") {
		t.Fatalf("missing 2 second target duration: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Fatalf("missing media sequence: %s", playlist)
	}
	if strings.Contains(playlist, "segment_") || strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist advertised unavailable media: %s", playlist)
	}
}

func TestGrowingMediaPlaylistIncludesReadySegmentsAndOneLookahead(t *testing.T) {
	playlist, ok := GrowingMediaPlaylist(MediaInfo{RunTimeTicks: 10_500_0000}, defaultSegmentTicks, "X-Emby-Token=abc", 0, 2)
	if !ok {
		t.Fatal("expected growing playlist")
	}
	for _, segment := range []string{"segment_00000.ts", "segment_00001.ts", "segment_00002.ts"} {
		if !strings.Contains(playlist, segment) {
			t.Fatalf("missing %s: %s", segment, playlist)
		}
	}
	if strings.Contains(playlist, "segment_00003.ts") || strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist advertised beyond lookahead: %s", playlist)
	}
}

func TestGrowingMediaPlaylistSeekStartsAtRequestedWindow(t *testing.T) {
	playlist, ok := GrowingMediaPlaylist(MediaInfo{RunTimeTicks: 3600 * timeSecondTicks}, defaultSegmentTicks, "StartTimeTicks=6418677540", 320, 1)
	if !ok {
		t.Fatal("expected growing playlist")
	}
	if !strings.Contains(playlist, "#EXT-X-MEDIA-SEQUENCE:320") ||
		!strings.Contains(playlist, "segment_00320.ts") {
		t.Fatalf("seek playlist is not aligned: %s", playlist)
	}
	if strings.Contains(playlist, "segment_00000.ts") || strings.Contains(playlist, "#EXT-X-START") {
		t.Fatalf("seek playlist referenced the title beginning: %s", playlist)
	}
}

func TestGrowingMediaPlaylistEndsOnlyWhenAllSegmentsAreReady(t *testing.T) {
	playlist, ok := GrowingMediaPlaylist(MediaInfo{RunTimeTicks: 5 * timeSecondTicks}, 2*timeSecondTicks, "", 0, 3)
	if !ok {
		t.Fatal("expected growing playlist")
	}

	if !strings.Contains(playlist, "#EXT-X-TARGETDURATION:2") {
		t.Fatalf("missing 2 second target duration: %s", playlist)
	}
	if !strings.Contains(playlist, "segment_00002.ts?runtimeTicks=40000000&actualSegmentLengthTicks=10000000") {
		t.Fatalf("missing configured final segment timing: %s", playlist)
	}
	if strings.Contains(playlist, "segment_00005.ts") {
		t.Fatalf("playlist should not use 1 second segment count: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-PLAYLIST-TYPE:VOD") ||
		!strings.HasSuffix(strings.TrimSpace(playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("complete playlist should be final VOD: %s", playlist)
	}
}
