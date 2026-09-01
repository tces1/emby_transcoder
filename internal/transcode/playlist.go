package transcode

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

const (
	timeSecondTicks     int64 = 10_000_000
	defaultSegmentTicks       = 2 * timeSecondTicks
	playlistLookahead         = 1
)

func GrowingMediaPlaylist(info MediaInfo, segmentTicks int64, rawQuery string, firstIndex, readyCount int) (string, bool) {
	if info.RunTimeTicks <= 0 || segmentTicks <= 0 {
		return "", false
	}

	segmentCount := int((info.RunTimeTicks + segmentTicks - 1) / segmentTicks)
	if segmentCount <= 0 {
		return "", false
	}
	if firstIndex < 0 {
		firstIndex = 0
	}
	if firstIndex >= segmentCount {
		firstIndex = segmentCount - 1
	}
	if readyCount < 0 {
		readyCount = 0
	}
	remaining := segmentCount - firstIndex
	if readyCount > remaining {
		readyCount = remaining
	}
	complete := readyCount == remaining
	listed := readyCount
	if readyCount > 0 && !complete {
		listed += playlistLookahead
	}
	if listed > remaining {
		listed = remaining
	}

	var builder strings.Builder
	builder.WriteString("#EXTM3U\n")
	builder.WriteString("#EXT-X-VERSION:6\n")
	builder.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(ticksFloatSeconds(segmentTicks)))))
	if complete {
		builder.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	} else {
		builder.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	}
	if firstIndex == 0 {
		if startTicks := startTimeTicksFromRawQuery(rawQuery); startTicks > 0 && startTicks < segmentTicks {
			builder.WriteString(fmt.Sprintf("#EXT-X-START:TIME-OFFSET=%.3f,PRECISE=YES\n", ticksFloatSeconds(startTicks)))
		}
	}
	builder.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", firstIndex))
	builder.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	for index := firstIndex; index < firstIndex+listed; index++ {
		segmentStart := int64(index) * segmentTicks
		durationTicks := min(segmentTicks, info.RunTimeTicks-segmentStart)
		builder.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", ticksFloatSeconds(durationTicks)))
		builder.WriteString(fmt.Sprintf("segment_%05d.ts", index))
		if rawQuery != "" {
			builder.WriteString("?")
			builder.WriteString(rawQuery)
			builder.WriteString("&")
		} else {
			builder.WriteString("?")
		}
		builder.WriteString("runtimeTicks=")
		builder.WriteString(strconv.FormatInt(segmentStart, 10))
		builder.WriteString("&actualSegmentLengthTicks=")
		builder.WriteString(strconv.FormatInt(durationTicks, 10))
		builder.WriteString("\n")
	}
	if complete {
		builder.WriteString("#EXT-X-ENDLIST\n")
	}
	return builder.String(), true
}

func startTimeTicksFromRawQuery(rawQuery string) int64 {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return 0
	}
	ticks, _ := strconv.ParseInt(values.Get("StartTimeTicks"), 10, 64)
	return ticks
}

func ticksFloatSeconds(ticks int64) float64 {
	return float64(ticks) / float64(timeSecondTicks)
}
