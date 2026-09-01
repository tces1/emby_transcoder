package transcode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildFFmpegArgsAppliesLocalSeekBeforeInputAndKeepsOutputOffset(t *testing.T) {
	session := &Session{
		ID:                "item123",
		Dir:               t.TempDir(),
		StartTimeTicks:    2403356050,
		SegmentStartIndex: 60,
	}
	request := Request{InputURL: "http://upstream/stream"}

	args := buildFFmpegArgs(session, request)

	seekIndex := slices.Index(args, "-ss")
	if seekIndex < 0 {
		t.Fatalf("missing -ss in args: %v", args)
	}
	if args[seekIndex+1] != "240.335605" {
		t.Fatalf("seek value = %q", args[seekIndex+1])
	}
	inputIndex := slices.Index(args, "-i")
	if inputIndex < 0 {
		t.Fatalf("missing -i in args: %v", args)
	}
	if seekIndex > inputIndex {
		t.Fatalf("-ss should be an input seek before -i, args=%v", args)
	}

	offsetIndex := slices.Index(args, "-output_ts_offset")
	if offsetIndex < 0 {
		t.Fatalf("missing -output_ts_offset in args: %v", args)
	}
	if args[offsetIndex+1] != "240.335605" {
		t.Fatalf("offset value = %q", args[offsetIndex+1])
	}
	startNumberIndex := slices.Index(args, "-start_number")
	if startNumberIndex < 0 {
		t.Fatalf("missing -start_number in args: %v", args)
	}
	if args[startNumberIndex+1] != "60" {
		t.Fatalf("start number = %q", args[startNumberIndex+1])
	}
	listSizeIndex := slices.Index(args, "-hls_list_size")
	if listSizeIndex < 0 || args[listSizeIndex+1] != "0" {
		t.Fatalf("expected unbounded HLS list size, args=%v", args)
	}
	flagsIndex := slices.Index(args, "-hls_flags")
	if flagsIndex < 0 || args[flagsIndex+1] != "independent_segments+temp_file" {
		t.Fatalf("expected HLS temp_file flag, args=%v", args)
	}
	hlsTimeIndex := slices.Index(args, "-hls_time")
	if hlsTimeIndex < 0 || args[hlsTimeIndex+1] != "2" {
		t.Fatalf("expected default HLS segment duration, args=%v", args)
	}
	initTimeIndex := slices.Index(args, "-hls_init_time")
	if initTimeIndex < 0 || args[initTimeIndex+1] != "1" {
		t.Fatalf("expected first HLS segment to use hls_init_time 1, args=%v", args)
	}
	if args[len(args)-1] != filepath.Join(session.Dir, "master.m3u8") {
		t.Fatalf("playlist output = %q", args[len(args)-1])
	}
}

func TestBuildFFmpegArgsUsesConfiguredSegmentDuration(t *testing.T) {
	session := &Session{
		ID:           "item123",
		Dir:          t.TempDir(),
		SegmentTicks: 2 * timeSecondTicks,
	}

	args := buildFFmpegArgs(session, Request{InputURL: "http://upstream/stream"})

	hlsTimeIndex := slices.Index(args, "-hls_time")
	if hlsTimeIndex < 0 || args[hlsTimeIndex+1] != "2" {
		t.Fatalf("expected configured HLS segment duration, args=%v", args)
	}
	initTimeIndex := slices.Index(args, "-hls_init_time")
	if initTimeIndex < 0 || args[initTimeIndex+1] != "1" {
		t.Fatalf("expected first HLS segment shorter than configured duration, args=%v", args)
	}
}

func TestBuildFFmpegArgsDoesNotThrottleInputWithRealtimeFlag(t *testing.T) {
	session := &Session{
		ID:             "item123",
		Dir:            t.TempDir(),
		StartTimeTicks: 0,
	}

	args := buildFFmpegArgs(session, Request{InputURL: "http://upstream/stream"})

	if slices.Contains(args, "-re") {
		t.Fatalf("ffmpeg args should not include realtime throttling: %v", args)
	}
}

