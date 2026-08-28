package inputproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyDownloadsRangesConcurrentlyAndInOrder(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 32*1024)
	var active atomic.Int32
	var maxActive atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "secret" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
		if !isProbeRequest(r) {
			current := active.Add(1)
			for {
				seen := maxActive.Load()
				if current <= seen || maxActive.CompareAndSwap(seen, current) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			defer active.Add(-1)
		}
		writeRange(w, data, start, end)
	}))
	defer upstream.Close()

	proxy, err := New(Options{Workers: 4, ChunkSize: 32 << 10, BufferSize: 128 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register(upstream.URL+"/video", http.Header{"X-Emby-Token": []string{"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("accelerated body differs: got=%d want=%d", len(body), len(data))
	}
	if maxActive.Load() < 2 {
		t.Fatalf("expected concurrent upstream ranges, max active = %d", maxActive.Load())
	}
	var downloaded int64
	for _, worker := range proxy.Snapshot() {
		downloaded += worker.TotalBytes
	}
	if downloaded != int64(len(data)) {
		t.Fatalf("worker metrics downloaded=%d want=%d", downloaded, len(data))
	}
}

func TestProxyDistributesTwoWorkersAcrossOrigins(t *testing.T) {
	data := bytes.Repeat([]byte("two-origin-media"), 32*1024)
	var firstChunks atomic.Int32
	var secondChunks atomic.Int32
	var thirdRequests atomic.Int32
	newOrigin := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
			if !isProbeRequest(r) {
				counter.Add(1)
			}
			writeRange(w, data, start, end)
		}))
	}
	first := newOrigin(&firstChunks)
	defer first.Close()
	second := newOrigin(&secondChunks)
	defer second.Close()
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdRequests.Add(1)
		start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
		writeRange(w, data, start, end)
	}))
	defer third.Close()

	proxy, err := New(Options{
		Workers:    2,
		ChunkSize:  32 << 10,
		BufferSize: 64 << 10,
		Origins:    []string{first.URL, second.URL, third.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register("https://original.invalid/video/file.mkv?token=value", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("two-origin body differs: got=%d want=%d", len(body), len(data))
	}
	if firstChunks.Load() == 0 || secondChunks.Load() == 0 {
		t.Fatalf("chunks were not distributed across both origins: first=%d second=%d", firstChunks.Load(), secondChunks.Load())
	}
	if thirdRequests.Load() != 0 {
		t.Fatalf("standby third origin received %d requests", thirdRequests.Load())
	}
}

func TestProxyUsesSampleFingerprintWhenValidatorsAreMissing(t *testing.T) {
	data := bytes.Repeat([]byte("fingerprinted-media"), 32*1024)
	var firstChunks atomic.Int32
	var secondChunks atomic.Int32
	newOrigin := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
			if !isProbeRequest(r) {
				counter.Add(1)
			}
			writeRangeWithoutValidator(w, data, start, end)
		}))
	}
	first := newOrigin(&firstChunks)
	defer first.Close()
	second := newOrigin(&secondChunks)
	defer second.Close()

	proxy, err := New(Options{
		Workers:    2,
		ChunkSize:  32 << 10,
		BufferSize: 64 << 10,
		Origins:    []string{first.URL, second.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register("https://original.invalid/video.mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("fingerprinted body differs: got=%d want=%d", len(body), len(data))
	}
	if firstChunks.Load() == 0 || secondChunks.Load() == 0 {
		t.Fatalf("fingerprinted routes were not both used: first=%d second=%d", firstChunks.Load(), secondChunks.Load())
	}
}

func TestProxyAssignsTwoSessionsToSeparatePriorityRoutes(t *testing.T) {
	data := bytes.Repeat([]byte("priority-routes"), 32*1024)
	var firstRouteTaskOne atomic.Int32
	var firstRouteTaskTwo atomic.Int32
	var secondRouteTaskOne atomic.Int32
	var secondRouteTaskTwo atomic.Int32
	newOrigin := func(taskOne, taskTwo *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
			if !isProbeRequest(r) {
				switch r.URL.Path {
				case "/task-one.mkv":
					taskOne.Add(1)
				case "/task-two.mkv":
					taskTwo.Add(1)
				}
			}
			writeRange(w, data, start, end)
		}))
	}
	firstOrigin := newOrigin(&firstRouteTaskOne, &firstRouteTaskTwo)
	defer firstOrigin.Close()
	secondOrigin := newOrigin(&secondRouteTaskOne, &secondRouteTaskTwo)
	defer secondOrigin.Close()

	proxy, err := New(Options{
		Workers:    2,
		ChunkSize:  32 << 10,
		BufferSize: 64 << 10,
		Origins:    []string{firstOrigin.URL, secondOrigin.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	firstURL, releaseFirst, err := proxy.Register("https://original.invalid/task-one.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	secondURL, releaseSecond, err := proxy.Register("https://original.invalid/task-two.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()

	var wg sync.WaitGroup
	errorsByRequest := make(chan error, 2)
	for _, localURL := range []string{firstURL, secondURL} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(localURL)
			if err != nil {
				errorsByRequest <- err
				return
			}
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			errorsByRequest <- err
		}()
	}
	wg.Wait()
	close(errorsByRequest)
	for err := range errorsByRequest {
		if err != nil {
			t.Fatal(err)
		}
	}
	if firstRouteTaskOne.Load() == 0 || secondRouteTaskTwo.Load() == 0 {
		t.Fatalf(
			"priority routes were not used: first/task1=%d second/task2=%d",
			firstRouteTaskOne.Load(),
			secondRouteTaskTwo.Load(),
		)
	}
	if firstRouteTaskTwo.Load() != 0 || secondRouteTaskOne.Load() != 0 {
		t.Fatalf(
			"sessions crossed assigned routes: first/task2=%d second/task1=%d",
			firstRouteTaskTwo.Load(),
			secondRouteTaskOne.Load(),
		)
	}

	releaseSecond()
	resp, err := http.Get(firstURL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if secondRouteTaskOne.Load() == 0 {
		t.Fatal("remaining session did not return to dual-route mode")
	}
}

func TestRouteFailureUsesStandbyWithoutStealingOtherSessionRoute(t *testing.T) {
	first := &source{
		active:   []string{"route-a", "route-b", "route-c"},
		failures: []int{2, 0, 0},
		order:    1,
	}
	second := &source{
		active:   []string{"route-a", "route-b", "route-c"},
		failures: []int{0, 0, 0},
		order:    2,
	}
	first.dedicated.Store(0)
	second.dedicated.Store(1)
	proxy := &Proxy{
		sources: map[string]*source{"first": first, "second": second},
	}

	assignments := proxy.routeAssignments()
	if assignments[first] != 2 || assignments[second] != 1 {
		t.Fatalf("assignments = first:%d second:%d", assignments[first], assignments[second])
	}
}

func TestProxyDistributesChunksAcrossEntrancesThatRedirectToMediaHosts(t *testing.T) {
	data := bytes.Repeat([]byte("redirected-media"), 32*1024)
	var firstChunks atomic.Int32
	var secondChunks atomic.Int32
	newMediaHost := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("api_key") != "test-key" {
				http.Error(w, "missing media key", http.StatusUnauthorized)
				return
			}
			start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
			if !isProbeRequest(r) {
				counter.Add(1)
			}
			writeRange(w, data, start, end)
		}))
	}
	firstMedia := newMediaHost(&firstChunks)
	defer firstMedia.Close()
	secondMedia := newMediaHost(&secondChunks)
	defer secondMedia.Close()
	newEntrance := func(mediaURL string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, mediaURL+r.URL.RequestURI(), http.StatusFound)
		}))
	}
	firstEntrance := newEntrance(firstMedia.URL)
	defer firstEntrance.Close()
	secondEntrance := newEntrance(secondMedia.URL)
	defer secondEntrance.Close()

	proxy, err := New(Options{
		Workers:    2,
		ChunkSize:  32 << 10,
		BufferSize: 64 << 10,
		Origins:    []string{firstEntrance.URL, secondEntrance.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register(
		"https://original.invalid/emby/videos/867699/original.mkv?api_key=test-key",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("redirected media body differs: got=%d want=%d", len(body), len(data))
	}
	if firstChunks.Load() == 0 || secondChunks.Load() == 0 {
		t.Fatalf("redirected chunks were not distributed: first=%d second=%d", firstChunks.Load(), secondChunks.Load())
	}
}

func TestProxySupportsFFmpegRangeRequests(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 32*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
		writeRange(w, data, start, end)
	}))
	defer upstream.Close()

	proxy, err := New(Options{Workers: 3, ChunkSize: 16 << 10, BufferSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register(upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	req, err := http.NewRequest(http.MethodGet, localURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=12345-98765")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes 12345-98765/%d", len(data)); got != want {
		t.Fatalf("content range = %q want %q", got, want)
	}
	if !bytes.Equal(body, data[12345:98766]) {
		t.Fatal("range body differs")
	}
}

func TestProxyBoundsPrefetchWindowWhenFirstChunkIsSlow(t *testing.T) {
	const chunkSize = 16 << 10
	data := bytes.Repeat([]byte("windowed"), 16*1024)
	firstChunkGate := make(chan struct{})
	var started atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
		if !isProbeRequest(r) {
			started.Add(1)
		}
		if start == 0 && end == chunkSize-1 {
			<-firstChunkGate
		}
		writeRange(w, data, start, end)
	}))
	defer upstream.Close()

	proxy, err := New(Options{Workers: 4, ChunkSize: chunkSize, BufferSize: 4 * chunkSize})
	if err != nil {
		t.Fatal(err)
	}
	if proxy.Workers() != 2 {
		t.Fatalf("workers = %d, want hard limit 2", proxy.Workers())
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register(upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		resp, err := http.Get(localURL)
		if err != nil {
			done <- err
			return
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			done <- readErr
			return
		}
		done <- closeErr
	}()

	deadline := time.Now().Add(time.Second)
	for started.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
	if got := started.Load(); got != 2 {
		close(firstChunkGate)
		t.Fatalf("prefetch window started %d chunks, want 2", got)
	}
	close(firstChunkGate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accelerated request did not finish")
	}
}

