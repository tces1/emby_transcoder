//go:build !windows

package proxy

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"emby-transcoder/internal/config"
	"emby-transcoder/internal/transcode"
)

func TestSingleDownloadWorkerDoesNotInstallNilInputProxy(t *testing.T) {
	dir := t.TempDir()
	ffmpegPath := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Upstream.URL = "http://upstream.local"
	cfg.Transcode.DownloadWorkers = 1
	cfg.Transcode.FFmpegPath = ffmpegPath
	cfg.Transcode.TempDir = dir
	server, err := NewWithTransport(cfg, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	if _, err := server.transcodeManager.Ensure("item123", transcode.Request{
		InputURL: "http://upstream.local/video.mp4",
	}); err != nil {
		t.Fatal(err)
	}
}