func TestExecProcessParsesFFmpegProgressSpeed(t *testing.T) {
	process := &execProcess{}
	process.readProgress(strings.NewReader("frame=10\nspeed=1.375x\nprogress=continue\n"))
	if got := process.TranscodeSpeed(); got != 1.375 {
		t.Fatalf("transcode speed = %v", got)
	}
}

func TestManagerStatusSnapshotReportsVideoAndUploadRate(t *testing.T) {
	manager := NewManager(Options{TempDir: t.TempDir()})
	session, err := manager.Ensure("item123", Request{
		InputURL: "http://upstream/video",
		Media:    MediaInfo{Name: "Example Movie"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.RecordSessionUpload(session, 2<<20)
	manager.mu.Lock()
	session.uploadSampleTime = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	statuses := manager.StatusSnapshot()
	if len(statuses) != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
	if statuses[0].VideoName != "Example Movie" || statuses[0].UploadBPS <= 0 || statuses[0].UploadedBytes != 2<<20 {
		t.Fatalf("status = %+v", statuses[0])
	}
	manager.Close()
}

func TestManagerStatusSnapshotReportsTranscodeBuffer(t *testing.T) {
	manager := NewManager(Options{TempDir: t.TempDir()})
	t.Cleanup(manager.Close)
	session, err := manager.Ensure("item123", Request{
		InputURL: "http://upstream/video",
		Media:    MediaInfo{Name: "Buffered Movie", RunTimeTicks: 120 * timeSecondTicks},
	})
	if err != nil {
		t.Fatal(err)
	}
	for segment := 0; segment <= 9; segment++ {
		path := filepath.Join(session.Dir, fmt.Sprintf("segment_%05d.ts", segment))
		if err := os.WriteFile(path, []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manager.RecordSegmentRequest(session.ID, 9)

	statuses := manager.StatusSnapshot()
	if len(statuses) != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
	status := statuses[0]
	if status.GeneratedSeconds != 20 || status.BufferSeconds != 2 {
		t.Fatalf("buffer status = %+v", status)
	}
	if status.RuntimeSeconds != 120 || status.BufferPauseSeconds != 300 || status.BufferResumeSeconds != 120 {
		t.Fatalf("buffer thresholds = %+v", status)
	}
}

func TestManagerStatusSnapshotIgnoresInFlightSegments(t *testing.T) {
	manager := NewManager(Options{TempDir: t.TempDir()})
	t.Cleanup(manager.Close)
	session, err := manager.Ensure("item123", Request{
		InputURL: "http://upstream/video",
		Media:    MediaInfo{Name: "Buffered Movie", RunTimeTicks: 120 * timeSecondTicks},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.RecordSegmentRequest(session.ID, 4)

	statuses := manager.StatusSnapshot()
	if len(statuses) != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
	status := statuses[0]
	if status.GeneratedSeconds != 0 || status.BufferSeconds != 0 {
		t.Fatalf("in-flight buffer should be empty: %+v", status)
	}
}

func TestManagerIgnoresUploadFromReplacedSession(t *testing.T) {
	manager := NewManager(Options{TempDir: t.TempDir()})
	first, err := manager.Ensure("item123", Request{InputURL: "http://upstream/video", StartTimeTicks: 0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Ensure("item123", Request{InputURL: "http://upstream/video", StartTimeTicks: 10_000_000})
	if err != nil {
		t.Fatal(err)
	}
	manager.RecordSessionUpload(first, 4096)
	manager.RecordSessionUpload(second, 2048)
	statuses := manager.StatusSnapshot()
	if len(statuses) != 1 || statuses[0].UploadedBytes != 2048 {
		t.Fatalf("statuses = %+v", statuses)
	}
	manager.Close()
}

func TestFFmpegRunnerUsesAcceleratedInputAndReleasesIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is unix-only")
	}
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	input := &recordingInputProxy{released: make(chan struct{})}
	runner := FFmpegRunner{Path: ffmpegPath, InputProxy: input}
	session := &Session{ID: "item123", Dir: dir}
	request := Request{
		InputURL: "http://upstream.local/video?X-Emby-Token=secret",
		Headers:  http.Header{"X-Emby-Token": []string{"secret"}},
	}

	process, err := runner.Start(context.Background(), session, request)
	if err != nil {
		t.Fatal(err)
	}
	execProcess, ok := process.(*execProcess)
	if !ok {
		t.Fatalf("process = %T", process)
	}
	if !execProcess.waitForExit(3 * time.Second) {
		t.Fatal("fake ffmpeg did not exit")
	}
	select {
	case <-input.released:
	case <-time.After(time.Second):
		t.Fatal("accelerated input registration was not released")
	}
	if input.rawURL != request.InputURL || input.headers.Get("X-Emby-Token") != "secret" {
		t.Fatalf("registered source = %q headers=%v", input.rawURL, input.headers)
	}
	logData, err := os.ReadFile(filepath.Join(dir, "ffmpeg.log"))
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "http://127.0.0.1:12345/input") {
		t.Fatalf("ffmpeg args do not use accelerated URL: %s", logText)
	}
	if strings.Contains(logText, "upstream.local") || strings.Contains(logText, "-headers") || strings.Contains(logText, "secret") {
		t.Fatalf("ffmpeg args leaked upstream source details: %s", logText)
	}
}

func TestFFmpegProcessIsNotDoneUntilAcceleratedInputIsReleased(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is unix-only")
	}
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	input := &blockingReleaseInputProxy{
		releaseStarted: make(chan struct{}),
		allowRelease:   make(chan struct{}),
	}
	runner := FFmpegRunner{Path: ffmpegPath, InputProxy: input}
	process, err := runner.Start(context.Background(), &Session{ID: "item123", Dir: dir}, Request{InputURL: "http://upstream/video"})
	if err != nil {
		t.Fatal(err)
	}
	execProcess := process.(*execProcess)

	select {
	case <-input.releaseStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("input release did not start")
	}
	if execProcess.waitForExit(20 * time.Millisecond) {
		t.Fatal("process reported done before input release completed")
	}
	close(input.allowRelease)
	if !execProcess.waitForExit(time.Second) {
		t.Fatal("process did not report done after input release")
	}
}

func TestBuildFFmpegArgsKeepsOutputLowLatencyWithoutUnsafeInputFlags(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}

	args := buildFFmpegArgs(session, Request{InputURL: "http://upstream/stream"})

	for _, want := range []string{"-tune", "zerolatency", "-g", "25", "-keyint_min", "25", "-sc_threshold", "0", "-bf", "0", "-hls_init_time", "1"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing low-latency arg %q in %v", want, args)
		}
	}
	for _, risky := range []string{"-fflags", "nobuffer", "-flags", "low_delay", "-analyzeduration", "-probesize"} {
		if slices.Contains(args, risky) {
			t.Fatalf("should not force risky probing arg %q for mkv/dts inputs: %v", risky, args)
		}
	}
}

type recordingInputProxy struct {
	rawURL   string
	headers  http.Header
	released chan struct{}
}

type blockingReleaseInputProxy struct {
	releaseStarted chan struct{}
	allowRelease   chan struct{}
}

func (p *blockingReleaseInputProxy) Register(string, http.Header) (string, func(), error) {
	return "http://127.0.0.1:12345/input", func() {
		close(p.releaseStarted)
		<-p.allowRelease
	}, nil
}

func (p *recordingInputProxy) Register(rawURL string, headers http.Header) (string, func(), error) {
	p.rawURL = rawURL
	p.headers = headers.Clone()
	return "http://127.0.0.1:12345/input", func() {
		close(p.released)
	}, nil
}

func TestBuildFFmpegArgsAppliesVAAPIHardwareDecodeBeforeInput(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}
	request := Request{InputURL: "http://upstream/stream"}

	args := buildFFmpegArgs(session, request, FFmpegOptions{
		HardwareDecode:   "vaapi",
		HardwareDevice:   "/dev/dri/renderD128",
		HardwarePipeline: "vaapi-full",
	})

	hwaccelIndex := slices.Index(args, "-hwaccel")
	if hwaccelIndex < 0 || args[hwaccelIndex+1] != "vaapi" {
		t.Fatalf("missing VAAPI hwaccel args: %v", args)
	}
	deviceIndex := slices.Index(args, "-hwaccel_device")
	if deviceIndex < 0 || args[deviceIndex+1] != "/dev/dri/renderD128" {
		t.Fatalf("missing VAAPI device args: %v", args)
	}
	inputIndex := slices.Index(args, "-i")
	if inputIndex < 0 {
		t.Fatalf("missing -i in args: %v", args)
	}
	if hwaccelIndex > inputIndex || deviceIndex > inputIndex {
		t.Fatalf("hardware decode args should be input options before -i: %v", args)
	}
}

func TestBuildFFmpegArgsUsesFullVAAPITranscodePipeline(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}
	request := Request{InputURL: "http://upstream/stream"}

	args := buildFFmpegArgs(session, request, FFmpegOptions{
		HardwareDecode: "vaapi",
		HardwareDevice: "/dev/dri/renderD128",
	})

	outputFormatIndex := slices.Index(args, "-hwaccel_output_format")
	if outputFormatIndex < 0 || args[outputFormatIndex+1] != "vaapi" {
		t.Fatalf("missing VAAPI hardware frame output: %v", args)
	}
	if slices.Contains(args, "-vaapi_device") {
		t.Fatalf("full VAAPI pipeline must not initialize a second device: %v", args)
	}
	codecIndex := slices.Index(args, "-c:v")
	if codecIndex < 0 || args[codecIndex+1] != "h264_vaapi" {
		t.Fatalf("expected VAAPI H.264 encoder, args=%v", args)
	}
	vfIndex := slices.Index(args, "-vf")
	if vfIndex < 0 || args[vfIndex+1] != "scale_vaapi=format=nv12" {
		t.Fatalf("full VAAPI pipeline should convert surfaces to NV12 with VAAPI, args=%v", args)
	}
	for _, softwareOnly := range []string{"libx264", "-preset", "-tune", "-level"} {
		if slices.Contains(args, softwareOnly) {
			t.Fatalf("VAAPI pipeline should not include software encoder arg %q: %v", softwareOnly, args)
		}
	}
}

func TestBuildFFmpegArgsUsesVAAPIEncodeFallbackPipeline(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}
	request := Request{InputURL: "http://upstream/stream"}

	args := buildFFmpegArgs(session, request, FFmpegOptions{
		HardwareDecode:   "vaapi",
		HardwareDevice:   "/dev/dri/renderD128",
		HardwarePipeline: "vaapi-encode",
	})

	if slices.Contains(args, "-hwaccel_output_format") {
		t.Fatalf("VAAPI encode fallback should not require VAAPI decoded frames: %v", args)
	}
	deviceIndex := slices.Index(args, "-vaapi_device")
	if deviceIndex < 0 || args[deviceIndex+1] != "/dev/dri/renderD128" {
		t.Fatalf("missing VAAPI encode device: %v", args)
	}
	vfIndex := slices.Index(args, "-vf")
	if vfIndex < 0 || !strings.Contains(args[vfIndex+1], "scale=") || !strings.Contains(args[vfIndex+1], "hwupload") {
		t.Fatalf("expected software scale plus VAAPI upload filter, args=%v", args)
	}
	codecIndex := slices.Index(args, "-c:v")
	if codecIndex < 0 || args[codecIndex+1] != "h264_vaapi" {
		t.Fatalf("expected VAAPI H.264 encoder, args=%v", args)
	}
	if slices.Contains(args, "libx264") {
		t.Fatalf("VAAPI encode fallback should not use libx264: %v", args)
	}
}

