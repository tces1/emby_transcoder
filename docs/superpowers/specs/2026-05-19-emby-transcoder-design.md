# EmbyTranscoder Design

## Purpose

EmbyTranscoder is a lightweight Go reverse proxy for Emby and Jellyfin playback compatibility. It forwards normal API traffic transparently and adds local FFmpeg HLS transcoding only when a configured client needs a fallback because the upstream server does not provide transcoding.

## Scope

The project focuses on playback proxying and local online transcoding. It does not implement virtual libraries, RSS libraries, cover generation, media scraping, recommendations, or a management UI in the first version.

## Architecture

The service runs as a native Linux binary. Clients point their Emby/Jellyfin server address at EmbyTranscoder. EmbyTranscoder forwards ordinary requests to the upstream server. When a request targets `PlaybackInfo`, EmbyTranscoder can rewrite the upstream JSON response to advertise a local HLS `TranscodingUrl` served by the proxy.

```text
Emby Android TV / Yamby TV / SenPlayer
        |
        v
  EmbyTranscoder
  - transparent proxy
  - PlaybackInfo rewrite
  - client policy matching
  - local FFmpeg HLS sessions
        |
        v
  Emby / Jellyfin upstream
```

## First Version Behavior

- Listen on a configurable HTTP address.
- Forward non-transcode traffic to the configured upstream server.
- Detect `PlaybackInfo` requests.
- Match clients using `User-Agent` and `X-Emby-Authorization`.
- Rewrite `MediaSources` only when a client profile enables proxy transcoding.
- Serve local HLS playlists and `.ts` segments under `/streambridge/transcode/{session}/...`.
- Start FFmpeg on demand for a transcode session.
- Output H.264 video, AAC audio, and MPEG-TS HLS segments.
- Limit concurrent sessions.
- Stop idle sessions and remove temporary segment directories.
- Fall back to the upstream response when rewriting cannot be done safely.

## Configuration

The first version uses JSON to avoid external runtime dependencies.

```json
{
  "server": {
    "listen": ":8097",
    "public_url": ""
  },
  "upstream": {
    "url": "http://127.0.0.1:8096"
  },
  "transcode": {
    "enabled": true,
    "ffmpeg_path": "/usr/bin/ffmpeg",
    "temp_dir": "/var/lib/emby-transcoder/transcode",
    "max_sessions": 2,
    "idle_timeout_seconds": 60
  },
  "clients": [
    {
      "name": "emby_android_tv",
      "match": ["Emby for Android TV", "Android TV"],
      "transcode": true
    },
    {
      "name": "yamby_tv",
      "match": ["Yamby"],
      "transcode": true
    },
    {
      "name": "senplayer_macos",
      "match": ["SenPlayer"],
      "transcode": false
    }
  ]
}
```

## PlaybackInfo Rewrite

When policy allows proxy transcoding, EmbyTranscoder rewrites each media source with a local URL:

```json
{
  "SupportsDirectPlay": false,
  "SupportsDirectStream": false,
  "SupportsTranscoding": true,
  "TranscodingUrl": "/streambridge/transcode/<session>/master.m3u8"
}
```

The session stores the upstream item id, request headers needed to fetch the original stream, and an upstream media URL. The first version can use an upstream stream URL derived from the item id:

```text
/emby/Videos/{itemId}/stream
```

If the upstream PlaybackInfo contains a better direct stream path, later versions can prefer that value.

## FFmpeg Strategy

The first implementation uses conservative CPU encoding:

```text
ffmpeg -hide_banner -loglevel warning
  -headers <forwarded auth headers>
  -i <upstream stream URL>
  -map 0:v:0 -map 0:a:0?
  -c:v libx264 -preset veryfast -profile:v high -level 4.1
  -pix_fmt yuv420p
  -c:a aac -b:a 160k -ac 2
  -f hls -hls_time 4 -hls_list_size 6
  -hls_flags delete_segments+append_list+independent_segments
  -hls_segment_filename <session-dir>/segment_%05d.ts
  <session-dir>/master.m3u8
```

Hardware acceleration, subtitle burn-in, HDR tonemapping, and media-specific rules are explicitly later work.

## Non-Goals

- No virtual libraries.
- No RSS integration.
- No cover generation.
- No database.
- No Docker-only assumptions.
- No complete Emby server replacement.
- No copied implementation from third-party projects.

