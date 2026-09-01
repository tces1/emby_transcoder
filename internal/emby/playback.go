package emby

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var sessionSafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func RewritePlaybackInfo(body []byte, itemID string, publicURL string, rawQuery ...string) ([]byte, bool, error) {
	out, changed, _, err := RewritePlaybackInfoWithReport(body, itemID, publicURL, rawQuery...)
	return out, changed, err
}

type RewriteReport struct {
	ItemID  string
	Sources []SourceReport
}

type SourceReport struct {
	Index                    int
	ID                       string
	SessionID                string
	Name                     string
	Path                     string
	Container                string
	VideoCodec               string
	VideoProfile             string
	VideoPixelFormat         string
	VideoBitDepth            int
	Width                    int
	Height                   int
	AudioCodec               string
	AudioChannels            int
	AudioTitle               string
	AudioStreams             []AudioStreamReport
	DefaultAudioStreamIndex  int
	HasDefaultAudioStream    bool
	Bitrate                  int64
	RunTimeTicks             int64
	BeforeSupportsDirectPlay bool
	BeforeSupportsTranscode  bool
	BeforeDirectStreamURL    string
	BeforeTranscodingURL     string
	AfterTranscodingURL      string
}

type AudioStreamReport struct {
	Index     int
	Ordinal   int
	Codec     string
	Channels  int
	Title     string
	IsDefault bool
}

func RewritePlaybackInfoWithReport(body []byte, itemID string, publicURL string, rawQuery ...string) ([]byte, bool, RewriteReport, error) {
	report := RewriteReport{ItemID: itemID}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, report, err
	}

	rawSources, ok := root["MediaSources"].([]any)
	if !ok || len(rawSources) == 0 {
		return body, false, report, nil
	}

	changed := false
	for index, rawSource := range rawSources {
		source, ok := rawSource.(map[string]any)
		if !ok {
			continue
		}
		sourceID, _ := source["Id"].(string)
		sessionID := SessionID(itemID, sourceID, index)
		beforeDirectPlay, _ := source["SupportsDirectPlay"].(bool)
		beforeTranscode, _ := source["SupportsTranscoding"].(bool)
		beforeDirectStreamURL, _ := source["DirectStreamUrl"].(string)
		beforeTranscodingURL, _ := source["TranscodingUrl"].(string)
		sourceReport := sourceReportFromMap(source)
		transcodeURL := fmt.Sprintf("/streambridge/transcode/%s/master.m3u8", sessionID)
		if enrichedQuery := transcodeQueryForSource(firstRawQuery(rawQuery), sourceID, sourceReport); enrichedQuery != "" {
			transcodeURL += "?" + enrichedQuery
		}
		sourceReport.Index = index
		sourceReport.ID = sourceID
		sourceReport.SessionID = sessionID
		sourceReport.BeforeSupportsDirectPlay = beforeDirectPlay
		sourceReport.BeforeSupportsTranscode = beforeTranscode
		sourceReport.BeforeDirectStreamURL = beforeDirectStreamURL
		sourceReport.BeforeTranscodingURL = beforeTranscodingURL
		sourceReport.AfterTranscodingURL = transcodeURL
		report.Sources = append(report.Sources, SourceReport{
			Index:                    sourceReport.Index,
			ID:                       sourceReport.ID,
			SessionID:                sourceReport.SessionID,
			Name:                     sourceReport.Name,
			Path:                     sourceReport.Path,
			Container:                sourceReport.Container,
			VideoCodec:               sourceReport.VideoCodec,
			VideoProfile:             sourceReport.VideoProfile,
			VideoPixelFormat:         sourceReport.VideoPixelFormat,
			VideoBitDepth:            sourceReport.VideoBitDepth,
			Width:                    sourceReport.Width,
			Height:                   sourceReport.Height,
			AudioCodec:               sourceReport.AudioCodec,
			AudioChannels:            sourceReport.AudioChannels,
			AudioTitle:               sourceReport.AudioTitle,
			AudioStreams:             sourceReport.AudioStreams,
			DefaultAudioStreamIndex:  sourceReport.DefaultAudioStreamIndex,
			HasDefaultAudioStream:    sourceReport.HasDefaultAudioStream,
			Bitrate:                  sourceReport.Bitrate,
			RunTimeTicks:             sourceReport.RunTimeTicks,
			BeforeSupportsDirectPlay: sourceReport.BeforeSupportsDirectPlay,
			BeforeSupportsTranscode:  sourceReport.BeforeSupportsTranscode,
			BeforeDirectStreamURL:    sourceReport.BeforeDirectStreamURL,
			BeforeTranscodingURL:     sourceReport.BeforeTranscodingURL,
			AfterTranscodingURL:      sourceReport.AfterTranscodingURL,
		})

		source["SupportsDirectPlay"] = false
		source["SupportsDirectStream"] = false
		source["SupportsTranscoding"] = true
		source["TranscodingUrl"] = transcodeURL
		source["TranscodingContainer"] = "ts"
		source["TranscodingSubProtocol"] = "hls"
		source["Protocol"] = "Http"
		source["DirectStreamUrl"] = transcodeURL
		changed = true
	}

	if !changed {
		return body, false, report, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, report, err
	}
	return out, true, report, nil
}

