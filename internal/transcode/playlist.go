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
)

func VirtualVODPlaylist(info MediaInfo, segmentTicks int64, rawQuery string) (string, bool) {
	if info.RunTimeTicks <= 0 || segmentTicks <= 0 {
		return "", false
	}

	segmentCount := int((info.RunTimeTicks + segmentTicks - 1) / segmentTicks)
	if segmentCount <= 0 {
		return "", false
	}

	var builder strings.Builder
	builder.WriteString("#EXTM3U\n")
	builder.WriteString("#EXT-X-VERSION:6\n")
	builder.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(ticksFloatSeconds(segmentTicks)))))
	builder.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	if startTicks := startTimeTicksFromRawQuery(rawQuery); startTicks > 0 {
		builder.WriteString(fmt.Sprintf("#EXT-X-START:TIME-OFFSET=%.3f,PRECISE=YES\n", ticksFloatSeconds(startTicks)))
	}
	builder.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	builder.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	for index := 0; index < segmentCount; index++ {
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
	builder.WriteString("#EXT-X-ENDLIST\n")
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