func TestSelectHardwarePipelineUsesEncodeFallbackFor4KHEVCMain8(t *testing.T) {
	info := MediaInfo{
		VideoCodec:    "hevc",
		VideoProfile:  "Main",
		VideoBitDepth: 8,
		VideoPixFmt:   "yuv420p",
		Width:         3840,
		Height:        2160,
	}
	if got := selectHardwarePipeline(info, "vaapi-full"); got != "vaapi-encode" {
		t.Fatalf("pipeline = %q, want vaapi-encode", got)
	}
}

func TestBuildFFmpegArgsUsesVAAPIHybridPipelineForHEVCMain10(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
		Media: MediaInfo{
			VideoCodec:    "hevc",
			VideoProfile:  "Main 10",
			VideoBitDepth: 10,
			Width:         3840,
			Height:        1920,
		},
		HardwarePipeline: selectHardwarePipeline(MediaInfo{
			VideoCodec:    "hevc",
			VideoProfile:  "Main 10",
			VideoBitDepth: 10,
			Width:         3840,
			Height:        1920,
		}, "vaapi-full"),
	}
	request := Request{InputURL: "http://upstream/stream"}

	args := buildFFmpegArgs(session, request, effectiveFFmpegOptions(session, FFmpegOptions{
		HardwareDecode:   "vaapi",
		HardwareDevice:   "/dev/dri/renderD128",
		HardwarePipeline: "vaapi-full",
	}))

	if got := session.HardwarePipeline; got != "vaapi-hybrid" {
		t.Fatalf("pipeline = %q", got)
	}
	if hwaccelIndex := slices.Index(args, "-hwaccel"); hwaccelIndex < 0 || args[hwaccelIndex+1] != "vaapi" {
		t.Fatalf("missing VAAPI hardware decode: %v", args)
	}
	if outputFormatIndex := slices.Index(args, "-hwaccel_output_format"); outputFormatIndex < 0 || args[outputFormatIndex+1] != "vaapi" {
		t.Fatalf("missing VAAPI surface output: %v", args)
	}
	if slices.Contains(args, "-vaapi_device") {
		t.Fatalf("hybrid pipeline must not initialize a second VAAPI device: %v", args)
	}
	vfIndex := slices.Index(args, "-vf")
	if vfIndex < 0 {
		t.Fatalf("missing filter chain: %v", args)
	}
	filter := args[vfIndex+1]
	for _, want := range []string{"hwdownload", "format=p010le", "scale=", "format=nv12", "hwupload"} {
		if !strings.Contains(filter, want) {
			t.Fatalf("hybrid filter missing %q: %q", want, filter)
		}
	}
	codecIndex := slices.Index(args, "-c:v")
	if codecIndex < 0 || args[codecIndex+1] != "h264_vaapi" {
		t.Fatalf("expected VAAPI H.264 encoder, args=%v", args)
	}
}

