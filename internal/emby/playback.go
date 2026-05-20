package emby

import (
	"encoding/json"
	"fmt"
	"regexp"
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
	Width                    int
	Height                   int
	AudioCodec               string
	AudioChannels            int
	AudioTitle               string
	Bitrate                  int64
	RunTimeTicks             int64
	BeforeSupportsDirectPlay bool
	BeforeSupportsTranscode  bool
	BeforeDirectStreamURL    string
	BeforeTranscodingURL     string
	AfterTranscodingURL      string
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
		transcodeURL := fmt.Sprintf("/streambridge/transcode/%s/master.m3u8", sessionID)
		if len(rawQuery) > 0 && rawQuery[0] != "" {
			transcodeURL += "?" + rawQuery[0]
		}

		beforeDirectPlay, _ := source["SupportsDirectPlay"].(bool)
		beforeTranscode, _ := source["SupportsTranscoding"].(bool)
		beforeDirectStreamURL, _ := source["DirectStreamUrl"].(string)
		beforeTranscodingURL, _ := source["TranscodingUrl"].(string)
		sourceReport := sourceReportFromMap(source)
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
			Width:                    sourceReport.Width,
			Height:                   sourceReport.Height,
			AudioCodec:               sourceReport.AudioCodec,
			AudioChannels:            sourceReport.AudioChannels,
			AudioTitle:               sourceReport.AudioTitle,
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

func sourceReportFromMap(source map[string]any) SourceReport {
	report := SourceReport{
		Name:         stringValue(source, "Name"),
		Path:         stringValue(source, "Path"),
		Container:    stringValue(source, "Container"),
		Bitrate:      int64Value(source, "Bitrate"),
		RunTimeTicks: int64Value(source, "RunTimeTicks"),
	}

	rawStreams, _ := source["MediaStreams"].([]any)
	for _, rawStream := range rawStreams {
		stream, ok := rawStream.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case strings.EqualFold(stringValue(stream, "Type"), "Video") && report.VideoCodec == "":
			report.VideoCodec = stringValue(stream, "Codec")
			report.Width = intValue(stream, "Width")
			report.Height = intValue(stream, "Height")
		case strings.EqualFold(stringValue(stream, "Type"), "Audio") && report.AudioCodec == "":
			report.AudioCodec = stringValue(stream, "Codec")
			report.AudioChannels = intValue(stream, "Channels")
			report.AudioTitle = stringValue(stream, "DisplayTitle")
		}
	}

	return report
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
