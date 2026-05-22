package transcode

import (
	"strings"
	"testing"
)

func TestVirtualVODPlaylistUsesMediaDurationAndOriginalQuery(t *testing.T) {
	playlist, ok := VirtualVODPlaylist(MediaInfo{RunTimeTicks: 10_500_0000}, defaultSegmentTicks, "X-Emby-Token=abc&StartTimeTicks=40000000")
	if !ok {
		t.Fatal("expected virtual playlist")
	}

	if !strings.Contains(playlist, "#EXT-X-PLAYLIST-TYPE:VOD") {
		t.Fatalf("playlist = %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-TARGETDURATION:2") {
		t.Fatalf("missing 2 second target duration: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-START:TIME-OFFSET=4.000") {
		t.Fatalf("missing start offset: %s", playlist)
	}
	if !strings.Contains(playlist, "segment_00000.ts?X-Emby-Token=abc&StartTimeTicks=40000000") {
		t.Fatalf("missing first segment query: %s", playlist)
	}
	if !strings.Contains(playlist, "segment_00005.ts?X-Emby-Token=abc&StartTimeTicks=40000000") {
		t.Fatalf("missing final segment: %s", playlist)
	}
	if !strings.Contains(playlist, "runtimeTicks=100000000&actualSegmentLengthTicks=5000000") {
		t.Fatalf("missing final segment timing: %s", playlist)
	}
	if !strings.Contains(playlist, "#EXTINF:0.500,") {
		t.Fatalf("missing partial final duration: %s", playlist)
	}
	if !strings.HasSuffix(strings.TrimSpace(playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("playlist should end with ENDLIST: %s", playlist)
	}
}

func TestVirtualVODPlaylistUsesConfiguredSegmentDuration(t *testing.T) {
	playlist, ok := VirtualVODPlaylist(MediaInfo{RunTimeTicks: 5 * timeSecondTicks}, 2*timeSecondTicks, "")
	if !ok {
		t.Fatal("expected virtual playlist")
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
}
