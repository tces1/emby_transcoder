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

- set `upstream.url` to your Emby or Jellyfin server
- set `server.public_url` if clients reach the proxy through another reverse proxy
- set `server.debug` to `true` when you need detailed diagnostics
- leave `transcode.hardware_decode` as `""` to disable hardware acceleration and use CPU transcoding
- set `transcode.hardware_decode` to `vaapi` to use Intel or AMD VAAPI through `/dev/dri`
- VAAPI mode uses hardware decode plus `h264_vaapi` hardware encoding and does not add a scale filter
- startup probes VAAPI support, including device initialization and `h264_vaapi`, and fails startup when the device, driver, or ffmpeg support is unavailable
- the image includes common Intel and AMD VAAPI userspace drivers plus `vainfo`
- software transcoding caps video output at 1920x1080 while preserving aspect ratio; VAAPI mode does not scale
- `transcode.segment_seconds` controls HLS segment duration; default `2` balances startup latency with segment count
- playbackinfo rewrites prewarm the transcode session before the first playlist request
- ffmpeg runs with low-latency startup and GOP settings to reduce first-segment delay
- old `segment_*.ts` files are deleted once they are more than `transcode.segment_retention_seconds` behind playback

For GitHub Actions publishing to Docker Hub, set `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` in the repository secrets.

The transcode cache is stored in `docker/data/transcode`; active sessions keep only the retained back-buffer plus the forward buffer.

## Operations

```bash
docker compose logs -f
docker compose restart
docker compose down
```
