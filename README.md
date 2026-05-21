# EmbyTranscoder

EmbyTranscoder is a lightweight Go reverse proxy that adds local FFmpeg HLS transcoding fallback for Emby and Jellyfin clients.

It is intentionally narrow: normal API traffic is forwarded to the upstream server, while selected clients can receive a proxy-provided HLS `TranscodingUrl` when they request `PlaybackInfo`.

## Current Scope

- Native Linux-friendly Go binary.
- Transparent reverse proxy for ordinary Emby/Jellyfin requests.
- Client profile matching by `User-Agent` and `X-Emby-Authorization`.
- PlaybackInfo rewriting for matched profiles.
- Local FFmpeg HLS sessions under `/streambridge/transcode/`.
- Playback lifecycle tracking through Emby `/Sessions/Playing*` check-ins plus HLS access.
- Conservative output target: H.264 video, AAC audio, HLS MPEG-TS segments.

Not included: virtual libraries, RSS, cover generation, scraping, database storage, or a management UI.

## Run

```bash
go run ./cmd/emby-transcoder -config config.example.json
```

Point a client at the proxy listen address, for example `http://linux-host:8097`.

## Docker Compose

The published linux/amd64 image is available as `tces1/emby_transcoder:latest`.

```bash
cd docker
mkdir -p data/transcode
cp config/config.json config/config.local.json
```

Edit `docker/config/config.local.json` before starting:

- set `upstream.url` to your Emby or Jellyfin server
- set `server.public_url` if clients reach the proxy through another reverse proxy
- leave `server.debug` as `false` for concise logs, or set it to `true` for detailed diagnostics
- set `transcode.hardware_decode` to `vaapi` on Linux hosts with Intel or AMD `/dev/dri` hardware decode support
- startup will probe VAAPI availability and fall back to software decode if the device or ffmpeg support is missing

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
    "idle_timeout_seconds": 60
  }
}
```

Leave `public_url` empty when clients connect directly to EmbyTranscoder. Set it when EmbyTranscoder sits behind another reverse proxy.
Leave `debug` as `false` for concise action-level logs. Set it to `true` when you want detailed `TRACE_SWITCH` and request-level diagnostics.
Set `hardware_decode` to `vaapi` to enable VAAPI hardware decode. The default `hardware_device` is `/dev/dri/renderD128`.
If the device or ffmpeg probe fails, startup falls back to software decode and logs the reason.

## Transcode Lifecycle

EmbyTranscoder keeps local FFmpeg sessions tied to Emby playback check-ins:

- `POST /Sessions/Playing` and `/Sessions/Playing/Progress` update local playback state.
- `POST /Sessions/Playing/Stopped` immediately stops the matching local FFmpeg session.
- HLS playlist and segment requests refresh media activity.
- When transcoded media gets more than `buffer_pause_seconds` ahead of playback, FFmpeg is paused.
- When buffered media falls back under `buffer_resume_seconds`, FFmpeg resumes.
- If neither playback activity nor HLS access arrives before `idle_timeout_seconds`, the idle reaper stops the session.
- A new `master.m3u8` request with a different upstream stream URL, such as a seek with a different `StartTimeTicks`, restarts the local session.