func TestProxyFallsBackWhenUpstreamIgnoresRanges(t *testing.T) {
	data := []byte("non-seekable upstream stream")
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "video/x-matroska")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer upstream.Close()

	proxy, err := New(Options{Workers: 4, ChunkSize: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register(upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("fallback status=%d body=%q", resp.StatusCode, body)
	}
	if requests.Load() != 2 {
		t.Fatalf("expected one range probe and one fallback request, got %d", requests.Load())
	}
}

func TestProxyLimitsConcurrentRangesAcrossRegisteredSources(t *testing.T) {
	const chunkSize = 16 << 10
	data := bytes.Repeat([]byte("shared-budget"), 16*1024)
	var active atomic.Int32
	var maxActive atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
		current := active.Add(1)
		for {
			seen := maxActive.Load()
			if current <= seen || maxActive.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		defer active.Add(-1)
		writeRange(w, data, start, end)
	}))
	defer upstream.Close()

	proxy, err := New(Options{Workers: 2, ChunkSize: chunkSize, BufferSize: 2 * chunkSize})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	firstURL, releaseFirst, err := proxy.Register(upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	secondURL, releaseSecond, err := proxy.Register(upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	requests := []struct {
		url        string
		rangeValue string
	}{
		{url: firstURL, rangeValue: "bytes=0-98303"},
		{url: secondURL, rangeValue: "bytes=98304-196607"},
	}
	for _, request := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, request.url, nil)
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("Range", request.rangeValue)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := maxActive.Load(); got < 2 || got > 2 {
		t.Fatalf("shared source concurrency = %d, want 2", got)
	}
}

