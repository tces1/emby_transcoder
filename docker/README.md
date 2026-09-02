# Docker

## Files

- `Dockerfile`: multi-stage image build for Emby-Transcoder plus runtime `ffmpeg`
- `docker-compose.yml`: container run setup using `tces1/emby_transcoder:latest`
- `config/config.json`: container config template

## Run

From the `docker/` directory:

```bash
mkdir -p data/transcode
docker compose pull
docker compose up -d
```

## Configure

Edit `config/config.json` before startup, or copy it to `config/config.local.json` and update the compose volume:

- set `upstream.urls` to your Emby or Jellyfin entrance list; the first route is primary and later routes are API failover targets
- set `server.public_url` if clients reach the proxy through another reverse proxy
- set `server.dashboard_password` to enable password login for `/emby_transcoder`
- set `server.debug` to `true` when you need detailed diagnostics
- set `transcode.hardware_acceleration` to `false` to disable hardware acceleration and use CPU transcoding
- set `transcode.hardware_acceleration` to `true` to use Intel or AMD VAAPI through `/dev/dri`; an empty device path defaults to `/dev/dri/renderD128`
- VAAPI normally uses hardware decode plus `h264_vaapi`; 4K HEVC Main 8 uses software decode/scale with VAAPI encode for compatibility
- startup probes VAAPI support, including device initialization and `h264_vaapi`, and fails startup when the device, driver, or ffmpeg support is unavailable
- the image includes common Intel and AMD VAAPI userspace drivers plus `vainfo`
- software and VAAPI compatibility pipelines cap output at 1920x1080 while preserving aspect ratio
- `transcode.segment_seconds` controls HLS segment duration; default `2` balances startup latency with segment count
- set `transcode.download_workers` to `2` to assemble dual HTTP Range downloads in a sparse file with `8` MB chunks and `64` MB read-ahead; the process hard-limits upstream concurrency to `2`
- with two `upstream.urls`, media workers preserve the PlaybackInfo `DirectStreamUrl`, alternate requests through both entrances, and follow each entrance's redirect to its actual media host
- `upstream.urls` order is route priority: one session uses the first two healthy routes, while two sessions are pinned one route each; later entries are failover routes
- media download starts as soon as the first usable route confirms Range support; a second distinct final host is probed in the background
- missing ETag/Last-Modified falls back to file-size plus head/middle/tail SHA-256 validation only when adding that second route
- transcoding starts only on an HLS playlist or segment request; PlaybackInfo browsing does not prewarm or download media
- ffmpeg runs with low-latency startup and GOP settings to reduce first-segment delay
- old `segment_*.ts` files are deleted once they are more than `transcode.segment_retention_seconds` behind playback
- open `/emby_transcoder` on the proxy and authenticate with `server.dashboard_password` to view worker, FFmpeg, download, and HLS upload status
- the authenticated dashboard includes Chinese/English switching, a validated JSON configuration editor, and a service restart action; keep the config bind mount writable

For GitHub Actions publishing to Docker Hub, set `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` in the repository secrets.

The transcode cache is stored in `docker/data/transcode`; active sessions keep only the retained back-buffer plus the forward buffer.

## Operations

```bash
docker compose logs -f
docker compose restart
docker compose down
```
