# EmbyTranscoder

[中文](README.md)

[![Docker Image](https://github.com/tces1/emby_transcoder/actions/workflows/docker-image.yml/badge.svg)](https://github.com/tces1/emby_transcoder/actions/workflows/docker-image.yml)
[![Docker Pulls](https://img.shields.io/docker/pulls/tces1/emby_transcoder?logo=docker&label=Docker%20Pulls)](https://hub.docker.com/r/tces1/emby_transcoder)
[![Image](https://img.shields.io/badge/image-tces1%2Femby__transcoder-blue?logo=docker)](https://hub.docker.com/r/tces1/emby_transcoder)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-lightgrey)](docker/Dockerfile)
[![HLS](https://img.shields.io/badge/streaming-HLS%20%2B%20MPEG--TS-orange)](#transcode-lifecycle)
[![VAAPI](https://img.shields.io/badge/hardware-VAAPI-green)](#configuration)
[![License](https://img.shields.io/badge/license-not%20specified-lightgrey)](#license)

EmbyTranscoder is a lightweight Go reverse proxy that adds local FFmpeg HLS transcoding fallback for Emby and Jellyfin clients.

It is intentionally narrow: normal API traffic is forwarded to the upstream server, while selected clients can receive a proxy-provided HLS `TranscodingUrl` when they request `PlaybackInfo`.

## Current Scope

- Native Linux-friendly Go binary.
- Transparent reverse proxy for ordinary Emby/Jellyfin requests.
- Client profile matching by `User-Agent` and `X-Emby-Authorization`.
- PlaybackInfo rewriting for matched profiles.
- Local FFmpeg HLS sessions under `/streambridge/transcode/`.
- Audio track selection through Emby `AudioStreamIndex`, with local transcode restart on audio changes.
- Playback lifecycle tracking through Emby `/Sessions/Playing*` check-ins plus HLS access.
- Conservative output target: H.264 video, AAC audio, HLS MPEG-TS segments.
- Video output is capped at 1920x1080 and keeps aspect ratio.
- PlaybackInfo rewrite prewarms the transcode session before the first playlist request.
- FFmpeg uses low-latency startup and GOP settings to cut first-segment delay.

Not included: virtual libraries, RSS, cover generation, scraping, database storage, or a management UI.

## Run

```bash
go run ./cmd/emby-transcoder -config config.example.json
```

Point a client at the proxy listen address, for example `http://linux-host:8097`.

## Docker Compose

Use the public Docker Hub image `tces1/emby_transcoder:latest`. The compose file in `docker/` already points at this published linux/amd64 image and does not build locally.

Minimal compose service:

```yaml
services:
  emby-transcoder:
    image: tces1/emby_transcoder:latest
    container_name: emby-transcoder
    restart: unless-stopped
    privileged: true
    ports:
      - "8097:8097"
    devices:
      - /dev/dri:/dev/dri
    volumes:
      - ./config/config.json:/app/config/config.json:ro
      - ./data/transcode:/var/lib/emby-transcoder/transcode
```

```bash
cd docker
mkdir -p data/transcode
cp config/config.json config/config.local.json
```

Edit `docker/config/config.local.json` before starting:

- set `upstream.url` to your Emby or Jellyfin server
- set `server.public_url` if clients reach the proxy through another reverse proxy
- leave `server.debug` as `false` for concise logs, or set it to `true` for detailed diagnostics
- set `transcode.hardware_decode` to `vaapi` on Linux hosts with Intel or AMD `/dev/dri` VAAPI support
- VAAPI mode first tries `vaapi-full` (`scale_vaapi` GPU scaling plus `h264_vaapi` encoding), then falls back to `vaapi-encode` (CPU scaling plus `h264_vaapi`) when GPU scaling is unsupported
- startup will probe VAAPI availability, including device initialization and `h264_vaapi`, and fail startup if the device, driver, or ffmpeg support is missing

Update `docker/docker-compose.yml` to mount the local config file if you use `config.local.json`:

```yaml
volumes:
  - ./config/config.local.json:/app/config/config.json:ro
  - ./data/transcode:/var/lib/emby-transcoder/transcode
```

Start or update the service:

```bash
docker compose pull
docker compose up -d
docker compose logs -f
```

Stop the service:

```bash
docker compose down
```

## Build

```bash
go build ./cmd/emby-transcoder
```

## Configuration

Copy `config.example.json` and change the upstream URL:

```json
{
  "server": {
    "listen": ":8097",
    "public_url": "",
    "debug": false
  },
  "upstream": {
    "url": "http://127.0.0.1:8096"
  },
  "transcode": {
    "enabled": true,
    "ffmpeg_path": "/usr/bin/ffmpeg",
    "temp_dir": "/var/lib/emby-transcoder/transcode",
    "hardware_decode": "",
    "hardware_device": "/dev/dri/renderD128",
    "max_sessions": 2,
    "buffer_pause_seconds": 300,
    "buffer_resume_seconds": 120,
    "segment_seconds": 2,
    "segment_retention_seconds": 300,
    "idle_timeout_seconds": 60
  }
}
```

Leave `public_url` empty when clients connect directly to EmbyTranscoder. Set it when EmbyTranscoder sits behind another reverse proxy.
Leave `debug` as `false` for concise action-level logs. Set it to `true` when you want detailed `TRACE_SWITCH` and request-level diagnostics.
Set `hardware_decode` to `vaapi` to enable VAAPI hardware transcoding. The default `hardware_device` is `/dev/dri/renderD128`.
Startup prefers `vaapi-full`; if `scale_vaapi` fails, it falls back to `vaapi-encode` so H.264 encoding still stays on the GPU. If the device, driver, or `h264_vaapi` probe fails, startup stops with an error.

## Transcode Lifecycle

EmbyTranscoder keeps local FFmpeg sessions tied to Emby playback check-ins:

- `POST /Sessions/Playing` and `/Sessions/Playing/Progress` update local playback state.
- `POST /Sessions/Playing/Stopped` immediately stops the matching local FFmpeg session.
- HLS playlist and segment requests refresh media activity.
- `segment_seconds` controls HLS segment duration; default `2` balances startup latency with segment count, while `1` is fastest and higher values reduce disk churn.
- When transcoded media gets more than `buffer_pause_seconds` ahead of playback, FFmpeg is paused.
- When buffered media falls back under `buffer_resume_seconds`, FFmpeg resumes.
- Segments older than `segment_retention_seconds` behind the current playback position are deleted from the local cache.
- If neither playback activity nor HLS access arrives before `idle_timeout_seconds`, the idle reaper stops the session.
- A new `master.m3u8` request with a different upstream stream URL, such as a seek with a different `StartTimeTicks`, restarts the local session.

## License

This repository does not declare an open-source license yet. After choosing MIT, Apache-2.0, or another license, add a `LICENSE` file and update the badge above.
