package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"emby-transcoder/internal/config"
)

func TestDefaultConfigIsUsable(t *testing.T) {
	cfg := config.Default()

	if cfg.Server.Listen != ":8097" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Upstream.URL != "http://127.0.0.1:8096" {
		t.Fatalf("upstream url = %q", cfg.Upstream.URL)
	}
	if !cfg.Transcode.Enabled {
		t.Fatal("transcoding should be enabled by default")
	}
	if cfg.Transcode.FFmpegPath == "" {
		t.Fatal("ffmpeg path should default")
	}
	if cfg.Transcode.MaxSessions != 2 {
		t.Fatalf("max sessions = %d", cfg.Transcode.MaxSessions)
	}
	if cfg.Transcode.BufferPauseSeconds != 300 {
		t.Fatalf("buffer pause seconds = %d", cfg.Transcode.BufferPauseSeconds)
	}
	if cfg.Transcode.BufferResumeSeconds != 120 {
		t.Fatalf("buffer resume seconds = %d", cfg.Transcode.BufferResumeSeconds)
	}
	if cfg.Server.Debug {
		t.Fatal("server debug should default false")
	}
}

func TestLoadMergesJSONOverDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{
		"server": {"listen": ":9000", "debug": true},
		"upstream": {"url": "http://emby.local:8096"},
		"transcode": {"max_sessions": 4, "buffer_pause_seconds": 600, "buffer_resume_seconds": 90},
		"clients": [{"name": "yamby", "match": ["Yamby"], "transcode": true}]
	}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.Listen != ":9000" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Upstream.URL != "http://emby.local:8096" {
		t.Fatalf("upstream url = %q", cfg.Upstream.URL)
	}
	if cfg.Transcode.FFmpegPath == "" {
		t.Fatal("ffmpeg path should be preserved from defaults")
	}
	if cfg.Transcode.MaxSessions != 4 {
		t.Fatalf("max sessions = %d", cfg.Transcode.MaxSessions)
	}
	if cfg.Transcode.BufferPauseSeconds != 600 || cfg.Transcode.BufferResumeSeconds != 90 {
		t.Fatalf("buffer thresholds = %d/%d", cfg.Transcode.BufferPauseSeconds, cfg.Transcode.BufferResumeSeconds)
	}
	if !cfg.Server.Debug {
		t.Fatal("server debug should be loaded from config")
	}
	if len(cfg.Clients) != 1 || cfg.Clients[0].Name != "yamby" {
		t.Fatalf("clients = %+v", cfg.Clients)
	}
}

func TestLoadSupportsVAAPIHardwareDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{
		"upstream": {"url": "http://emby.local:8096"},
		"transcode": {"hardware_decode": "vaapi"}
	}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Transcode.HardwareDecode != "vaapi" {
		t.Fatalf("hardware decode = %q", cfg.Transcode.HardwareDecode)
	}
	if cfg.Transcode.HardwareDevice != "/dev/dri/renderD128" {
		t.Fatalf("hardware device = %q", cfg.Transcode.HardwareDevice)
	}
}

func TestLoadSupportsBooleanFalseHardwareDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{
		"upstream": {"url": "http://upstream.local"},
		"transcode": {"hardware_decode": false}
	}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Transcode.HardwareDecode != "false" {
		t.Fatalf("hardware decode = %q", cfg.Transcode.HardwareDecode)
	}
}
