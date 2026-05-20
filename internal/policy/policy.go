package policy

import (
	"net/http"
	"strings"

	"emby-transcoder/internal/config"
)

type Result struct {
	Enabled     bool
	ProfileName string
}

func ShouldTranscode(headers http.Header, profiles []config.ClientProfile) Result {
	haystack := strings.ToLower(strings.Join([]string{
		headers.Get("User-Agent"),
		headers.Get("X-Emby-Authorization"),
		headers.Get("X-MediaBrowser-Token"),
	}, "\n"))

	for _, profile := range profiles {
		for _, needle := range profile.Match {
			needle = strings.ToLower(strings.TrimSpace(needle))
			if needle == "*" {
				return Result{Enabled: profile.Transcode, ProfileName: profile.Name}
			}
			if needle != "" && strings.Contains(haystack, needle) {
				return Result{Enabled: profile.Transcode, ProfileName: profile.Name}
			}
		}
	}
	return Result{}
}
