package inputproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"emby-transcoder/internal/logging"
)

const (
	DownloadModeParallel = "parallel"
	DownloadModeFailover = "failover"

	defaultChunkSize   = 8 << 20
	defaultBufferSize  = 64 << 20
	maxWorkers         = 2
	probeTimeout       = 10 * time.Second
	sourceProbeTimeout = 15 * time.Second
	chunkTimeout       = 30 * time.Second
	probeSampleSize    = 64 << 10
	sizeProbeRange     = "bytes=0-0"
	probeRetryAttempts = 2
	chunkRetryAttempts = 3
	retryDelay         = 250 * time.Millisecond
	hedgeLossThreshold = 3
	hedgeChunkOffset   = 10
)

var (
	errInvalidRange = errors.New("invalid byte range")
	errHedgeLost    = errors.New("hedged chunk completed by another route")
)

type Options struct {
	Workers    int
	Mode       string
	ChunkSize  int64
	BufferSize int64
	Transport  http.RoundTripper
	Origins    []string
	CacheDir   string
}

// Proxy exposes a loopback-only HTTP endpoint to FFmpeg and downloads
// seekable upstream resources with a bounded window of concurrent ranges.
type Proxy struct {
	client         *http.Client
	workers        int
	mode           string
	chunkSize      int64
	listener       net.Listener
	server         *http.Server
	baseURL        string
	slots          chan struct{}
	origins        []*url.URL
	cacheDir       string
	prefetchChunks int

	mu      sync.RWMutex
	sources map[string]*source
	closed  bool
	nextID  atomic.Uint64

	metricsMu sync.Mutex
	metrics   []workerMetric
}

type source struct {
	id         string
	name       string
	generation uint64
	rawURL     string
	headers    http.Header
	urls       []string
	active     []string
	finalHosts []string
	order      uint64
	nextURL    atomic.Uint64

	metaMu      sync.Mutex
	meta        *metadata
	unsupported bool
	fallbackLog sync.Once

	routeMu       sync.Mutex
	failures      []int
	hedgeLoss     map[string]int
	routeChecks   map[string]routeCheck
	routeGates    map[string]chan struct{}
	failoverFocus string
	dedicated     atomic.Int32

	cacheMu     sync.Mutex
	cacheFile   *os.File
	cachePath   string
	cacheChunks map[int64]*cacheChunk

	closed         atomic.Bool
	recovering     atomic.Bool
	needsReplenish atomic.Bool
	expandDone     chan struct{}
	expandOnce     sync.Once
}

type WorkerSnapshot struct {
	ID           int     `json:"id"`
	State        string  `json:"state"`
	SessionID    string  `json:"session_id,omitempty"`
	GenerationID uint64  `json:"generation_id,omitempty"`
	VideoName    string  `json:"video_name,omitempty"`
	Route        string  `json:"route,omitempty"`
	ByteRange    string  `json:"byte_range,omitempty"`
	DownloadBPS  float64 `json:"download_bps"`
	TotalBytes   int64   `json:"total_bytes"`
	LastError    string  `json:"last_error,omitempty"`
}

type CacheRange struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	State string `json:"state"`
}

type CacheSnapshot struct {
	SessionID    string       `json:"session_id,omitempty"`
	GenerationID uint64       `json:"generation_id,omitempty"`
	VideoName    string       `json:"video_name,omitempty"`
	Size         int64        `json:"size"`
	CachedBytes  int64        `json:"cached_bytes"`
	PendingBytes int64        `json:"pending_bytes"`
	WindowBytes  int64        `json:"window_bytes"`
	ChunkSize    int64        `json:"chunk_size"`
	Ranges       []CacheRange `json:"ranges,omitempty"`
}

