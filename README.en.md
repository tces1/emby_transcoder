# Emby-Transcoder

[中文](README.md)

[![Docker Image](https://github.com/tces1/emby_transcoder/actions/workflows/docker-image.yml/badge.svg)](https://github.com/tces1/emby_transcoder/actions/workflows/docker-image.yml)
[![Docker Pulls](https://img.shields.io/docker/pulls/tces1/emby_transcoder?logo=docker&label=Docker%20Pulls)](https://hub.docker.com/r/tces1/emby_transcoder)
[![Image](https://img.shields.io/badge/image-tces1%2Femby__transcoder-blue?logo=docker)](https://hub.docker.com/r/tces1/emby_transcoder)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-lightgrey)](docker/Dockerfile)
[![HLS](https://img.shields.io/badge/streaming-HLS%20%2B%20MPEG--TS-orange)](#transcode-lifecycle)
[![VAAPI](https://img.shields.io/badge/hardware-VAAPI-green)](#configuration)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Emby-Transcoder is a lightweight Go reverse proxy that adds local FFmpeg HLS transcoding fallback for Emby and Jellyfin clients.

It is intentionally narrow: normal API traffic is forwarded to the upstream server, while selected clients can receive a proxy-provided HLS `TranscodingUrl` when they request `PlaybackInfo`.

## How It Works

```text
Emby / Jellyfin client
        |
        v
  Emby-Transcoder
  - transparent proxy for ordinary requests
  - PlaybackInfo matching by client profile
  - PlaybackInfo rewrite to local HLS
  - local FFmpeg transcode sessions
        |
        v
  upstream Emby / Jellyfin
```

## Current Scope

- Native Linux-friendly Go binary.
- Transparent reverse proxy for ordinary Emby/Jellyfin requests.
- Client profile matching by `User-Agent`, `X-Emby-Authorization`, and `X-MediaBrowser-Token`.
- PlaybackInfo rewriting for matched profiles.
- Local FFmpeg HLS sessions under `/streambridge/transcode/`.
- Audio track selection through Emby `AudioStreamIndex`, with local transcode restart on audio changes.
- Playback lifecycle tracking through Emby `/Sessions/Playing*` check-ins plus HLS access.
- Conservative output target: H.264 video, AAC audio, HLS MPEG-TS segments.
- Software transcoding caps video output at 1920x1080 and keeps aspect ratio; VAAPI mode does not scale.
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

- set `upstream.urls` to your Emby or Jellyfin entrance list; the first route is primary and later routes are failover targets
- set `server.public_url` if clients reach the proxy through another reverse proxy
- leave `server.debug` as `false` for concise logs, or set it to `true` for detailed diagnostics
- leave `transcode.hardware_decode` as `""` to disable hardware acceleration and use CPU transcoding
- set `transcode.hardware_decode` to `vaapi` on Linux hosts with Intel or AMD `/dev/dri` VAAPI support
- the current VAAPI path uses hardware decode plus `h264_vaapi` hardware encoding and does not add a scale filter
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
    "urls": [
      "http://127.0.0.1:8096"
    ]
  },
  "transcode": {
    "enabled": true,
    "ffmpeg_path": "/usr/bin/ffmpeg",
    "temp_dir": "/var/lib/emby-transcoder/transcode",
    "hardware_decode": "",
    "hardware_device": "/dev/dri/renderD128",
    "max_sessions": 2,
    "download_workers": 1,
    "download_chunk_mb": 8,
    "download_buffer_mb": 64,
    "buffer_pause_seconds": 300,
    "buffer_resume_seconds": 120,
    "segment_seconds": 2,
    "segment_retention_seconds": 300,
    "idle_timeout_seconds": 60
  }
}
```

Leave `public_url` empty when clients connect directly to Emby-Transcoder. Set it when Emby-Transcoder sits behind another reverse proxy.
Leave `debug` as `false` for concise action-level logs. Set it to `true` when you want detailed `TRACE_SWITCH` and request-level diagnostics.
Leave `hardware_decode` as `""` to disable hardware acceleration and use CPU transcoding. Set it to `vaapi` to enable VAAPI hardware transcoding. The default `hardware_device` is `/dev/dri/renderD128`.
The current VAAPI path uses hardware decode plus `h264_vaapi` hardware encoding and does not add a scale filter. If the device, driver, or `h264_vaapi` probe fails, startup stops with an error.

`download_workers` controls global concurrent HTTP Range downloads for FFmpeg input. The default `1` disables acceleration and lets FFmpeg access the upstream directly; set it to `2` to enable dual-stream downloading. To avoid having extra connections counted as additional playback streams, the process enforces a hard global limit of `2` upstream Range requests even when a larger value is configured. `download_chunk_mb` sets each range size and `download_buffer_mb` bounds the global read-ahead window; `2 / 8 / 64` is the recommended starting point. Streams without byte-range support or a stable ETag/Last-Modified validator automatically fall back to normal forwarding so chunks from different resource versions cannot be mixed.

When the same upstream has multiple entrances, configure them directly in `upstream.urls`. The first is the primary API route. For safely retryable GET, HEAD, and OPTIONS requests, a connection error or 502/503/504 response switches the service to a working backup route. Non-idempotent requests such as POST are not replayed, preventing duplicate operations. The legacy single-value `upstream.url` remains supported.

Array order is also media-route priority. With one transcode session, the first two healthy routes download concurrently while later routes remain on standby. With two sessions, the earlier session is pinned to the first healthy route and the second session to the next one. When either session ends, the remaining session automatically returns to dual-route mode. Repeated route failures advance to the next configured route, while global upstream concurrency always remains capped at `2`.

With dual downloading enabled, the project preserves the real `DirectStreamUrl` returned by PlaybackInfo, including its `/original.mkv` path and signed query. Each worker requests that path through a different entrance and follows its 302 redirect to the actual media host; final media hostnames are not guessed or replaced:

```json
"upstream": {
  "urls": [
    "https://entry-a.example.com",
    "https://entry-b.example.com"
  ]
},
"transcode": {
  "download_workers": 2
}
```

Both entrances are probed through their final media responses before downloading. Only routes with matching file sizes and ETag/Last-Modified validators participate. An unavailable or inconsistent route is excluded, and downloading automatically becomes single-route when only one valid entrance remains.

## Transcode Lifecycle

Emby-Transcoder keeps local FFmpeg sessions tied to Emby playback check-ins:

- `POST /Sessions/Playing` and `/Sessions/Playing/Progress` update local playback state.
- `POST /Sessions/Playing/Stopped` immediately stops the matching local FFmpeg session.
- HLS playlist and segment requests refresh media activity.
- `segment_seconds` controls HLS segment duration; default `2` balances startup latency with segment count, while `1` is fastest and higher values reduce disk churn.
- When transcoded media gets more than `buffer_pause_seconds` ahead of playback, FFmpeg is paused.
- When buffered media falls back under `buffer_resume_seconds`, FFmpeg resumes.
- Segments older than `segment_retention_seconds` behind the current playback position are deleted from the local cache.
- If neither playback activity nor HLS access arrives before `idle_timeout_seconds`, the idle reaper stops the session.
- A new `master.m3u8` request with a different upstream stream URL, such as a seek with a different `StartTimeTicks`, restarts the local session.

## Status Dashboard

Open `/emby_transcoder` on the same proxy origin to access the status dashboard. An Emby API Key or Token is validated through `/emby/Users/Me`. After login, only an opaque dashboard session ID is stored in an HttpOnly cookie; the Emby token is not embedded in the page or status API.

The dashboard refreshes every second and shows:

- Idle, probing, downloading, forwarding, and error states for both download workers.
- Entrance/final media hosts, byte ranges, live download rates, and cumulative bytes.
- Video names, FFmpeg running/paused/exited state, and FFmpeg `speed` ratio.
- Live and cumulative HLS upload traffic sent to clients.
- A “route → download → FFmpeg → HLS upload” state-machine view.

## License

This repository's code is licensed under the [Apache License 2.0](LICENSE), Copyright 2026 tces1.

Debian, FFmpeg, and VAAPI userspace components included in the Docker image remain under their respective upstream licenses.
