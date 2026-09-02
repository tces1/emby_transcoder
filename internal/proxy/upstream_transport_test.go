package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestFailoverTransportSwitchesAndKeepsHealthyRoute(t *testing.T) {
	routes := []*url.URL{
		mustParseUpstreamURL(t, "https://primary.example"),
		mustParseUpstreamURL(t, "https://backup.example"),
	}
	var mu sync.Mutex
	var hosts []string
	base := failoverRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		hosts = append(hosts, req.URL.Host)
		mu.Unlock()
		if req.URL.Host == "primary.example" {
			return nil, errors.New("primary unavailable")
		}
		return failoverResponse(http.StatusOK, "backup"), nil
	})
	transport := newFailoverTransport(base, routes)

	for range 2 {
		req, err := http.NewRequest(http.MethodGet, "https://primary.example/emby/System/Info", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(hosts, ","); got != "primary.example,backup.example,backup.example" {
		t.Fatalf("attempted hosts = %s", got)
	}
}

func TestFailoverTransportReplaysPlaybackInfoPostWhenBodyCanBeReplayed(t *testing.T) {
	routes := []*url.URL{
		mustParseUpstreamURL(t, "https://primary.example"),
		mustParseUpstreamURL(t, "https://backup.example"),
	}
	var backupBody string
	var hosts []string
	base := failoverRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		hosts = append(hosts, req.URL.Host)
		if req.URL.Host == "primary.example" {
			return nil, errors.New("EOF")
		}
		backupBody = string(body)
		return failoverResponse(http.StatusOK, "ok"), nil
	})
	transport := newFailoverTransport(base, routes)
	req, err := http.NewRequest(http.MethodPost, "https://primary.example/emby/Items/item/PlaybackInfo", strings.NewReader("request-body"))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || backupBody != "request-body" {
		t.Fatalf("status=%d backup body=%q hosts=%v", resp.StatusCode, backupBody, hosts)
	}
	if got := strings.Join(hosts, ","); got != "primary.example,backup.example" {
		t.Fatalf("attempted hosts = %s", got)
	}
}

func TestFailoverTransportDoesNotReplayStreamingPost(t *testing.T) {
	routes := []*url.URL{
		mustParseUpstreamURL(t, "https://primary.example"),
		mustParseUpstreamURL(t, "https://backup.example"),
	}
	calls := 0
	base := failoverRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		_ = req.Body.Close()
		return nil, errors.New("unavailable")
	})
	transport := newFailoverTransport(base, routes)
	req, err := http.NewRequest(http.MethodPost, "https://primary.example/emby/upload", io.NopCloser(strings.NewReader("stream")))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("expected primary error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

type failoverRoundTripper func(*http.Request) (*http.Response, error)

func (f failoverRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func mustParseUpstreamURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func failoverResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