type RouteSnapshot struct {
	SessionID    string `json:"session_id,omitempty"`
	GenerationID uint64 `json:"generation_id,omitempty"`
	Entry        string `json:"entry"`
	Final        string `json:"final,omitempty"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Active       bool   `json:"active"`
	Failures     int    `json:"failures"`
	HedgeLosses  int    `json:"hedge_losses"`
}

type routeCheck struct {
	state  string
	final  string
	reason string
}

type workerMetric struct {
	token        uint64
	busy         bool
	state        string
	sessionID    string
	generationID uint64
	videoName    string
	route        string
	byteRange    string
	startedAt    time.Time
	lastEndedAt  time.Time
	lastSpeedBPS float64
	currentBytes int64
	totalBytes   int64
	lastError    string
}

type workerLease struct {
	index int
	token uint64
}

type metadata struct {
	size                int64
	contentType         string
	etag                string
	lastModified        string
	finalHost           string
	fingerprint         [sha256.Size]byte
	hasFingerprint      bool
	fingerprintHeadOnly bool
}

type byteRange struct {
	start       int64
	end         int64
	urlIndex    int
	workerIndex int
	fixedWorker bool
}

type chunkResult struct {
	index      int64
	lane       int
	routeIndex int
	err        error
}

type chunkTask struct {
	index      int64
	lane       int
	routeIndex int
	hedge      bool
}

type cacheChunk struct {
	done      chan struct{}
	completed bool
	err       error
	ctx       context.Context
	cancel    context.CancelFunc
	attempts  int
	routes    map[int]struct{}
}

func New(options Options) (*Proxy, error) {
	workers := options.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > maxWorkers {
		workers = maxWorkers
	}
	chunkSize := options.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	bufferSize := options.BufferSize
	if bufferSize <= 0 {
		bufferSize = defaultBufferSize
	}
	maxBufferedChunks := int(bufferSize / chunkSize)
	if maxBufferedChunks < 1 {
		maxBufferedChunks = 1
	}
	if workers > maxBufferedChunks {
		workers = maxBufferedChunks
	}
	mode := strings.ToLower(strings.TrimSpace(options.Mode))
	if mode == "" {
		mode = DownloadModeParallel
	}
	if mode != DownloadModeParallel && mode != DownloadModeFailover {
		return nil, fmt.Errorf("invalid download mode %q", options.Mode)
	}
	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	origins, err := parseOrigins(options.Origins)
	if err != nil {
		return nil, err
	}
	cacheDir := strings.TrimSpace(options.CacheDir)
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "emby-transcoder-input")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create input cache directory: %w", err)
	}
	cleanupStaleCacheFiles(cacheDir)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for accelerated input: %w", err)
	}
	proxy := &Proxy{
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: safeRedirect,
		},
		workers:        workers,
		mode:           mode,
		chunkSize:      chunkSize,
		listener:       listener,
		baseURL:        "http://" + listener.Addr().String(),
		slots:          make(chan struct{}, workers),
		origins:        origins,
		cacheDir:       cacheDir,
		prefetchChunks: maxBufferedChunks,
		sources:        make(map[string]*source),
		metrics:        make([]workerMetric, workers),
	}
	proxy.server = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = proxy.server.Serve(listener)
	}()
	return proxy, nil
}

func (p *Proxy) Workers() int {
	return p.workers
}

func (p *Proxy) Snapshot() []WorkerSnapshot {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	now := time.Now()
	snapshots := make([]WorkerSnapshot, len(p.metrics))
	for index, metric := range p.metrics {
		state := metric.state
		speed := metric.lastSpeedBPS
		if metric.busy {
			if elapsed := now.Sub(metric.startedAt).Seconds(); elapsed > 0 {
				speed = float64(metric.currentBytes) / elapsed
			}
		} else {
			state = "idle"
			if metric.lastError != "" && now.Sub(metric.lastEndedAt) <= 10*time.Second {
				state = "error"
			}
			if now.Sub(metric.lastEndedAt) > 10*time.Second {
				speed = 0
			}
		}
		snapshots[index] = WorkerSnapshot{
			ID:           index + 1,
			State:        state,
			SessionID:    metric.sessionID,
			GenerationID: metric.generationID,
			VideoName:    metric.videoName,
			Route:        metric.route,
			ByteRange:    metric.byteRange,
			DownloadBPS:  speed,
			TotalBytes:   metric.totalBytes,
			LastError:    metric.lastError,
		}
	}
	return snapshots
}

func (p *Proxy) SessionRoutes() map[string][]string {
	p.mu.RLock()
	sources := make([]*source, 0, len(p.sources))
	for _, src := range p.sources {
		sources = append(sources, src)
	}
	p.mu.RUnlock()

	routes := make(map[string][]string, len(sources))
	for _, src := range sources {
		if src.id == "" {
			continue
		}
		src.routeMu.Lock()
		hosts := uniqueRouteHosts(src.active, src.finalHosts)
		src.routeMu.Unlock()
		if len(hosts) > 0 {
			routes[src.id] = hosts
		}
	}
	return routes
}

func (p *Proxy) RouteSnapshots() []RouteSnapshot {
	p.mu.RLock()
	sources := make([]*source, 0, len(p.sources))
	for _, src := range p.sources {
		sources = append(sources, src)
	}
	p.mu.RUnlock()
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].order < sources[j].order
	})

	var snapshots []RouteSnapshot
	for _, src := range sources {
		focused := make(map[int]struct{})
		for _, index := range p.streamRouteIndexes(src) {
			focused[index] = struct{}{}
		}
		src.routeMu.Lock()
		for _, candidate := range src.urls {
			check := src.routeChecks[candidate]
			snapshot := RouteSnapshot{
				SessionID:    src.id,
				GenerationID: src.generation,
				Entry:        routeHost(candidate),
				Final:        check.final,
				State:        check.state,
				Reason:       check.reason,
				HedgeLosses:  src.hedgeLoss[candidate],
			}
			if snapshot.State == "" {
				snapshot.State = "pending"
				snapshot.Reason = "not_probed"
			}
			for index, active := range src.active {
				if active != candidate {
					continue
				}
				snapshot.Active = true
				if index < len(src.finalHosts) && src.finalHosts[index] != "" {
					snapshot.Final = src.finalHosts[index]
				}
				if index < len(src.failures) {
					snapshot.Failures = src.failures[index]
				}
				if snapshot.Failures >= 2 {
					snapshot.State = "unhealthy"
					snapshot.Reason = "consecutive_failures"
				} else if p.mode == DownloadModeFailover {
					if _, selected := focused[index]; !selected {
						snapshot.State = "standby"
						snapshot.Reason = "failover_standby"
					} else {
						snapshot.State = "active"
						snapshot.Reason = "accepted"
					}
				} else {
					snapshot.State = "active"
					snapshot.Reason = "accepted"
				}
				break
			}
			snapshots = append(snapshots, snapshot)
		}
		src.routeMu.Unlock()
	}
	return snapshots
}

func (p *Proxy) CacheSnapshots() []CacheSnapshot {
	p.mu.RLock()
	sources := make([]*source, 0, len(p.sources))
	for _, src := range p.sources {
		sources = append(sources, src)
	}
	p.mu.RUnlock()

	snapshots := make([]CacheSnapshot, 0, len(sources))
	for _, src := range sources {
		if snap, ok := p.cacheSnapshot(src); ok {
			snapshots = append(snapshots, snap)
		}
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].SessionID < snapshots[j].SessionID
	})
	return snapshots
}

func (p *Proxy) cacheSnapshot(src *source) (CacheSnapshot, bool) {
	src.metaMu.Lock()
	size := int64(0)
	if src.meta != nil {
		size = src.meta.size
	}
	src.metaMu.Unlock()

	src.cacheMu.Lock()
	defer src.cacheMu.Unlock()
	if src.cacheChunks == nil && size == 0 {
		return CacheSnapshot{}, false
	}

	ranges := make([]CacheRange, 0, len(src.cacheChunks))
	var cached, pending int64
	for index, chunk := range src.cacheChunks {
		if chunk == nil {
			continue
		}
		start := index * p.chunkSize
		end := start + p.chunkSize - 1
		if size > 0 && end >= size {
			end = size - 1
		}
		if end < start {
			continue
		}
		length := end - start + 1
		state := "downloading"
		if chunk.completed && chunk.err == nil {
			state = "cached"
			cached += length
		} else if !chunk.completed {
			pending += length
		} else {
			continue
		}
		ranges = append(ranges, CacheRange{Start: start, End: end, State: state})
	}
	ranges = downsampleCacheRanges(mergeCacheRanges(ranges), size, 160)
	return CacheSnapshot{
		SessionID:    src.id,
		GenerationID: src.generation,
		VideoName:    src.name,
		Size:         size,
		CachedBytes:  cached,
		PendingBytes: pending,
		WindowBytes:  int64(p.prefetchChunks) * p.chunkSize,
		ChunkSize:    p.chunkSize,
		Ranges:       ranges,
	}, true
}

func (p *Proxy) beginWorker(state string, src *source, rawURL string, rangeHeader string) workerLease {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	index := 0
	for candidate := range p.metrics {
		if !p.metrics[candidate].busy {
			index = candidate
			break
		}
	}
	return p.beginWorkerLocked(index, state, src, rawURL, rangeHeader)
}

func (p *Proxy) beginWorkerAt(index int, state string, src *source, rawURL string, rangeHeader string) workerLease {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	return p.beginWorkerLocked(index, state, src, rawURL, rangeHeader)
}

func (p *Proxy) beginWorkerLocked(index int, state string, src *source, rawURL string, rangeHeader string) workerLease {
	if index < 0 || index >= len(p.metrics) {
		return workerLease{index: -1}
	}
	metric := &p.metrics[index]
	metric.token++
	metric.busy = true
	metric.state = state
	metric.sessionID = src.id
	metric.generationID = src.generation
	metric.videoName = src.name
	metric.route = routeHost(rawURL)
	metric.byteRange = rangeHeader
	metric.startedAt = time.Now()
	metric.currentBytes = 0
	metric.lastError = ""
	return workerLease{index: index, token: metric.token}
}

func (p *Proxy) endWorker(lease workerLease, bytes int64, failed bool, finalURL string) {
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	if lease.index < 0 || lease.index >= len(p.metrics) {
		return
	}
	metric := &p.metrics[lease.index]
	if metric.token != lease.token {
		return
	}
	elapsed := time.Since(metric.startedAt).Seconds()
	if bytes > 0 && elapsed > 0 {
		metric.lastSpeedBPS = float64(bytes) / elapsed
		metric.totalBytes += bytes
	}
	if finalURL != "" {
		metric.route = routeHost(finalURL)
	}
	if failed {
		metric.lastError = "request failed"
	}
	metric.busy = false
	metric.state = "idle"
	metric.lastEndedAt = time.Now()
}

func isWorkerFailure(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled)
}

func (p *Proxy) addWorkerBytes(lease workerLease, bytes int) {
	if bytes <= 0 {
		return
	}
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	if lease.index >= 0 && lease.index < len(p.metrics) && p.metrics[lease.index].token == lease.token {
		p.metrics[lease.index].currentBytes += int64(bytes)
	}
}

func (p *Proxy) clearSourceMetrics(src *source) {
	if src == nil {
		return
	}
	p.metricsMu.Lock()
	defer p.metricsMu.Unlock()
	for index := range p.metrics {
		metric := &p.metrics[index]
		if metric.sessionID != src.id || metric.generationID != src.generation {
			continue
		}
		nextToken := metric.token + 1
		*metric = workerMetric{token: nextToken, state: "idle"}
	}
}

func routeHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func canonicalHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if name, port, err := net.SplitHostPort(host); err == nil {
		if port == "80" || port == "443" {
			return name
		}
		return strings.ToLower(host)
	}
	return host
}

func uniqueRouteHosts(active, finalHosts []string) []string {
	seen := make(map[string]struct{}, len(active))
	hosts := make([]string, 0, len(active))
	for index, entry := range active {
		host := ""
		if index < len(finalHosts) {
			host = finalHosts[index]
		}
		if host == "" {
			host = routeHost(entry)
		}
		key := canonicalHost(host)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts
}

func mergeCacheRanges(ranges []CacheRange) []CacheRange {
	if len(ranges) <= 1 {
		return ranges
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start == ranges[j].Start {
			if ranges[i].State == ranges[j].State {
				return ranges[i].End < ranges[j].End
			}
			return ranges[i].State < ranges[j].State
		}
		return ranges[i].Start < ranges[j].Start
	})
	out := []CacheRange{ranges[0]}
	for _, next := range ranges[1:] {
		last := &out[len(out)-1]
		if last.State == next.State && next.Start <= last.End+1 {
			if next.End > last.End {
				last.End = next.End
			}
			continue
		}
		out = append(out, next)
	}
	return out
}

func downsampleCacheRanges(ranges []CacheRange, size int64, limit int) []CacheRange {
	if limit <= 0 || len(ranges) <= limit {
		return ranges
	}
	if size <= 0 {
		return ranges[:limit]
	}
	bucket := size / int64(limit)
	if bucket < 1 {
		bucket = 1
	}
	out := make([]CacheRange, 0, limit)
	for start := int64(0); start < size; start += bucket {
		end := start + bucket - 1
		if end >= size {
			end = size - 1
		}
		state := ""
		for _, r := range ranges {
			if r.End < start || r.Start > end {
				continue
			}
			if r.State == "downloading" {
				state = "downloading"
				break
			}
			if r.State == "cached" {
				state = "cached"
			}
		}
		if state == "" {
			continue
		}
		out = append(out, CacheRange{Start: start, End: end, State: state})
	}
	return mergeCacheRanges(out)
}

type workerReader struct {
	reader io.Reader
	proxy  *Proxy
	worker workerLease
}

func (r workerReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.proxy.addWorkerBytes(r.worker, read)
	return read, err
}

// Register makes an upstream source available through an opaque loopback URL.
// The returned release function is safe to call more than once.
func (p *Proxy) Register(rawURL string, headers http.Header) (string, func(), error) {
	return p.RegisterSource("", "", 0, rawURL, headers)
}

func (p *Proxy) RegisterSource(id string, name string, generation uint64, rawURL string, headers http.Header) (string, func(), error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", nil, errors.New("invalid accelerated input URL")
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", nil, fmt.Errorf("create accelerated input token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	src := &source{
		id:          id,
		name:        name,
		generation:  generation,
		rawURL:      rawURL,
		headers:     upstreamHeaders(headers),
		urls:        p.sourceURLs(parsed),
		order:       p.nextID.Add(1),
		cacheChunks: make(map[int64]*cacheChunk),
		expandDone:  make(chan struct{}),
	}
	src.dedicated.Store(-1)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return "", nil, errors.New("accelerated input proxy is closed")
	}
	p.sources[token] = src
	p.rebalanceLocked()
	p.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			src.closed.Store(true)
			p.mu.Lock()
			delete(p.sources, token)
			p.rebalanceLocked()
			p.mu.Unlock()
			src.closeCache()
			src.markExpandDone()
			p.clearSourceMetrics(src)
		})
	}
	return p.baseURL + "/" + token, release, nil
}

func (p *Proxy) rebalanceLocked() {
	sources := make([]*source, 0, len(p.sources))
	for _, src := range p.sources {
		sources = append(sources, src)
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].order < sources[j].order
	})
	if len(sources) == 1 {
		sources[0].dedicated.Store(-1)
		return
	}
	for index, src := range sources {
		src.dedicated.Store(int32(index % maxWorkers))
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.Trim(strings.TrimSpace(r.URL.Path), "/")
	p.mu.RLock()
	src := p.sources[token]
	p.mu.RUnlock()
	if src == nil {
		http.NotFound(w, r)
		return
	}

	meta, supported, err := p.sourceMetadata(r.Context(), src)
	if err != nil || !supported {
		src.fallbackLog.Do(func() {
			reason := "range_or_validator_unsupported"
			if err != nil {
				reason = "range_probe_failed"
			}
			logging.Infof("accelerated input fallback source=%s reason=%s", sourceLabel(src.rawURL), reason)
		})
		p.serveFallback(w, r, src)
		return
	}
	p.serveAccelerated(w, r, src, meta)
}

func (p *Proxy) sourceMetadata(ctx context.Context, src *source) (metadata, bool, error) {
	src.metaMu.Lock()
	if src.expandDone == nil {
		src.expandDone = make(chan struct{})
	}
	if src.meta != nil {
		meta := *src.meta
		src.metaMu.Unlock()
		return meta, true, nil
	}
	if src.unsupported {
		src.metaMu.Unlock()
		return metadata{}, false, nil
	}

	candidates := src.urls
	if len(candidates) == 0 {
		candidates = []string{src.rawURL}
	}
	need := p.workers
	if need < 1 {
		need = 1
	}
	if need > len(candidates) {
		need = len(candidates)
	}

	probeCtx, cancel := context.WithTimeout(ctx, sourceProbeTimeout)
	selected, active, finalHosts, launched, lastErr := p.collectFirstRoute(probeCtx, src, candidates)
	cancel()
	if len(active) == 0 {
		if lastErr != nil {
			src.metaMu.Unlock()
			src.markExpandDone()
			return metadata{}, false, lastErr
		}
		src.unsupported = true
		src.metaMu.Unlock()
		src.markExpandDone()
		return metadata{}, false, nil
	}
	src.replaceRoutes(active, finalHosts)
	if err := src.prepareCache(p.cacheDir, selected.size); err != nil {
		src.metaMu.Unlock()
		src.markExpandDone()
		return metadata{}, false, err
	}
	src.meta = &selected
	if len(candidates) > 1 {
		logging.Infof(
			"accelerated input routes source=%s active=%d configured=%d",
			sourceLabel(src.rawURL),
			len(active),
			len(candidates),
		)
	}
	needExpand := need > 1
	remaining := remainingCandidates(candidates, active, launched)
	usedFinalHosts := map[string]struct{}{}
	if key := routeKey(finalHosts[0], active[0]); key != "" {
		usedFinalHosts[key] = struct{}{}
	}
	src.metaMu.Unlock()
	if needExpand {
		go p.expandRoutes(src, remaining, usedFinalHosts, need, len(candidates))
	} else {
		src.markExpandDone()
	}
	return selected, true, nil
}

type probeResult struct {
	candidate string
	meta      metadata
	supported bool
	err       error
}

func (p *Proxy) collectFirstRoute(ctx context.Context, src *source, candidates []string) (metadata, []string, []string, []string, error) {
	if len(candidates) == 0 {
		return metadata{}, nil, nil, nil, nil
	}
	results := make(chan probeResult, len(candidates))
	next := 0
	inFlight := 0
	launched := make([]string, 0, len(candidates))
	launch := func() {
		candidate := candidates[next]
		next++
		inFlight++
		launched = append(launched, candidate)
		go func() {
			meta, supported, err := p.probeMetadata(ctx, src, candidate)
			results <- probeResult{candidate: candidate, meta: meta, supported: supported, err: err}
		}()
	}
	batch := p.workers
	if batch < 1 {
		batch = 1
	}
	for inFlight < batch && next < len(candidates) {
		launch()
	}
	var lastErr error
	for inFlight > 0 {
		var result probeResult
		select {
		case result = <-results:
			inFlight--
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return metadata{}, nil, nil, launched, lastErr
		}
		if result.err != nil {
			lastErr = result.err
		} else if result.supported {
			return result.meta, []string{result.candidate}, []string{result.meta.finalHost}, launched, nil
		}
		if next < len(candidates) {
			launch()
		}
	}
	return metadata{}, nil, nil, launched, lastErr
}

func (p *Proxy) expandRoutes(src *source, candidates []string, usedFinalHosts map[string]struct{}, need, configured int) {
	defer src.markExpandDone()
	if src.closed.Load() || len(candidates) == 0 || need <= 1 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sourceProbeTimeout)
	defer cancel()
	if usedFinalHosts == nil {
		usedFinalHosts = map[string]struct{}{}
	}

	results := make(chan probeResult, len(candidates))
	next := 0
	inFlight := 0
	batch := p.workers
	if batch < 1 {
		batch = 1
	}
	launch := func() {
		candidate := candidates[next]
		next++
		inFlight++
		go func() {
			meta, supported, err := p.probeMetadata(ctx, src, candidate)
			results <- probeResult{candidate: candidate, meta: meta, supported: supported, err: err}
		}()
	}
	launchMore := func() {
		for next < len(candidates) && !src.closed.Load() {
			remainingNeed := need - activeCount(src)
			if remainingNeed <= 0 || inFlight >= remainingNeed || inFlight >= batch {
				return
			}
			launch()
		}
	}
	launchMore()
	for inFlight > 0 && !src.closed.Load() {
		var result probeResult
		select {
		case result = <-results:
			inFlight--
		case <-ctx.Done():
			return
		}
		if result.err == nil && result.supported {
			finalKey := routeKey(result.meta.finalHost, result.candidate)
			if finalKey != "" {
				if _, duplicate := usedFinalHosts[finalKey]; duplicate {
					src.recordRouteCheck(result.candidate, "duplicate", result.meta.finalHost, "same_final_host")
					logging.Infof(
						"accelerated input probe entry=%s final=%s result=duplicate_line",
						routeHost(result.candidate),
						result.meta.finalHost,
					)
				} else {
					recoverIndex := -1
					if index, recovering := src.failedRouteIndex(result.candidate); recovering {
						recoverIndex = index
					}
					if !p.adoptRoute(ctx, src, result.candidate, result.meta, recoverIndex) {
						launchMore()
						continue
					}
					usedFinalHosts[finalKey] = struct{}{}
					active := activeCount(src)
					logging.Infof(
						"accelerated input routes source=%s active=%d configured=%d",
						sourceLabel(src.rawURL),
						active,
						configured,
					)
					if active >= need {
						return
					}
				}
			}
		}
		launchMore()
	}
}

func (p *Proxy) adoptRoute(ctx context.Context, src *source, candidate string, next metadata, recoverIndex int) bool {
	src.routeMu.Lock()
	if len(src.active) == 0 {
		src.routeMu.Unlock()
		return false
	}
	firstURL := ""
	firstFinalHost := ""
	for index, candidate := range src.active {
		if index >= len(src.failures) || src.failures[index] < 2 {
			firstURL = candidate
			if index < len(src.finalHosts) {
				firstFinalHost = src.finalHosts[index]
			}
			break
		}
	}
	src.routeMu.Unlock()
	if firstURL == "" {
		return false
	}

	src.metaMu.Lock()
	if src.meta == nil {
		src.metaMu.Unlock()
		return false
	}
	first := *src.meta
	src.metaMu.Unlock()
	if firstFinalHost != "" {
		first.finalHost = firstFinalHost
	}

	if _, ok := mergeRepresentation(first, next); ok {
		src.acceptRoute(candidate, next.finalHost, recoverIndex)
		src.recordRouteCheck(candidate, "active", next.finalHost, "validator_matched")
		return true
	}
	if first.size != next.size {
		src.recordRouteCheck(candidate, "rejected", next.finalHost, "size_mismatch")
		return false
	}
	if strongETag(first.etag) && strongETag(next.etag) {
		src.recordRouteCheck(candidate, "rejected", next.finalHost, "etag_mismatch")
		return false
	}

	if src.closed.Load() {
		return false
	}
	fingerprintCtx, cancel := context.WithTimeout(context.Background(), sourceProbeTimeout)
	defer cancel()

	cacheFirst := src.hasInFlightChunks()
	fingerprintedFirst, used, err := p.ensureFingerprint(fingerprintCtx, src, firstURL, first, cacheFirst)
	if err != nil {
		src.recordRouteCheck(candidate, "rejected", next.finalHost, "primary_fingerprint_error")
		logging.Infof(
			"accelerated input probe entry=%s result=fingerprint_error detail=%q",
			routeHost(firstURL),
			transportErrorDetail(err),
		)
		return false
	}
	fingerprintedNext, err := p.fingerprintRanges(fingerprintCtx, src, candidate, next, used, false)
	if err != nil {
		src.recordRouteCheck(candidate, "rejected", next.finalHost, "candidate_fingerprint_error")
		logging.Infof(
			"accelerated input probe entry=%s result=fingerprint_error detail=%q",
			routeHost(candidate),
			transportErrorDetail(err),
		)
		return false
	}
	if _, ok := mergeRepresentation(fingerprintedFirst, fingerprintedNext); !ok {
		src.recordRouteCheck(candidate, "rejected", next.finalHost, "fingerprint_mismatch")
		return false
	}
	src.metaMu.Lock()
	if src.meta != nil && !src.meta.hasFingerprint {
		updated := fingerprintedFirst
		src.meta = &updated
	}
	src.metaMu.Unlock()
	src.acceptRoute(candidate, next.finalHost, recoverIndex)
	src.recordRouteCheck(candidate, "active", next.finalHost, "fingerprint_matched")
	return true
}

func remainingCandidates(candidates, active, delayed []string) []string {
	used := make(map[string]struct{}, len(active))
	for _, url := range active {
		used[url] = struct{}{}
	}
	hold := make(map[string]struct{}, len(delayed))
	for _, url := range delayed {
		hold[url] = struct{}{}
	}
	rest := make([]string, 0, len(candidates))
	later := make([]string, 0, len(delayed))
	for _, candidate := range candidates {
		if _, exists := used[candidate]; exists {
			continue
		}
		if _, paused := hold[candidate]; paused {
			later = append(later, candidate)
			continue
		}
		rest = append(rest, candidate)
	}
	return append(rest, later...)
}

func routeKey(finalHost, candidate string) string {
	key := canonicalHost(finalHost)
	if key == "" {
		key = canonicalHost(routeHost(candidate))
	}
	return key
}

func activeCount(src *source) int {
	src.routeMu.Lock()
	defer src.routeMu.Unlock()
	count := 0
	for index := range src.active {
		if index >= len(src.failures) || src.failures[index] < 2 {
			count++
		}
	}
	return count
}

func strongETag(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.HasPrefix(value, "W/")
}

func (p *Proxy) probeMetadata(ctx context.Context, src *source, rawURL string) (result metadata, supported bool, resultErr error) {
	src.recordRouteCheck(rawURL, "probing", "", "checking_range_support")
	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	req, err := p.upstreamRequest(requestCtx, http.MethodGet, src, rawURL, sizeProbeRange)
	if err != nil {
		cancel()
		src.recordRouteCheck(rawURL, "error", "", "invalid_request")
		return metadata{}, false, err
	}
	resp, err := p.doRequestWithRetry(requestCtx, req, probeRetryAttempts)
	if err != nil {
		cancel()
		reason := "request_error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			reason = "timeout"
		}
		src.recordRouteCheck(rawURL, "error", "", reason)
		logging.Infof(
			"accelerated input probe entry=%s result=%s detail=%q",
			routeHost(rawURL),
			reason,
			transportErrorDetail(err),
		)
		return metadata{}, false, err
	}
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	start, _, total, valid := parseContentRange(resp.Header.Get("Content-Range"))
	if resp.StatusCode != http.StatusPartialContent || !valid || start != 0 || total <= 0 {
		src.recordRouteCheck(rawURL, "unsupported", routeHost(finalURL), "range_unsupported")
		logging.Infof(
			"accelerated input probe entry=%s final=%s status=%d range=%t validator=false result=unsupported",
			routeHost(rawURL),
			routeHost(finalURL),
			resp.StatusCode,
			valid,
		)
		return metadata{}, false, nil
	}
	finalHost := routeHost(finalURL)
	if finalHost == "" {
		finalHost = routeHost(rawURL)
	}
	meta := metadata{
		size:         total,
		contentType:  resp.Header.Get("Content-Type"),
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
		finalHost:    finalHost,
	}
	_, _, hasValidator := representationValidator(meta)
	src.recordRouteCheck(rawURL, "ready", finalHost, "range_supported")
	logging.Infof(
		"accelerated input probe entry=%s final=%s status=%d range=true validator=%t fingerprint=false result=ready",
		routeHost(rawURL),
		routeHost(finalURL),
		resp.StatusCode,
		hasValidator,
	)
	return meta, true, nil
}

func (p *Proxy) ensureFingerprint(ctx context.Context, src *source, rawURL string, meta metadata, cacheFirst bool) (metadata, []byteRange, error) {
	if meta.hasFingerprint {
		return meta, fingerprintSampleRanges(meta.size, meta.fingerprintHeadOnly), nil
	}
	headOnly := false
	fromCache := false
	if cacheFirst {
		head := fingerprintSampleRanges(meta.size, true)
		if _, ok := p.readCachedSample(src, head[0]); !ok {
			p.waitCachedSample(ctx, src, head[0], 2*time.Second)
		}
		if _, ok := p.readCachedSample(src, head[0]); ok {
			headOnly = true
			fromCache = true
		}
	}
	ranges := fingerprintSampleRanges(meta.size, headOnly)
	fingerprint, err := p.hashFingerprint(ctx, src, rawURL, meta, ranges, fromCache)
	if err != nil {
		return metadata{}, nil, err
	}
	meta.fingerprint = fingerprint
	meta.hasFingerprint = true
	meta.fingerprintHeadOnly = headOnly
	return meta, ranges, nil
}

func (p *Proxy) fingerprintRanges(ctx context.Context, src *source, rawURL string, meta metadata, ranges []byteRange, fromCache bool) (metadata, error) {
	if meta.hasFingerprint {
		return meta, nil
	}
	if len(ranges) == 0 {
		ranges = fingerprintSampleRanges(meta.size, false)
	}
	fingerprint, err := p.hashFingerprint(ctx, src, rawURL, meta, ranges, fromCache)
	if err != nil {
		return metadata{}, err
	}
	meta.fingerprint = fingerprint
	meta.hasFingerprint = true
	meta.fingerprintHeadOnly = len(dedupeByteRanges(ranges)) == 1
	return meta, nil
}

func fingerprintSampleRanges(size int64, headOnly bool) []byteRange {
	if size <= 0 {
		return nil
	}
	sampleSize := int64(probeSampleSize)
	if sampleSize > size {
		sampleSize = size
	}
	ranges := []byteRange{{start: 0, end: sampleSize - 1}}
	if headOnly {
		return ranges
	}
	return dedupeByteRanges([]byteRange{
		ranges[0],
		{start: (size - sampleSize) / 2, end: (size-sampleSize)/2 + sampleSize - 1},
		{start: size - sampleSize, end: size - 1},
	})
}

func dedupeByteRanges(ranges []byteRange) []byteRange {
	seen := make(map[byteRange]struct{}, len(ranges))
	out := make([]byteRange, 0, len(ranges))
	for _, requested := range ranges {
		if _, duplicate := seen[requested]; duplicate {
			continue
		}
		seen[requested] = struct{}{}
		out = append(out, requested)
	}
	return out
}

func (p *Proxy) hashFingerprint(ctx context.Context, src *source, rawURL string, meta metadata, ranges []byteRange, fromCache bool) ([sha256.Size]byte, error) {
	hasher := sha256.New()
	for _, requested := range dedupeByteRanges(ranges) {
		var sample []byte
		if fromCache {
			cached, ok := p.readCachedSample(src, requested)
			if !ok {
				return [sha256.Size]byte{}, fmt.Errorf("fingerprint range %d-%d missing from cache", requested.start, requested.end)
			}
			sample = cached
		} else {
			var err error
			sample, err = p.fetchProbeSampleWithRetry(ctx, src, rawURL, meta, requested)
			if err != nil {
				return [sha256.Size]byte{}, err
			}
		}
		_, _ = fmt.Fprintf(hasher, "%d-%d:", requested.start, requested.end)
		_, _ = hasher.Write(sample)
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return fingerprint, nil
}

func (s *source) hasInFlightChunks() bool {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for _, chunk := range s.cacheChunks {
		if chunk != nil && !chunk.completed {
			return true
		}
	}
	return false
}

func (p *Proxy) readCachedSample(src *source, requested byteRange) ([]byte, bool) {
	if requested.end < requested.start || p.chunkSize <= 0 {
		return nil, false
	}
	first := requested.start / p.chunkSize
	last := requested.end / p.chunkSize
	length := requested.end - requested.start + 1
	src.cacheMu.Lock()
	defer src.cacheMu.Unlock()
	if src.cacheFile == nil {
		return nil, false
	}
	for index := first; index <= last; index++ {
		chunk, ok := src.cacheChunks[index]
		if !ok || !chunk.completed || chunk.err != nil {
			return nil, false
		}
	}
	buf := make([]byte, length)
	n, err := src.cacheFile.ReadAt(buf, requested.start)
	if err != nil || int64(n) != length {
		return nil, false
	}
	return buf, true
}

func (p *Proxy) waitCachedSample(ctx context.Context, src *source, requested byteRange, wait time.Duration) {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, ok := p.readCachedSample(src, requested); ok {
			return
		}
		select {
		case <-waitCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Proxy) fetchProbeSample(ctx context.Context, src *source, rawURL string, meta metadata, requested byteRange) ([]byte, error) {
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	rangeHeader := fmt.Sprintf("bytes=%d-%d", requested.start, requested.end)
	req, err := p.upstreamRequest(ctx, http.MethodGet, src, rawURL, rangeHeader)
	if err != nil {
		return nil, err
	}
	validatorHeader, validatorValue, hasValidator := representationValidator(meta)
	if hasValidator {
		req.Header.Set("If-Range", validatorValue)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("fingerprint range %s returned %s", rangeHeader, resp.Status)
	}
	start, end, total, valid := parseContentRange(resp.Header.Get("Content-Range"))
	if !valid || start != requested.start || end != requested.end || total != meta.size {
		return nil, fmt.Errorf("fingerprint range %s returned invalid content range", rangeHeader)
	}
	if hasValidator && resp.Header.Get(validatorHeader) != validatorValue {
		return nil, fmt.Errorf("representation changed during fingerprint probe")
	}
	if finalHost := routeHostFromResponse(resp, rawURL); meta.finalHost != "" && finalHost != meta.finalHost {
		return nil, fmt.Errorf("media route changed during fingerprint probe")
	}
	expected := requested.end - requested.start + 1
	sample, err := io.ReadAll(io.LimitReader(resp.Body, expected+1))
	if err != nil {
		return nil, err
	}
	if int64(len(sample)) != expected {
		return nil, fmt.Errorf("fingerprint range %s returned %d bytes, expected %d", rangeHeader, len(sample), expected)
	}
	return sample, nil
}

func (p *Proxy) fetchProbeSampleWithRetry(ctx context.Context, src *source, rawURL string, meta metadata, requested byteRange) ([]byte, error) {
	var lastErr error
	for attempt := range probeRetryAttempts {
		sample, err := p.fetchProbeSample(ctx, src, rawURL, meta, requested)
		if err == nil {
			return sample, nil
		}
		lastErr = err
		if attempt+1 < probeRetryAttempts {
			if err := waitForRetry(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
	}
	return nil, lastErr
}

func (p *Proxy) doRequestWithRetry(ctx context.Context, req *http.Request, attempts int) (*http.Response, error) {
	var lastErr error
	for attempt := range attempts {
		response, err := p.client.Do(req.Clone(ctx))
		if err == nil {
			return response, nil
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		lastErr = err
		if attempt+1 < attempts {
			if err := waitForRetry(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
	}
	return nil, lastErr
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func routeHostFromResponse(resp *http.Response, fallback string) string {
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		if host := routeHost(resp.Request.URL.String()); host != "" {
			return host
		}
	}
	return routeHost(fallback)
}

func transportErrorDetail(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *source) prepareCache(cacheDir string, size int64) error {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cacheFile != nil {
		return nil
	}
	file, err := os.CreateTemp(cacheDir, "input-*.cache")
	if err != nil {
		return fmt.Errorf("create sparse input cache: %w", err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return fmt.Errorf("size sparse input cache: %w", err)
	}
	s.cacheFile = file
	s.cachePath = file.Name()
	return nil
}

func (s *source) closeCache() {
	s.cacheMu.Lock()
	file := s.cacheFile
	path := s.cachePath
	s.cacheFile = nil
	s.cachePath = ""
	s.cacheChunks = make(map[int64]*cacheChunk)
	s.cacheMu.Unlock()
	if file != nil {
		_ = file.Close()
	}
	if path != "" {
		_ = os.Remove(path)
	}
}

func (p *Proxy) serveAccelerated(w http.ResponseWriter, r *http.Request, src *source, meta metadata) {
	start, end, partial, err := parseRequestRange(r.Header.Get("Range"), meta.size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", meta.size))
		http.Error(w, "requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	setMetadataHeaders(w.Header(), meta)
	w.Header().Set("Accept-Ranges", "bytes")
	if meta.size == 0 {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	if err := p.streamRanges(r.Context(), w, src, meta, start, end); err != nil && r.Context().Err() == nil {
		logging.Errorf(
			"accelerated input interrupted source=%s detail=%q",
			sourceLabel(src.rawURL),
			transportErrorDetail(err),
		)
	}
}

func (p *Proxy) streamRanges(ctx context.Context, dst io.Writer, src *source, meta metadata, start, end int64) error {
	if end < start {
		return nil
	}
	firstChunk := start / p.chunkSize
	lastChunk := end / p.chunkSize
	chunkCount := int(lastChunk-firstChunk) + 1
	routes := p.streamRouteIndexes(src)
	if len(routes) > chunkCount {
		routes = routes[:chunkCount]
	}

	workerCtx, cancel := context.WithCancel(ctx)
	tasks := make(chan chunkTask, len(routes))
	results := make(chan chunkResult)
	pending := make(map[int64]chunkResult, len(routes))
	var wg sync.WaitGroup
	for range routes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range tasks {
				err := p.ensureChunk(workerCtx, src, meta, task.index, task.lane, task.routeIndex, task.hedge)
				select {
				case results <- chunkResult{index: task.index, lane: task.lane, routeIndex: task.routeIndex, err: err}:
				case <-workerCtx.Done():
					return
				}
				if err != nil && !errors.Is(err, errHedgeLost) {
					return
				}
			}
		}()
	}
	defer func() {
		cancel()
		close(tasks)
		wg.Wait()
	}()

	nextWrite := firstChunk
	busyLanes := make(map[int]bool, len(routes))
	activeRoutes := make(map[int64]map[int]struct{}, len(routes))
	launch := func(index int64, lane int, routeIndex int, hedge bool) {
		busyLanes[lane] = true
		if activeRoutes[index] == nil {
			activeRoutes[index] = make(map[int]struct{}, len(routes))
		}
		activeRoutes[index][routeIndex] = struct{}{}
		tasks <- chunkTask{index: index, lane: lane, routeIndex: routeIndex, hedge: hedge}
	}
	nextAvailable := func(from int64) (int64, bool) {
		lastPrefetch := nextWrite + int64(p.prefetchChunks) - 1
		if lastPrefetch > lastChunk {
			lastPrefetch = lastChunk
		}
		for index := from; index <= lastPrefetch; index++ {
			if index < nextWrite {
				continue
			}
			if _, complete := pending[index]; complete {
				continue
			}
			if len(activeRoutes[index]) > 0 {
				continue
			}
			return index, true
		}
		return 0, false
	}
	schedule := func() {
		if current := activeRoutes[nextWrite]; len(current) > 0 {
			for lane, routeIndex := range routes {
				if busyLanes[lane] {
					continue
				}
				if src.hedgeOffset(routeIndex) > 0 {
					continue
				}
				if _, alreadyTrying := current[routeIndex]; alreadyTrying {
					continue
				}
				launch(nextWrite, lane, routeIndex, true)
				return
			}
		}
		for lane, routeIndex := range routes {
			if busyLanes[lane] {
				continue
			}
			from := nextWrite + src.hedgeOffset(routeIndex)
			index, ok := nextAvailable(from)
			if !ok {
				continue
			}
			launch(index, lane, routeIndex, false)
		}
	}
	schedule()

	for nextWrite <= lastChunk {
		select {
		case result := <-results:
			busyLanes[result.lane] = false
			if active := activeRoutes[result.index]; active != nil {
				delete(active, result.routeIndex)
				if len(active) == 0 {
					delete(activeRoutes, result.index)
				}
			}
			lostHedge := errors.Is(result.err, errHedgeLost)
			if result.err != nil && !lostHedge {
				return result.err
			}
			src.recordHedgeResult(result.routeIndex, lostHedge)
			if result.index >= nextWrite {
				pending[result.index] = result
			}
			for {
				ready, ok := pending[nextWrite]
				if !ok {
					break
				}
				if err := p.copyCachedChunk(dst, src, ready.index, start, end); err != nil {
					return err
				}
				delete(pending, nextWrite)
				nextWrite++
			}
			schedule()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (p *Proxy) ensureChunk(ctx context.Context, src *source, meta metadata, index int64, lane int, routeIndex int, hedge bool) error {
	for {
		src.cacheMu.Lock()
		if src.cacheFile == nil {
			src.cacheMu.Unlock()
			return errors.New("input cache is closed")
		}
		if existing, ok := src.cacheChunks[index]; ok {
			if existing.completed {
				if existing.err == nil {
					src.cacheMu.Unlock()
					return nil
				}
				delete(src.cacheChunks, index)
			} else {
				if hedge {
					if _, alreadyTrying := existing.routes[routeIndex]; !alreadyTrying {
						existing.attempts++
						existing.routes[routeIndex] = struct{}{}
						src.cacheMu.Unlock()
						return p.runChunkAttempt(ctx, src, meta, index, lane, routeIndex, existing)
					}
				}
				done := existing.done
				src.cacheMu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		chunkCtx, cancel := context.WithCancel(ctx)
		chunk := &cacheChunk{
			done:     make(chan struct{}),
			ctx:      chunkCtx,
			cancel:   cancel,
			attempts: 1,
			routes:   map[int]struct{}{routeIndex: {}},
		}
		src.cacheChunks[index] = chunk
		src.cacheMu.Unlock()
		return p.runChunkAttempt(ctx, src, meta, index, lane, routeIndex, chunk)
	}
}

func (p *Proxy) runChunkAttempt(ctx context.Context, src *source, meta metadata, index int64, lane int, routeIndex int, chunk *cacheChunk) error {
	err := p.downloadChunk(chunk.ctx, src, meta, index, lane, routeIndex)
	src.cacheMu.Lock()
	if chunk.completed {
		finalErr := chunk.err
		src.cacheMu.Unlock()
		return chunkAttemptResult(err, finalErr)
	}
	chunk.attempts--
	delete(chunk.routes, routeIndex)
	if err == nil || chunk.attempts == 0 {
		chunk.completed = true
		chunk.err = err
		close(chunk.done)
		chunk.cancel()
		src.cacheMu.Unlock()
		return err
	}
	done := chunk.done
	src.cacheMu.Unlock()
	select {
	case <-done:
		src.cacheMu.Lock()
		finalErr := chunk.err
		src.cacheMu.Unlock()
		return chunkAttemptResult(err, finalErr)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func chunkAttemptResult(attemptErr, finalErr error) error {
	if finalErr == nil && errors.Is(attemptErr, context.Canceled) {
		return errHedgeLost
	}
	return finalErr
}

func (p *Proxy) downloadChunk(ctx context.Context, src *source, meta metadata, index int64, lane int, routeIndex int) error {
	start := index * p.chunkSize
	end := start + p.chunkSize - 1
	if end >= meta.size {
		end = meta.size - 1
	}
	requested := byteRange{
		start:       start,
		end:         end,
		urlIndex:    routeIndex,
		workerIndex: p.workerMetricIndex(src, lane),
		fixedWorker: true,
	}
	data, err := p.fetchRange(ctx, src, meta, requested)
	if err != nil {
		return err
	}
	src.cacheMu.Lock()
	file := src.cacheFile
	src.cacheMu.Unlock()
	if file == nil {
		return errors.New("input cache is closed")
	}
	written, err := file.WriteAt(data, start)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (p *Proxy) copyCachedChunk(dst io.Writer, src *source, index int64, requestStart, requestEnd int64) error {
	chunkStart := index * p.chunkSize
	chunkEnd := chunkStart + p.chunkSize - 1
	readStart := chunkStart
	if readStart < requestStart {
		readStart = requestStart
	}
	readEnd := chunkEnd
	if readEnd > requestEnd {
		readEnd = requestEnd
	}
	if readEnd < readStart {
		return nil
	}
	src.cacheMu.Lock()
	file := src.cacheFile
	src.cacheMu.Unlock()
	if file == nil {
		return errors.New("input cache is closed")
	}
	length := readEnd - readStart + 1
	written, err := io.CopyN(dst, io.NewSectionReader(file, readStart, length), length)
	if err != nil {
		return err
	}
	if written != length {
		return io.ErrShortWrite
	}
	return nil
}

func (p *Proxy) routeLimit(src *source) int {
	if src.dedicated.Load() >= 0 {
		return 1
	}
	if len(p.origins) == 0 {
		return p.workers
	}
	healthy := src.healthyRouteIndexes()
	if len(healthy) > p.workers {
		return p.workers
	}
	if len(healthy) == 0 {
		return 1
	}
	return len(healthy)
}

func (p *Proxy) streamRouteIndexes(src *source) []int {
	healthy := src.healthyRouteIndexes()
	if len(healthy) == 0 {
		return []int{0}
	}
	if src.dedicated.Load() >= 0 {
		if assigned, ok := p.routeAssignments()[src]; ok {
			return []int{assigned}
		}
		return healthy[:1]
	}
	if p.mode == DownloadModeFailover {
		return []int{src.failoverRouteIndex()}
	}
	limit := p.routeLimit(src)
	if limit > len(healthy) {
		limit = len(healthy)
	}
	if len(p.origins) == 0 {
		limit = p.routeLimit(src)
	}
	routes := make([]int, limit)
	for index := range routes {
		routes[index] = healthy[index%len(healthy)]
	}
	return routes
}

func (p *Proxy) workerMetricIndex(src *source, lane int) int {
	if len(p.metrics) == 0 {
		return 0
	}
	if dedicated := src.dedicated.Load(); dedicated >= 0 {
		return int(dedicated) % len(p.metrics)
	}
	if lane < 0 {
		lane = 0
	}
	return lane % len(p.metrics)
}

func (p *Proxy) routeAssignments() map[*source]int {
	p.mu.RLock()
	sources := make([]*source, 0, len(p.sources))
	for _, src := range p.sources {
		if src.dedicated.Load() >= 0 {
			sources = append(sources, src)
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].order < sources[j].order
	})
	p.mu.RUnlock()

	type choice struct {
		index int
		rank  int
		key   string
	}
	type plan struct {
		src     *source
		desired int
		choices []choice
	}
	plans := make([]plan, 0, len(sources))
	reserved := make(map[string]*source, len(sources))
	for _, src := range sources {
		src.routeMu.Lock()
		ranks := make(map[string]int, len(src.urls))
		for index, candidate := range src.urls {
			ranks[candidate] = index
		}
		buildChoices := func(healthyOnly bool) []choice {
			var choices []choice
			for index, candidate := range src.active {
				if healthyOnly && index < len(src.failures) && src.failures[index] >= 2 {
					continue
				}
				rank, ok := ranks[candidate]
				if !ok {
					rank = index
				}
				final := ""
				if index < len(src.finalHosts) {
					final = src.finalHosts[index]
				}
				key := routeKey(final, candidate)
				if key == "" {
					key = candidate
				}
				choices = append(choices, choice{index: index, rank: rank, key: key})
			}
			sort.SliceStable(choices, func(i, j int) bool {
				return choices[i].rank < choices[j].rank
			})
			return choices
		}
		choices := buildChoices(true)
		if len(choices) == 0 {
			choices = buildChoices(false)
		}
		desiredRank := int(src.dedicated.Load())
		desired := -1
		for _, candidate := range choices {
			if candidate.rank == desiredRank {
				desired = candidate.index
				if candidate.key != "" {
					reserved[candidate.key] = src
				}
				break
			}
		}
		plans = append(plans, plan{src: src, desired: desired, choices: choices})
		src.routeMu.Unlock()
	}

	assignments := make(map[*source]int, len(plans))
	used := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		selected := choice{index: -1}
		for _, candidate := range plan.choices {
			if candidate.index == plan.desired {
				if _, occupied := used[candidate.key]; !occupied {
					selected = candidate
				}
				break
			}
		}
		if selected.index < 0 {
			for _, candidate := range plan.choices {
				if _, occupied := used[candidate.key]; occupied {
					continue
				}
				if owner, held := reserved[candidate.key]; held && owner != plan.src {
					continue
				}
				selected = candidate
				break
			}
		}
		if selected.index < 0 {
			for _, candidate := range plan.choices {
				if _, occupied := used[candidate.key]; !occupied {
					selected = candidate
					break
				}
			}
		}
		if selected.index < 0 && len(plan.choices) > 0 {
			selected = plan.choices[0]
		}
		if selected.index >= 0 {
			assignments[plan.src] = selected.index
			if selected.key != "" {
				used[selected.key] = struct{}{}
			}
		}
	}
	return assignments
}

func (s *source) healthyRouteIndexes() []int {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	indexes := make([]int, 0, len(s.active))
	for index := range s.active {
		if index >= len(s.failures) || s.failures[index] < 2 {
			indexes = append(indexes, index)
		}
	}
	if len(indexes) == 0 {
		for index := range s.active {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func (s *source) failoverRouteIndex() int {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	for index, candidate := range s.active {
		if candidate == s.failoverFocus &&
			(index >= len(s.failures) || s.failures[index] < 2) {
			return index
		}
	}
	for index, candidate := range s.active {
		if index >= len(s.failures) || s.failures[index] < 2 {
			s.failoverFocus = candidate
			return index
		}
	}
	return 0
}

func (s *source) setFailoverFocus(index int) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if index >= 0 && index < len(s.active) {
		s.failoverFocus = s.active[index]
	}
}

func (s *source) failedRouteIndex(candidate string) (int, bool) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	for index, active := range s.active {
		if active == candidate && index < len(s.failures) && s.failures[index] >= 2 {
			return index, true
		}
	}
	return 0, false
}

func (p *Proxy) fetchRange(ctx context.Context, src *source, meta metadata, requested byteRange) ([]byte, error) {
	if _, ok := src.routeURL(0); !ok {
		return nil, errors.New("no active accelerated input route")
	}
	var lastErr error
	for retry := range chunkRetryAttempts {
		for _, index := range p.routeAttempts(src, requested.urlIndex) {
			rawURL, ok := src.routeURL(index)
			if !ok {
				continue
			}
			data, err := p.fetchRangeFromURL(ctx, src, meta, rawURL, requested)
			if err == nil {
				src.recordRouteResult(index, true)
				if p.mode == DownloadModeFailover {
					src.setFailoverFocus(index)
				}
				if src.needsReplenish.Swap(false) {
					p.replenishRoutePool(src)
				}
				return data, nil
			}
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil, err
			}
			src.recordRouteResult(index, false)
			src.markRouteUnhealthy(index)
			src.needsReplenish.Store(true)
			logging.Infof(
				"accelerated input chunk route=%s range=%d-%d attempt=%d result=error detail=%q",
				routeHost(rawURL),
				requested.start,
				requested.end,
				retry+1,
				transportErrorDetail(err),
			)
		}
		if retry+1 < chunkRetryAttempts {
			if err := waitForRetry(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
	}
	return nil, lastErr
}

func (p *Proxy) routeAttempts(src *source, preferred int) []int {
	if p.mode == DownloadModeFailover && src.dedicated.Load() < 0 {
		preferred = src.failoverRouteIndex()
	}
	assignments := p.routeAssignments()
	src.routeMu.Lock()
	defer src.routeMu.Unlock()
	attempts := make([]int, 0, len(src.active))
	add := func(index int) {
		if index < 0 || index >= len(src.active) {
			return
		}
		for _, existing := range attempts {
			if existing == index {
				return
			}
		}
		attempts = append(attempts, index)
	}
	preferredHealthy := preferred >= 0 &&
		preferred < len(src.active) &&
		(preferred >= len(src.failures) || src.failures[preferred] < 2)
	if preferredHealthy {
		add(preferred)
		return attempts
	}
	if src.dedicated.Load() < 0 {
		for index := range src.active {
			if index >= len(src.failures) || src.failures[index] < 2 {
				add(index)
			}
		}
		add(preferred)
		return attempts
	}
	reserved := make(map[int]struct{}, len(assignments))
	for other, index := range assignments {
		if other != src {
			reserved[index] = struct{}{}
		}
	}
	for index := range src.active {
		if _, occupied := reserved[index]; occupied {
			continue
		}
		if index >= len(src.failures) || src.failures[index] < 2 {
			add(index)
		}
	}
	for index := range src.active {
		if index >= len(src.failures) || src.failures[index] < 2 {
			add(index)
		}
	}
	for index := range src.active {
		add(index)
	}
	return attempts
}

func (s *source) markExpandDone() {
	if s == nil || s.expandDone == nil {
		return
	}
	s.expandOnce.Do(func() {
		close(s.expandDone)
	})
}

func (s *source) replaceRoutes(active, finalHosts []string) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	s.active = append([]string{}, active...)
	s.finalHosts = append([]string{}, finalHosts...)
	s.failures = make([]int, len(active))
	if len(active) > 0 {
		s.failoverFocus = active[0]
	}
}

func (s *source) acceptRoute(rawURL, finalHost string, recoverIndex int) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if recoverIndex >= 0 &&
		recoverIndex < len(s.active) &&
		s.active[recoverIndex] == rawURL {
		if recoverIndex < len(s.finalHosts) {
			s.finalHosts[recoverIndex] = finalHost
		}
		if recoverIndex < len(s.failures) {
			s.failures[recoverIndex] = 0
		}
		return
	}
	s.active = append(append([]string{}, s.active...), rawURL)
	s.finalHosts = append(append([]string{}, s.finalHosts...), finalHost)
	s.failures = append(append([]int{}, s.failures...), 0)
}

func (s *source) routeURL(index int) (string, bool) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if index < 0 || index >= len(s.active) {
		return "", false
	}
	return s.active[index], true
}

func (s *source) acquireRoute(ctx context.Context, rawURL string) (func(), error) {
	s.routeMu.Lock()
	if len(s.active) < 2 && len(s.urls) <= 1 {
		s.routeMu.Unlock()
		return func() {}, nil
	}
	if s.routeGates == nil {
		s.routeGates = make(map[string]chan struct{})
	}
	gate := s.routeGates[rawURL]
	if gate == nil {
		gate = make(chan struct{}, 1)
		s.routeGates[rawURL] = gate
	}
	s.routeMu.Unlock()

	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *source) recordRouteResult(index int, success bool) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if index < 0 || index >= len(s.failures) {
		return
	}
	if success {
		s.failures[index] = 0
		return
	}
	s.failures[index]++
}

func (s *source) markRouteUnhealthy(index int) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if index < 0 || index >= len(s.failures) {
		return
	}
	if s.failures[index] < 2 {
		s.failures[index] = 2
	}
}

func (p *Proxy) replenishRoutePool(src *source) {
	if src == nil ||
		src.closed.Load() ||
		!src.recovering.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer src.recovering.Store(false)
		src.routeMu.Lock()
		active := append([]string(nil), src.active...)
		finalHosts := append([]string(nil), src.finalHosts...)
		candidates := append([]string(nil), src.urls...)
		failures := append([]int(nil), src.failures...)
		src.routeMu.Unlock()

		healthy := make(map[string]struct{}, len(active))
		for index, candidate := range active {
			if index >= len(failures) || failures[index] < 2 {
				healthy[candidate] = struct{}{}
			}
		}
		remaining := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			if _, inUse := healthy[candidate]; !inUse {
				remaining = append(remaining, candidate)
			}
		}
		if len(remaining) == 0 {
			return
		}
		usedFinalHosts := make(map[string]struct{}, len(active))
		for index, candidate := range active {
			if index < len(failures) && failures[index] >= 2 {
				continue
			}
			finalHost := ""
			if index < len(finalHosts) {
				finalHost = finalHosts[index]
			}
			if key := routeKey(finalHost, candidate); key != "" {
				usedFinalHosts[key] = struct{}{}
			}
		}
		logging.Infof(
			"accelerated input route replenish source=%s mode=%s candidates=%d",
			sourceLabel(src.rawURL),
			p.mode,
			len(remaining),
		)
		p.expandRoutes(src, remaining, usedFinalHosts, p.workers, len(candidates))
	}()
}

func (s *source) recordRouteCheck(rawURL, state, final, reason string) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if s.routeChecks == nil {
		s.routeChecks = make(map[string]routeCheck)
	}
	s.routeChecks[rawURL] = routeCheck{
		state:  state,
		final:  final,
		reason: reason,
	}
}

func (s *source) recordHedgeResult(index int, lost bool) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if index < 0 || index >= len(s.active) {
		return
	}
	key := s.active[index]
	if s.hedgeLoss == nil {
		s.hedgeLoss = make(map[string]int)
	}
	if lost {
		s.hedgeLoss[key]++
		return
	}
	delete(s.hedgeLoss, key)
}

func (s *source) hedgeOffset(index int) int64 {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if index < 0 || index >= len(s.active) || s.hedgeLoss == nil {
		return 0
	}
	if s.hedgeLoss[s.active[index]] >= hedgeLossThreshold {
		return hedgeChunkOffset
	}
	return 0
}

func (p *Proxy) fetchRangeFromURL(ctx context.Context, src *source, meta metadata, rawURL string, requested byteRange) (data []byte, resultErr error) {
	releaseRoute, err := src.acquireRoute(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer releaseRoute()
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()

	rangeHeader := fmt.Sprintf("bytes=%d-%d", requested.start, requested.end)
	requestCtx, cancel := context.WithTimeout(ctx, chunkTimeout)
	defer cancel()
	req, err := p.upstreamRequest(requestCtx, http.MethodGet, src, rawURL, rangeHeader)
	if err != nil {
		return nil, err
	}
	var workerID workerLease
	if requested.fixedWorker {
		workerID = p.beginWorkerAt(requested.workerIndex, "downloading", src, rawURL, rangeHeader)
	} else {
		workerID = p.beginWorker("downloading", src, rawURL, rangeHeader)
	}
	finalURL := ""
	defer func() {
		p.endWorker(workerID, int64(len(data)), isWorkerFailure(resultErr), finalURL)
	}()
	validatorHeader, validatorValue, hasValidator := representationValidator(meta)
	if hasValidator {
		req.Header.Set("If-Range", validatorValue)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("upstream range %s returned %s", rangeHeader, resp.Status)
	}
	start, end, total, valid := parseContentRange(resp.Header.Get("Content-Range"))
	if !valid || start != requested.start || end != requested.end || total != meta.size {
		return nil, fmt.Errorf("upstream returned invalid content range %q for %s", resp.Header.Get("Content-Range"), rangeHeader)
	}
	if hasValidator && resp.Header.Get(validatorHeader) != validatorValue {
		return nil, fmt.Errorf("upstream representation changed while fetching %s", rangeHeader)
	}
	expected := requested.end - requested.start + 1
	data, err = io.ReadAll(workerReader{
		reader: io.LimitReader(resp.Body, expected+1),
		proxy:  p,
		worker: workerID,
	})
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != expected {
		return nil, fmt.Errorf("upstream range %s returned %d bytes, expected %d", rangeHeader, len(data), expected)
	}
	return data, nil
}

func (p *Proxy) serveFallback(w http.ResponseWriter, r *http.Request, src *source) {
	req, err := p.upstreamRequest(r.Context(), r.Method, src, src.rawURL, r.Header.Get("Range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := p.acquire(r.Context()); err != nil {
		http.Error(w, "upstream request canceled", http.StatusBadGateway)
		return
	}
	defer p.release()
	workerID := p.beginWorker("forwarding", src, src.rawURL, r.Header.Get("Range"))
	var transferred int64
	failed := false
	finalURL := ""
	defer func() {
		p.endWorker(workerID, transferred, failed, finalURL)
	}()
	resp, err := p.client.Do(req)
	if err != nil {
		failed = isWorkerFailure(err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		transferred, err = io.Copy(w, workerReader{reader: resp.Body, proxy: p, worker: workerID})
		failed = err != nil
	}
}

func (p *Proxy) acquire(ctx context.Context) error {
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Proxy) release() {
	<-p.slots
}

func (p *Proxy) upstreamRequest(ctx context.Context, method string, src *source, rawURL string, rangeHeader string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = src.headers.Clone()
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Del("Range")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return req, nil
}

func (p *Proxy) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	shutdownErr := p.server.Shutdown(ctx)
	if shutdownErr != nil {
		_ = p.server.Close()
	}

	p.mu.Lock()
	sources := make([]*source, 0, len(p.sources))
	for _, src := range p.sources {
		sources = append(sources, src)
	}
	clear(p.sources)
	p.mu.Unlock()
	for _, src := range sources {
		src.closed.Store(true)
		src.closeCache()
		src.markExpandDone()
	}
	return shutdownErr
}

func cleanupStaleCacheFiles(cacheDir string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() ||
			!strings.HasPrefix(entry.Name(), "input-") ||
			!strings.HasSuffix(entry.Name(), ".cache") {
			continue
		}
		path := filepath.Join(cacheDir, entry.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logging.Errorf("remove stale input cache file=%s err=%v", entry.Name(), err)
		}
	}
}

func parseRequestRange(value string, size int64) (int64, int64, bool, error) {
	if strings.TrimSpace(value) == "" {
		if size == 0 {
			return 0, -1, false, nil
		}
		return 0, size - 1, false, nil
	}
	if size <= 0 || !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false, errInvalidRange
	}
	raw := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	if strings.Contains(raw, ",") {
		return 0, 0, false, errInvalidRange
	}
	parts := strings.SplitN(raw, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, errInvalidRange
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, errInvalidRange
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errInvalidRange
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errInvalidRange
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, nil
}

func parseContentRange(value string) (int64, int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}
	rangeAndSize := strings.SplitN(strings.TrimPrefix(value, "bytes "), "/", 2)
	if len(rangeAndSize) != 2 || rangeAndSize[1] == "*" {
		return 0, 0, 0, false
	}
	bounds := strings.SplitN(rangeAndSize[0], "-", 2)
	if len(bounds) != 2 {
		return 0, 0, 0, false
	}
	start, errStart := strconv.ParseInt(bounds[0], 10, 64)
	end, errEnd := strconv.ParseInt(bounds[1], 10, 64)
	total, errTotal := strconv.ParseInt(rangeAndSize[1], 10, 64)
	if errStart != nil || errEnd != nil || errTotal != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

func parseOrigins(values []string) ([]*url.URL, error) {
	origins := make([]*url.URL, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		origin, err := url.Parse(value)
		if err != nil ||
			origin.Host == "" ||
			(origin.Scheme != "http" && origin.Scheme != "https") ||
			(origin.Path != "" && origin.Path != "/") ||
			origin.RawQuery != "" ||
			origin.Fragment != "" ||
			origin.User != nil {
			return nil, fmt.Errorf("invalid download origin %q; expected scheme and host only", value)
		}
		origin.Path = ""
		key := strings.ToLower(origin.Scheme + "://" + origin.Host)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		origins = append(origins, origin)
	}
	return origins, nil
}

func (p *Proxy) sourceURLs(sourceURL *url.URL) []string {
	if len(p.origins) == 0 {
		return []string{sourceURL.String()}
	}
	urls := make([]string, 0, len(p.origins))
	for _, origin := range p.origins {
		candidate := *sourceURL
		candidate.Scheme = origin.Scheme
		candidate.Host = origin.Host
		urls = append(urls, candidate.String())
	}
	return urls
}

func mergeRepresentation(first, next metadata) (metadata, bool) {
	if first.size != next.size {
		return metadata{}, false
	}
	firstETag := strings.TrimSpace(first.etag)
	nextETag := strings.TrimSpace(next.etag)
	firstStrong := firstETag != "" && !strings.HasPrefix(firstETag, "W/")
	nextStrong := nextETag != "" && !strings.HasPrefix(nextETag, "W/")
	if firstStrong && nextStrong {
		if firstETag != nextETag {
			return metadata{}, false
		}
		return first, true
	}
	if first.lastModified != "" && first.lastModified == next.lastModified {
		first.etag = ""
		return first, true
	}
	if first.hasFingerprint && next.hasFingerprint && first.fingerprint == next.fingerprint {
		first.etag = ""
		first.lastModified = ""
		return first, true
	}
	return metadata{}, false
}

func representationValidator(meta metadata) (string, string, bool) {
	etag := strings.TrimSpace(meta.etag)
	if etag != "" && !strings.HasPrefix(etag, "W/") {
		return "ETag", etag, true
	}
	if lastModified := strings.TrimSpace(meta.lastModified); lastModified != "" {
		return "Last-Modified", lastModified, true
	}
	return "", "", false
}

func safeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 || sameOrigin(via[0].URL, req.URL) {
		return nil
	}
	for _, key := range []string{
		"Authorization",
		"Cookie",
		"Referer",
		"X-Emby-Authorization",
		"X-Emby-Token",
		"X-MediaBrowser-Token",
	} {
		req.Header.Del(key)
	}
	return nil
}

func sameOrigin(first, next *url.URL) bool {
	if first == nil || next == nil {
		return false
	}
	return strings.EqualFold(first.Scheme, next.Scheme) && strings.EqualFold(first.Host, next.Host)
}

func sourceLabel(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "unknown"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil
	return parsed.String()
}

func upstreamHeaders(headers http.Header) http.Header {
	out := make(http.Header)
	for _, key := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "User-Agent"} {
		if value := headers.Get(key); value != "" {
			out.Set(key, value)
		}
	}
	return out
}

func setMetadataHeaders(headers http.Header, meta metadata) {
	if meta.contentType != "" {
		headers.Set("Content-Type", meta.contentType)
	}
	if meta.etag != "" {
		headers.Set("ETag", meta.etag)
	}
	if meta.lastModified != "" {
		headers.Set("Last-Modified", meta.lastModified)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
