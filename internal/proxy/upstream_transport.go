package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"

	"emby-transcoder/internal/logging"
)

type failoverTransport struct {
	base      http.RoundTripper
	upstreams []*url.URL
	preferred atomic.Uint32
}

func newFailoverTransport(base http.RoundTripper, upstreams []*url.URL) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if len(upstreams) <= 1 {
		return base
	}
	return &failoverTransport{base: base, upstreams: upstreams}
}

func (t *failoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.upstreams) == 0 {
		return nil, errors.New("no upstream routes configured")
	}
	start := int(t.preferred.Load()) % len(t.upstreams)
	retryable := requestRetryable(req)
	var lastErr error

	for offset := range len(t.upstreams) {
		index := (start + offset) % len(t.upstreams)
		attempt, err := requestForUpstream(req, t.upstreams[index], offset > 0)
		if err != nil {
			return nil, err
		}
		resp, err := t.base.RoundTrip(attempt)
		if err == nil && resp == nil {
			err = errors.New("upstream transport returned no response")
		}
		if resp != nil {
			resp.Request = attempt
		}
		if err == nil && !retryableUpstreamStatus(resp.StatusCode) {
			if index != start {
				t.preferred.Store(uint32(index))
				logging.Infof("upstream failover selected host=%s", t.upstreams[index].Host)
			}
			return resp, nil
		}
		if err == nil && (!retryable || offset == len(t.upstreams)-1) {
			return resp, nil
		}
		if err != nil {
			lastErr = err
			if !retryable || offset == len(t.upstreams)-1 {
				return nil, err
			}
			continue
		}
		drainAndClose(resp.Body)
	}
	return nil, lastErr
}

func requestForUpstream(req *http.Request, upstream *url.URL, replay bool) (*http.Request, error) {
	attempt := req.Clone(req.Context())
	attemptURL := *req.URL
	attemptURL.Scheme = upstream.Scheme
	attemptURL.Host = upstream.Host
	attempt.URL = &attemptURL
	attempt.Host = upstream.Host
	if !replay {
		return attempt, nil
	}
	if req.Body == nil || req.Body == http.NoBody {
		attempt.Body = http.NoBody
		return attempt, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("upstream request body cannot be replayed")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	attempt.Body = body
	return attempt, nil
}

func requestRetryable(req *http.Request) bool {
	if req == nil {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	default:
		return false
	}
}

func retryableUpstreamStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