func TestBuildFFmpegArgsDoesNotForceVAAPIH264Profile(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}
	request := Request{InputURL: "http://upstream/stream"}

	for _, pipeline := range []string{"vaapi-full", "vaapi-encode"} {
		t.Run(pipeline, func(t *testing.T) {
			args := buildFFmpegArgs(session, request, FFmpegOptions{
				HardwareDecode:   "vaapi",
				HardwareDevice:   "/dev/dri/renderD128",
				HardwarePipeline: pipeline,
			})

			if slices.Contains(args, "-profile:v") {
				t.Fatalf("VAAPI pipeline should not force H.264 profile: %v", args)
			}
		})
	}
}

func TestBuildFFmpegArgsUsesVAAPILowPowerEncoding(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}
	request := Request{InputURL: "http://upstream/stream"}

	for _, pipeline := range []string{"vaapi-full", "vaapi-encode"} {
		t.Run(pipeline, func(t *testing.T) {
			args := buildFFmpegArgs(session, request, FFmpegOptions{
				HardwareDecode:   "vaapi",
				HardwareDevice:   "/dev/dri/renderD128",
				HardwarePipeline: pipeline,
			})

			lowPowerIndex := slices.Index(args, "-low_power")
			if lowPowerIndex < 0 || args[lowPowerIndex+1] != "1" {
				t.Fatalf("VAAPI pipeline should request low-power encoding: %v", args)
			}
		})
	}
}