func firstRawQuery(rawQuery []string) string {
	if len(rawQuery) == 0 {
		return ""
	}
	return rawQuery[0]
}

func transcodeQueryForSource(rawQuery string, sourceID string, source SourceReport) string {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	if sourceID != "" && query.Get("MediaSourceId") == "" {
		query.Set("MediaSourceId", sourceID)
	}
	if query.Get("AudioStreamIndex") == "" {
		if audioIndex, ok := preferredAudioStream(source); ok {
			query.Set("AudioStreamIndex", strconv.Itoa(audioIndex))
		}
	}
	return query.Encode()
}

func sourceReportFromMap(source map[string]any) SourceReport {
	report := SourceReport{
		Name:         stringValue(source, "Name"),
		Path:         stringValue(source, "Path"),
		Container:    stringValue(source, "Container"),
		Bitrate:      int64Value(source, "Bitrate"),
		RunTimeTicks: int64Value(source, "RunTimeTicks"),
	}
	if defaultIndex, ok := optionalIntValue(source, "DefaultAudioStreamIndex"); ok {
		report.DefaultAudioStreamIndex = defaultIndex
		report.HasDefaultAudioStream = true
	}

	rawStreams, _ := source["MediaStreams"].([]any)
	audioOrdinal := 0
	for _, rawStream := range rawStreams {
		stream, ok := rawStream.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case strings.EqualFold(stringValue(stream, "Type"), "Video") && report.VideoCodec == "":
			report.VideoCodec = stringValue(stream, "Codec")
			report.VideoProfile = stringValue(stream, "Profile")
			report.VideoPixelFormat = stringValue(stream, "PixelFormat")
			report.VideoBitDepth = intValue(stream, "BitDepth")
			report.Width = intValue(stream, "Width")
			report.Height = intValue(stream, "Height")
		case strings.EqualFold(stringValue(stream, "Type"), "Audio"):
			audio := AudioStreamReport{
				Index:     intValue(stream, "Index"),
				Ordinal:   audioOrdinal,
				Codec:     stringValue(stream, "Codec"),
				Channels:  intValue(stream, "Channels"),
				Title:     stringValue(stream, "DisplayTitle"),
				IsDefault: boolValue(stream, "IsDefault"),
			}
			report.AudioStreams = append(report.AudioStreams, audio)
			audioOrdinal++
			if report.AudioCodec == "" {
				report.AudioCodec = audio.Codec
				report.AudioChannels = audio.Channels
				report.AudioTitle = audio.Title
			}
		}
	}
	if selected, ok := preferredAudioStream(report); ok {
		for _, audio := range report.AudioStreams {
			if audio.Index == selected {
				report.AudioCodec = audio.Codec
				report.AudioChannels = audio.Channels
				report.AudioTitle = audio.Title
				break
			}
		}
	}

	return report
}

func preferredAudioStream(source SourceReport) (int, bool) {
	if source.HasDefaultAudioStream {
		for _, stream := range source.AudioStreams {
			if stream.Index == source.DefaultAudioStreamIndex {
				return source.DefaultAudioStreamIndex, true
			}
		}
	}
	for _, stream := range source.AudioStreams {
		if stream.IsDefault {
			return stream.Index, true
		}
	}
	if len(source.AudioStreams) == 1 {
		return source.AudioStreams[0].Index, true
	}
	return 0, false
}

func optionalIntValue(values map[string]any, key string) (int, bool) {
	value, ok := values[key]
	if !ok || value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int64:
		return int(typed), true
	case int:
		return typed, true
	}
	return 0, false
}

func boolValue(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func stringValue(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}

func intValue(values map[string]any, key string) int {
	return int(int64Value(values, key))
}

func int64Value(values map[string]any, key string) int64 {
	value, ok := values[key]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	}
	return 0
}

func SessionID(itemID string, sourceID string, index int) string {
	base := sessionSafe.ReplaceAllString(strings.TrimSpace(itemID), "_")
	if base == "" {
		base = "item"
	}
	return base
}
