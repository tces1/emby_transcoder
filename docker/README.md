# Docker

## Files

- `Dockerfile`: multi-stage image build for EmbyTranscoder plus runtime `ffmpeg`
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
- set `transcode.hardware_decode` to `vaapi` to use Intel or AMD VAAPI hardware decode through `/dev/dri`
- startup probes VAAPI support, including device initialization, and fails startup when the device, driver, or ffmpeg support is unavailable
- the image includes common Intel and AMD VAAPI userspace drivers plus `vainfo`
- video output is capped at 1920x1080 while preserving aspect ratio

For GitHub Actions publishing to Docker Hub, set `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` in the repository secrets.

The transcode cache is stored in `docker/data/transcode`.

## Operations

```bash
docker compose logs -f
docker compose restart
docker compose down
```