func TestProxyRejectsChangedRepresentation(t *testing.T) {
	data := bytes.Repeat([]byte("versioned"), 1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
		etag := `"version-2"`
		if isProbeRequest(r) {
			etag = `"version-1"`
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer upstream.Close()

	proxy, err := New(Options{Workers: 2, ChunkSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	src := &source{rawURL: upstream.URL, headers: make(http.Header)}
	meta, supported, err := proxy.sourceMetadata(context.Background(), src)
	if err != nil || !supported {
		t.Fatalf("metadata supported=%t err=%v", supported, err)
	}
	if _, err := proxy.fetchRange(context.Background(), src, meta, byteRange{start: 0, end: 1023}); err == nil {
		t.Fatal("expected changed representation to be rejected")
	}
}

func TestProxyExcludesOriginWithDifferentRepresentation(t *testing.T) {
	data := bytes.Repeat([]byte("same-size"), 1024)
	newOrigin := func(etag string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
		}))
	}
	first := newOrigin(`"version-1"`)
	defer first.Close()
	second := newOrigin(`"version-2"`)
	defer second.Close()

	proxy, err := New(Options{Workers: 2, Origins: []string{first.URL, second.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	src := &source{
		rawURL:  first.URL,
		headers: make(http.Header),
		urls:    []string{first.URL, second.URL},
	}
	_, supported, err := proxy.sourceMetadata(context.Background(), src)
	if err != nil || !supported {
		t.Fatalf("metadata supported=%t err=%v", supported, err)
	}
	if len(src.active) != 1 || src.active[0] != first.URL {
		t.Fatalf("active routes = %v", src.active)
	}
}

func TestProxyStripsCredentialsOnCrossOriginRedirect(t *testing.T) {
	data := bytes.Repeat([]byte("redirected"), 1024)
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "" ||
			r.Header.Get("X-Emby-Authorization") != "" ||
			r.Header.Get("Referer") != "" {
			leaked.Store(true)
		}
		start, end := requestedBounds(t, r.Header.Get("Range"), int64(len(data)))
		writeRange(w, data, start, end)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	proxy, err := New(Options{Workers: 2, ChunkSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register(origin.URL+"/video?api_key=test-key", http.Header{
		"X-Emby-Token":         []string{"secret"},
		"X-Emby-Authorization": []string{`MediaBrowser Token="secret"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if leaked.Load() {
		t.Fatal("Emby credentials were forwarded to a cross-origin redirect")
	}
}

func TestReleaseRemovesRegisteredSource(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeRange(w, []byte("content"), 0, 0)
	}))
	defer upstream.Close()

	proxy, err := New(Options{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	localURL, release, err := proxy.Register(upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()

	resp, err := http.Get(localURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func requestedBounds(t *testing.T, value string, size int64) (int64, int64) {
	t.Helper()
	if value == "" {
		return 0, size - 1
	}
	start, end, partial, err := parseRequestRange(value, size)
	if err != nil || !partial {
		t.Fatalf("invalid test range %q: partial=%t err=%v", value, partial, err)
	}
	return start, end
}

func isProbeRequest(r *http.Request) bool {
	raw := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	parts := strings.SplitN(raw, "-", 2)
	if len(parts) != 2 {
		return false
	}
	start, startErr := strconv.ParseInt(parts[0], 10, 64)
	end, endErr := strconv.ParseInt(parts[1], 10, 64)
	return startErr == nil && endErr == nil && end-start+1 == probeSampleSize
}

func writeRange(w http.ResponseWriter, data []byte, start, end int64) {
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("ETag", `"test-etag"`)
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func writeRangeWithoutValidator(w http.ResponseWriter, data []byte, start, end int64) {
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func closeProxy(t *testing.T, proxy *Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
