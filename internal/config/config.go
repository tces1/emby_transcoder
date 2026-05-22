package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

type Config struct {
	Server    Server          `json:"server"`
	Upstream  Upstream        `json:"upstream"`
	Transcode Transcode       `json:"transcode"`
	Clients   []ClientProfile `json:"clients"`
}

type Server struct {
	Listen    string `json:"listen"`
	PublicURL string `json:"public_url"`
	Debug     bool   `json:"debug"`
}

type Upstream struct {
	URL string `json:"url"`
}

type Transcode struct {
	Enabled                 bool          `json:"enabled"`
	FFmpegPath              string        `json:"ffmpeg_path"`
	TempDir                 string        `json:"temp_dir"`
	HardwareDecode          string        `json:"hardware_decode"`
	HardwareDevice          string        `json:"hardware_device"`
	MaxSessions             int           `json:"max_sessions"`
	BufferPauseSeconds      int           `json:"buffer_pause_seconds"`
	BufferResumeSeconds     int           `json:"buffer_resume_seconds"`
	SegmentRetentionSeconds int           `json:"segment_retention_seconds"`
	IdleTimeoutSeconds      int           `json:"idle_timeout_seconds"`
	BufferPause             time.Duration `json:"-"`
	BufferResume            time.Duration `json:"-"`
	SegmentRetention        time.Duration `json:"-"`
	IdleTimeout             time.Duration `json:"-"`
}

func (t *Transcode) UnmarshalJSON(data []byte) error {
	type transcodeAlias Transcode
	aux := struct {
		HardwareDecode json.RawMessage `json:"hardware_decode"`
		*transcodeAlias
	}{
		transcodeAlias: (*transcodeAlias)(t),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.HardwareDecode) == 0 || string(aux.HardwareDecode) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(aux.HardwareDecode, &text); err == nil {
		t.HardwareDecode = text
		return nil
	}
	var enabled bool
	if err := json.Unmarshal(aux.HardwareDecode, &enabled); err == nil {
		if !enabled {
			t.HardwareDecode = "false"
			return nil
		}
		return errors.New(`transcode.hardware_decode boolean true is not supported; use "vaapi"`)
	}
	return errors.New(`transcode.hardware_decode must be a string or false`)
}

type ClientProfile struct {
	Name      string   `json:"name"`
	Match     []string `json:"match"`
	Transcode bool     `json:"transcode"`
}

func Default() Config {
	cfg := Config{
		Server: Server{
			Listen: ":8097",
		},
		Upstream: Upstream{
			URL: "http://127.0.0.1:8096",
		},
		Transcode: Transcode{
			Enabled:                 true,
			FFmpegPath:              "/usr/bin/ffmpeg",
			TempDir:                 "/var/lib/emby-transcoder/transcode",
			MaxSessions:             2,
			BufferPauseSeconds:      300,
			BufferResumeSeconds:     120,
			SegmentRetentionSeconds: 300,
			IdleTimeoutSeconds:      60,
		},
		Clients: []ClientProfile{
			{Name: "emby_android_tv", Match: []string{"Emby for Android TV", "Android TV"}, Transcode: true},
			{Name: "yamby_tv", Match: []string{"Yamby"}, Transcode: true},
			{Name: "senplayer_macos", Match: []string{"SenPlayer"}, Transcode: false},
		},
	}
	normalize(&cfg)
	return cfg
}

func Load(path string) (Config, error) {
	cfg := Default()
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	normalize(&cfg)
	if cfg.Upstream.URL == "" {
		return Config{}, errors.New("upstream.url is required")
	}
	return cfg, nil
}

func normalize(cfg *Config) {
	cfg.Server.PublicURL = strings.TrimRight(cfg.Server.PublicURL, "/")
	cfg.Upstream.URL = strings.TrimRight(cfg.Upstream.URL, "/")
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8097"
	}
	if cfg.Transcode.FFmpegPath == "" {
		cfg.Transcode.FFmpegPath = "/usr/bin/ffmpeg"
	}
	if cfg.Transcode.TempDir == "" {
		cfg.Transcode.TempDir = "/var/lib/emby-transcoder/transcode"
	}
	cfg.Transcode.HardwareDecode = strings.ToLower(strings.TrimSpace(cfg.Transcode.HardwareDecode))
	cfg.Transcode.HardwareDevice = strings.TrimSpace(cfg.Transcode.HardwareDevice)
	if cfg.Transcode.HardwareDecode == "vaapi" && cfg.Transcode.HardwareDevice == "" {
		cfg.Transcode.HardwareDevice = "/dev/dri/renderD128"
	}
	if cfg.Transcode.MaxSessions <= 0 {
		cfg.Transcode.MaxSessions = 2
	}
	if cfg.Transcode.BufferPauseSeconds <= 0 {
		cfg.Transcode.BufferPauseSeconds = 300
	}
	if cfg.Transcode.BufferResumeSeconds <= 0 {
		cfg.Transcode.BufferResumeSeconds = 120
	}
	if cfg.Transcode.SegmentRetentionSeconds <= 0 {
		cfg.Transcode.SegmentRetentionSeconds = 300
	}
	if cfg.Transcode.IdleTimeoutSeconds <= 0 {
		cfg.Transcode.IdleTimeoutSeconds = 60
	}
	cfg.Transcode.BufferPause = time.Duration(cfg.Transcode.BufferPauseSeconds) * time.Second
	cfg.Transcode.BufferResume = time.Duration(cfg.Transcode.BufferResumeSeconds) * time.Second
	cfg.Transcode.SegmentRetention = time.Duration(cfg.Transcode.SegmentRetentionSeconds) * time.Second
	cfg.Transcode.IdleTimeout = time.Duration(cfg.Transcode.IdleTimeoutSeconds) * time.Second
}