func TestEffectiveFFmpegOptionsUsesSessionPipelineOverride(t *testing.T) {
	options := effectiveFFmpegOptions(&Session{HardwarePipeline: "vaapi-encode"}, FFmpegOptions{
		HardwareDecode:   "vaapi",
		HardwareDevice:   "/dev/dri/renderD128",
		HardwarePipeline: "vaapi-full",
	})

	if options.HardwarePipeline != "vaapi-encode" {
		t.Fatalf("pipeline = %q", options.HardwarePipeline)
	}

	args := buildFFmpegArgs(&Session{ID: "item123", Dir: t.TempDir()}, Request{InputURL: "http://upstream/stream"}, options)
	if slices.Contains(args, "-hwaccel") {
		t.Fatalf("fallback pipeline should not use VAAPI hardware decode: %v", args)
	}
	if deviceIndex := slices.Index(args, "-vaapi_device"); deviceIndex < 0 || args[deviceIndex+1] != "/dev/dri/renderD128" {
		t.Fatalf("fallback pipeline should keep VAAPI encode device: %v", args)
	}
	vfIndex := slices.Index(args, "-vf")
	if vfIndex < 0 || !strings.Contains(args[vfIndex+1], "format=nv12,hwupload") {
		t.Fatalf("fallback pipeline should upload software-decoded frames: %v", args)
	}
}

