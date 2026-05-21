# Docker

## Files

- `Dockerfile`: multi-stage image build for EmbyTranscoder plus runtime `ffmpeg`
- `docker-compose.yml`: local container run setup
- `config/config.json`: container config template

## Run

From the `docker/` directory:

```bash
mkdir -p data/transcode
docker compose up --build -d
```

## Configure

Edit `config/config.json` before startup:

- set `upstream.url` to your Emby or Jellyfin server
- set `server.public_url` if clients reach the proxy through another reverse proxy
- set `server.debug` to `true` when you need detailed diagnostics

For GitHub Actions publishing to Docker Hub, set `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` in the repository secrets.

The transcode cache is stored in `docker/data/transcode`.
