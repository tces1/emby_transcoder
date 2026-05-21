package transcode

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
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
	hlsTimeIndex := slices.Index(args, "-hls_time")
	if hlsTimeIndex < 0 || args[hlsTimeIndex+1] != "1" {
		t.Fatalf("expected 1 second HLS segments, args=%v", args)
	}
	if args[len(args)-1] != filepath.Join(session.Dir, "master.m3u8") {
		t.Fatalf("playlist output = %q", args[len(args)-1])
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

func TestBuildFFmpegArgsAppliesVAAPIHardwareDecodeBeforeInput(t *testing.T) {
	session := &Session{
		ID:  "item123",
		Dir: t.TempDir(),
	}
	request := Request{InputURL: "http://upstream/stream"}

	args := buildFFmpegArgs(session, request, FFmpegOptions{
		HardwareDecode: "vaapi",
		HardwareDevice: "/dev/dri/renderD128",
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

	if options.HardwareDecode != "vaapi" || options.HardwareDevice != "/dev/dri/renderD128" {
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
	if got := ffmpegOptionsSummary(FFmpegOptions{HardwareDecode: "vaapi", HardwareDevice: "/dev/dri/renderD128"}); got != "vaapi:/dev/dri/renderD128" {
		t.Fatalf("vaapi summary = %q", got)
	}
}
