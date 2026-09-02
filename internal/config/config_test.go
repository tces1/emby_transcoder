package config_test

import (
	"os"
	"path/filepath"
	"strings"
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
	if cfg.Transcode.HardwareDevice != "/dev/dri/renderD128" {
		t.Fatalf("hardware device = %q", cfg.Transcode.HardwareDevice)
	}
	if cfg.Transcode.MaxSessions != 2 {
		t.Fatalf("max sessions = %d", cfg.Transcode.MaxSessions)
	}
	if cfg.Transcode.DownloadWorkers != 1 || cfg.Transcode.DownloadChunkMB != 8 || cfg.Transcode.DownloadBufferMB != 64 {
		t.Fatalf(
			"download defaults = workers:%d chunk_mb:%d buffer_mb:%d",
			cfg.Transcode.DownloadWorkers,
			cfg.Transcode.DownloadChunkMB,
			cfg.Transcode.DownloadBufferMB,
		)
	}
	if cfg.Transcode.BufferPauseSeconds != 300 {
		t.Fatalf("buffer pause seconds = %d", cfg.Transcode.BufferPauseSeconds)
	}
	if cfg.Transcode.BufferResumeSeconds != 120 {
		t.Fatalf("buffer resume seconds = %d", cfg.Transcode.BufferResumeSeconds)
	}
	if cfg.Transcode.SegmentSeconds != 2 {
		t.Fatalf("segment seconds = %d", cfg.Transcode.SegmentSeconds)
	}
	if cfg.Transcode.SegmentRetentionSeconds != 300 {
		t.Fatalf("segment retention seconds = %d", cfg.Transcode.SegmentRetentionSeconds)
	}
	if cfg.Server.Debug {
		t.Fatal("server debug should default false")
	}
}

func TestLoadMergesJSONOverDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{
		"server": {"listen": ":9000", "dashboard_password": "status-secret", "debug": true},
		"upstream": {"urls": ["http://emby.local:8096/", "http://emby-backup.local:8096", "http://emby.local:8096/"]},
		"transcode": {"max_sessions": 4, "download_workers": 6, "download_chunk_mb": 4, "download_buffer_mb": 32, "buffer_pause_seconds": 600, "buffer_resume_seconds": 90, "segment_seconds": 2, "segment_retention_seconds": 180},
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
	if cfg.Path != path {
		t.Fatalf("config path = %q", cfg.Path)
	}
	if cfg.Upstream.URL != "http://emby.local:8096" {
		t.Fatalf("upstream url = %q", cfg.Upstream.URL)
	}
	if len(cfg.Upstream.URLs) != 2 || cfg.Upstream.URLs[1] != "http://emby-backup.local:8096" {
		t.Fatalf("upstream urls = %v", cfg.Upstream.URLs)
	}
	if cfg.Transcode.FFmpegPath == "" {
		t.Fatal("ffmpeg path should be preserved from defaults")
	}
	if cfg.Transcode.MaxSessions != 4 {
		t.Fatalf("max sessions = %d", cfg.Transcode.MaxSessions)
	}
	if cfg.Transcode.DownloadWorkers != 2 || cfg.Transcode.DownloadChunkMB != 4 || cfg.Transcode.DownloadBufferMB != 32 {
		t.Fatalf(
			"download config = workers:%d chunk_mb:%d buffer_mb:%d",
			cfg.Transcode.DownloadWorkers,
			cfg.Transcode.DownloadChunkMB,
			cfg.Transcode.DownloadBufferMB,
		)
	}
	if cfg.Transcode.BufferPauseSeconds != 600 || cfg.Transcode.BufferResumeSeconds != 90 {
		t.Fatalf("buffer thresholds = %d/%d", cfg.Transcode.BufferPauseSeconds, cfg.Transcode.BufferResumeSeconds)
	}
	if cfg.Transcode.SegmentSeconds != 2 {
		t.Fatalf("segment seconds = %d", cfg.Transcode.SegmentSeconds)
	}
	if cfg.Transcode.SegmentRetentionSeconds != 180 {
		t.Fatalf("segment retention seconds = %d", cfg.Transcode.SegmentRetentionSeconds)
	}
	if !cfg.Server.Debug {
		t.Fatal("server debug should be loaded from config")
	}
	if cfg.Server.DashboardPassword != "status-secret" {
		t.Fatalf("dashboard password = %q", cfg.Server.DashboardPassword)
	}
	if len(cfg.Clients) != 1 || cfg.Clients[0].Name != "yamby" {
		t.Fatalf("clients = %+v", cfg.Clients)
	}
}

func TestSaveAndParseConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Upstream.URLs = []string{"https://one.example/", "https://two.example"}
	cfg.Server.DashboardPassword = "secret"

	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Upstream.URLs) != 2 || parsed.Upstream.URL != "https://one.example" {
		t.Fatalf("upstreams = %+v", parsed.Upstream)
	}
	if parsed.Server.DashboardPassword != "secret" {
		t.Fatalf("dashboard password = %q", parsed.Server.DashboardPassword)
	}
	if strings.Contains(string(data), `"url":`) {
		t.Fatalf("saved config contains redundant legacy upstream.url: %s", data)
	}
	if !strings.Contains(string(data), `"hardware_acceleration": false`) ||
		strings.Contains(string(data), `"hardware_decode"`) {
		t.Fatalf("disabled hardware decode was not saved as a boolean: %s", data)
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
	if len(cfg.Upstream.URLs) != 1 || cfg.Upstream.URLs[0] != "http://emby.local:8096" {
		t.Fatalf("legacy upstream url was not migrated: %+v", cfg.Upstream)
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
	if cfg.Transcode.HardwareDevice != "/dev/dri/renderD128" {
		t.Fatalf("hardware device = %q", cfg.Transcode.HardwareDevice)
	}
}

func TestSaveWritesVAAPIHardwareDecodeAsBooleanTrue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Transcode.HardwareDecode = "vaapi"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"hardware_acceleration": true`) ||
		strings.Contains(string(data), `"hardware_decode"`) {
		t.Fatalf("VAAPI hardware decode was not saved as boolean true: %s", data)
	}
}

func TestLoadSupportsBooleanTrueHardwareDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	err := os.WriteFile(path, []byte(`{
		"upstream": {"urls": ["http://upstream.local"]},
		"transcode": {"hardware_acceleration": true}
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