func TestBuildFFmpegArgsCapsVideoOutputTo1080p(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}

	args := buildFFmpegArgs(session, Request{InputURL: "http://upstream/stream"})

	vfIndex := slices.Index(args, "-vf")
	if vfIndex < 0 {
		t.Fatalf("missing video scale filter: %v", args)
	}
	got := args[vfIndex+1]
	if !strings.Contains(got, "scale=") {
		t.Fatalf("expected scale filter, got %q", got)
	}
	if !strings.Contains(got, "1920") || !strings.Contains(got, "1080") {
		t.Fatalf("expected 1080p cap in scale filter, got %q", got)
	}
}

func TestBuildFFmpegArgsMapsFirstAudioStreamWhenNoAudioStreamIndexRequested(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
		Media: MediaInfo{
			AudioStreams: []AudioStreamInfo{
				{Index: 1, Ordinal: 0, Codec: "dts"},
				{Index: 2, Ordinal: 1, Codec: "aac"},
			},
		},
	}
	request := Request{InputURL: "http://upstream/stream"}

	args := buildFFmpegArgs(session, request)

	mapIndexes := allIndexes(args, "-map")
	if len(mapIndexes) < 2 {
		t.Fatalf("missing map args: %v", args)
	}
	if got := args[mapIndexes[1]+1]; got != "0:a:0?" {
		t.Fatalf("audio map = %q, args=%v", got, args)
	}
}

