package policy_test

import (
	"net/http"
	"testing"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/policy"
)

func TestShouldTranscodeMatchesUserAgent(t *testing.T) {
	headers := http.Header{"User-Agent": {"Yamby TV"}}
	result := policy.ShouldTranscode(headers, []config.ClientProfile{
		{Name: "yamby", Match: []string{"Yamby"}, Transcode: true},
	})

	if !result.Enabled || result.ProfileName != "yamby" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestShouldTranscodeMatchesEmbyAuthorization(t *testing.T) {
	headers := http.Header{"X-Emby-Authorization": {`MediaBrowser Client="Emby for Android TV", Device="Living Room"`}}
	result := policy.ShouldTranscode(headers, []config.ClientProfile{
		{Name: "android-tv", Match: []string{"emby for android tv"}, Transcode: true},
	})

	if !result.Enabled || result.ProfileName != "android-tv" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestShouldTranscodeRespectsDisabledProfile(t *testing.T) {
	headers := http.Header{"User-Agent": {"SenPlayer"}}
	result := policy.ShouldTranscode(headers, []config.ClientProfile{
		{Name: "senplayer", Match: []string{"SenPlayer"}, Transcode: false},
	})

	if result.Enabled {
		t.Fatalf("expected disabled result, got %+v", result)
	}
}

func TestShouldTranscodeSupportsWildcard(t *testing.T) {
	headers := http.Header{"User-Agent": {"Any Client"}}
	result := policy.ShouldTranscode(headers, []config.ClientProfile{
		{Name: "debug-all", Match: []string{"*"}, Transcode: true},
	})

	if !result.Enabled || result.ProfileName != "debug-all" {
		t.Fatalf("unexpected result: %+v", result)
	}
}