func TestBuildFFmpegArgsMapsRequestedAudioStreamIndex(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
		Media: MediaInfo{
			AudioStreams: []AudioStreamInfo{
				{Index: 1, Ordinal: 0, Codec: "aac"},
				{Index: 2, Ordinal: 1, Codec: "eac3"},
			},
		},
	}
	request := Request{
		InputURL:         "http://upstream/stream?AudioStreamIndex=2",
		AudioStreamIndex: 2,
	}

	args := buildFFmpegArgs(session, request)

	mapIndexes := allIndexes(args, "-map")
	if len(mapIndexes) < 2 {
		t.Fatalf("missing map args: %v", args)
	}
	if got := args[mapIndexes[1]+1]; got != "0:a:1?" {
		t.Fatalf("audio map = %q, args=%v", got, args)
	}
}

func TestBuildFFmpegArgsMapsExplicitAudioStreamIndexZero(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
		Media: MediaInfo{AudioStreams: []AudioStreamInfo{
			{Index: 2, Ordinal: 0, Codec: "aac"},
			{Index: 0, Ordinal: 1, Codec: "eac3"},
		}},
	}
	args := buildFFmpegArgs(session, Request{
		InputURL:            "http://upstream/stream?AudioStreamIndex=0",
		AudioStreamIndex:    0,
		HasAudioStreamIndex: true,
	})
	mapIndexes := allIndexes(args, "-map")
	if len(mapIndexes) < 2 {
		t.Fatalf("missing map args: %v", args)
	}
	if got := args[mapIndexes[1]+1]; got != "0:a:1?" {
		t.Fatalf("audio map = %q, args=%v", got, args)
	}
}

func allIndexes(values []string, needle string) []int {
	var indexes []int
	for i, value := range values {
		if value == needle {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func TestResolveHardwareDecodeKeepsVAAPIWhenProbePasses(t *testing.T) {
	options := resolveHardwareDecodeOptions("/usr/bin/ffmpeg", FFmpegOptions{
		HardwareDecode: "vaapi",
		HardwareDevice: "/dev/dri/renderD128",
	}, func(path string, options FFmpegOptions) error {
		if path != "/usr/bin/ffmpeg" {
			t.Fatalf("ffmpeg path = %q", path)
		}
		if options.HardwareDecode != "vaapi" {
			t.Fatalf("hardware decode = %q", options.HardwareDecode)
		}
		return nil
	})

	if options.HardwareDecode != "vaapi" || options.HardwareDevice != "/dev/dri/renderD128" || options.HardwarePipeline != "vaapi-full" {
		t.Fatalf("options = %+v", options)
	}
}

func TestResolveHardwareDecodeFallsBackToSoftwareWhenProbeFails(t *testing.T) {
	options := resolveHardwareDecodeOptions("/usr/bin/ffmpeg", FFmpegOptions{
		HardwareDecode: "vaapi",
		HardwareDevice: "/dev/dri/renderD128",
	}, func(string, FFmpegOptions) error {
		return errors.New("device not accessible")
	})

	if options.HardwareDecode != "" || options.HardwareDevice != "" {
		t.Fatalf("expected software fallback, options = %+v", options)
	}
}

func TestResolveHardwareDecodeFalseSkipsHardwareProbe(t *testing.T) {
	called := false
	options, err := resolveHardwareDecodeOptionsStrict("/definitely/missing/ffmpeg", FFmpegOptions{
		HardwareDecode: "false",
		HardwareDevice: "/dev/dri/renderD128",
	}, func(string, FFmpegOptions) error {
		called = true
		return errors.New("probe should not run")
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("hardware probe should not run when hardware decode is false")
	}
	if options.HardwareDecode != "" || options.HardwareDevice != "" {
		t.Fatalf("expected software options, got %+v", options)
	}
}

func TestDefaultHardwareProbeRejectsVAAPIWhenDeviceInitializationFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("linux-only ffmpeg probe test")
	}
	tempDir := t.TempDir()
	ffmpegPath := filepath.Join(tempDir, "ffmpeg")
	callsPath := filepath.Join(tempDir, "calls")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case " $* " in
  *" -hwaccels "*)
    printf 'Hardware acceleration methods:\nvaapi\n'
    exit 0
    ;;
  *" -encoders "*)
    printf ' V....D h264_vaapi           H.264/AVC (VAAPI)\n'
    exit 0
    ;;
  *" -init_hw_device "*)
    echo 'Failed to initialise VAAPI connection' >&2
    exit 1
    ;;
esac
exit 0
`, callsPath)
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	devicePath := filepath.Join(tempDir, "renderD128")
	if err := os.WriteFile(devicePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := defaultHardwareProbe(ffmpegPath, FFmpegOptions{
		HardwareDecode: "vaapi",
		HardwareDevice: devicePath,
	})

	if err == nil {
		t.Fatal("expected VAAPI device initialization failure")
	}
	if !strings.Contains(err.Error(), "vaapi device init probe failed") {
		t.Fatalf("error = %v", err)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "-init_hw_device vaapi=probe:"+devicePath) {
		t.Fatalf("ffmpeg calls should initialize the configured VAAPI device, calls=%s", calls)
	}
}

func TestDefaultHardwareProbeUsesVAAPIBaseOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("linux-only ffmpeg probe test")
	}
	tempDir := t.TempDir()
	ffmpegPath := filepath.Join(tempDir, "ffmpeg")
	callsPath := filepath.Join(tempDir, "calls")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case " $* " in
  *" -hwaccels "*)
    printf 'Hardware acceleration methods:\nvaapi\n'
    exit 0
    ;;
  *" -encoders "*)
    printf ' V....D h264_vaapi           H.264/AVC (VAAPI)\n'
    exit 0
    ;;
  *" -init_hw_device "*)
    exit 0
    ;;
esac
exit 0
`, callsPath)
	if err := os.WriteFile(ffmpegPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	devicePath := filepath.Join(tempDir, "renderD128")
	if err := os.WriteFile(devicePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := defaultHardwareProbe(ffmpegPath, FFmpegOptions{
		HardwareDecode: "vaapi",
		HardwareDevice: devicePath,
	}); err != nil {
		t.Fatal(err)
	}

	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-hwaccels", "-encoders", "-init_hw_device vaapi=probe:" + devicePath} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("expected hardware probe call containing %q, calls=%s", want, calls)
		}
	}
}

func TestNewManagerFallsBackToSoftwareDecodeWhenHardwareProbeFails(t *testing.T) {
	manager := NewManager(Options{
		FFmpegPath:     "/usr/bin/ffmpeg",
		HardwareDecode: "vaapi",
		HardwareDevice: "/dev/dri/renderD128",
		HardwareProbe: func(string, FFmpegOptions) error {
			return errors.New("device not accessible")
		},
	})

	runner, ok := manager.options.Runner.(FFmpegRunner)
	if !ok {
		t.Fatalf("runner = %T", manager.options.Runner)
	}
	if runner.Options.HardwareDecode != "" || runner.Options.HardwareDevice != "" {
		t.Fatalf("expected runner to use software decode, options = %+v", runner.Options)
	}
}

func TestFFmpegOptionsSummaryLabelsDecodeMode(t *testing.T) {
	if got := ffmpegOptionsSummary(FFmpegOptions{}); got != "software" {
		t.Fatalf("software summary = %q", got)
	}
	if got := ffmpegOptionsSummary(FFmpegOptions{HardwareDecode: "vaapi", HardwareDevice: "/dev/dri/renderD128", HardwarePipeline: "vaapi-full"}); got != "vaapi-full:/dev/dri/renderD128" {
		t.Fatalf("vaapi summary = %q", got)
	}
	if got := ffmpegOptionsSummary(FFmpegOptions{HardwareDecode: "vaapi", HardwareDevice: "/dev/dri/renderD128", HardwarePipeline: "vaapi-encode"}); got != "vaapi-encode:/dev/dri/renderD128" {
		t.Fatalf("vaapi encode summary = %q", got)
	}
}
