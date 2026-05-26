# Emby-Transcoder

[English](README.en.md)

[![Docker Image](https://github.com/tces1/emby_transcoder/actions/workflows/docker-image.yml/badge.svg)](https://github.com/tces1/emby_transcoder/actions/workflows/docker-image.yml)
[![Docker Pulls](https://img.shields.io/docker/pulls/tces1/emby_transcoder?logo=docker&label=Docker%20Pulls)](https://hub.docker.com/r/tces1/emby_transcoder)
[![Image](https://img.shields.io/badge/image-tces1%2Femby__transcoder-blue?logo=docker)](https://hub.docker.com/r/tces1/emby_transcoder)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/platform-linux%2Famd64-lightgrey)](docker/Dockerfile)
[![HLS](https://img.shields.io/badge/streaming-HLS%20%2B%20MPEG--TS-orange)](#转码生命周期)
[![VAAPI](https://img.shields.io/badge/hardware-VAAPI-green)](#配置)
[![License](https://img.shields.io/badge/license-not%20specified-lightgrey)](#协议)

Emby-Transcoder 是一个轻量级 Go 反向代理，为 Emby 和 Jellyfin 客户端补充本地 FFmpeg HLS 转码能力。

它的目标很窄：普通 API 请求继续透明转发到上游服务；命中配置规则的客户端请求 `PlaybackInfo` 时，会收到由代理提供的 HLS `TranscodingUrl`。

## 当前功能

- 原生 Go 二进制，适合 Linux 部署。
- 普通 Emby/Jellyfin 请求透明反向代理。
- 按 `User-Agent` 和 `X-Emby-Authorization` 匹配客户端配置。
- 对命中的客户端重写 PlaybackInfo。
- 本地 FFmpeg HLS 会话路径为 `/streambridge/transcode/`。
- 支持通过 Emby `AudioStreamIndex` 选择音轨，切换音轨时会重启本地转码。
- 通过 Emby `/Sessions/Playing*` check-in 和 HLS 访问跟踪播放生命周期。
- 输出目标保守固定为 H.264 视频、AAC 音频、HLS MPEG-TS 分片。
- 视频输出限制到 1920x1080，并保持原始宽高比。
- PlaybackInfo 重写时会预热转码会话，减少首次 playlist 请求等待。
- FFmpeg 使用低延迟启动和 GOP 参数，降低首分片延迟。

不包含：虚拟媒体库、RSS、封面生成、刮削、数据库存储或管理 UI。

## 直接运行

```bash
go run ./cmd/emby-transcoder -config config.example.json
```

然后让客户端连接代理地址，例如 `http://linux-host:8097`。

## Docker Compose

使用 Docker Hub 公共镜像 `tces1/emby_transcoder:latest`。`docker/` 目录里的 compose 文件已经指向这个发布好的 linux/amd64 镜像，不会在本地 build。

最小 compose 服务示例：

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

使用仓库内的 compose 模板：

```bash
cd docker
mkdir -p data/transcode
cp config/config.json config/config.local.json
```

启动前修改 `docker/config/config.local.json`：

- 将 `upstream.url` 改成你的 Emby 或 Jellyfin 地址。
- 如果客户端通过另一层反向代理访问本服务，设置 `server.public_url`。
- `server.debug` 默认保持 `false`，日志更简洁；需要诊断时改成 `true`。
- Linux 主机有 Intel 或 AMD `/dev/dri` VAAPI 支持时，可将 `transcode.hardware_decode` 设置为 `vaapi`。
- VAAPI 会先尝试 `vaapi-full`（`scale_vaapi` GPU 缩放加 `h264_vaapi` 编码），不支持 GPU 缩放时回退到 `vaapi-encode`（CPU 缩放加 `h264_vaapi` 编码）。
- 启动时会探测 VAAPI 可用性，包括设备初始化和 `h264_vaapi`；设备、驱动或 ffmpeg 支持缺失时会启动失败。

如果使用 `config.local.json`，需要把 `docker/docker-compose.yml` 的挂载改成本地配置文件：

```yaml
volumes:
  - ./config/config.local.json:/app/config/config.json:ro
  - ./data/transcode:/var/lib/emby-transcoder/transcode
```

启动或更新服务：

```bash
docker compose pull
docker compose up -d
docker compose logs -f
```

停止服务：

```bash
docker compose down
```

## 编译

```bash
go build ./cmd/emby-transcoder
```

## 配置

复制 `config.example.json`，然后修改上游地址：

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

客户端直接连接 Emby-Transcoder 时，`public_url` 留空。服务在另一层反向代理后面时，再设置它。

`debug` 保持 `false` 时只输出动作级日志；需要 `TRACE_SWITCH` 和请求级诊断时设置为 `true`。

设置 `hardware_decode` 为 `vaapi` 可启用 VAAPI 硬件转码，默认设备是 `/dev/dri/renderD128`。

启动时优先使用 `vaapi-full`；如果 `scale_vaapi` 失败，会回退到 `vaapi-encode`，仍然使用 GPU 做 H.264 编码。如果设备、驱动或 `h264_vaapi` 探测失败，服务会停止启动。

## 转码生命周期

Emby-Transcoder 会把本地 FFmpeg 会话绑定到 Emby 播放 check-in：

- `POST /Sessions/Playing` 和 `/Sessions/Playing/Progress` 更新本地播放状态。
- `POST /Sessions/Playing/Stopped` 会立即停止匹配的本地 FFmpeg 会话。
- HLS playlist 和 segment 请求会刷新媒体访问时间。
- `segment_seconds` 控制 HLS 分片时长；默认 `2` 秒，兼顾启动延迟和分片数量。设为 `1` 启动最快，更高值会减少文件数量和磁盘抖动。
- 已转码内容超过播放位置 `buffer_pause_seconds` 时，暂停 FFmpeg。
- 缓冲回落到 `buffer_resume_seconds` 以下时，恢复 FFmpeg。
- 落后当前播放位置超过 `segment_retention_seconds` 的旧分片会从本地缓存删除。
- 如果超过 `idle_timeout_seconds` 没有播放活动或 HLS 访问，idle reaper 会停止会话。
- 新的 `master.m3u8` 请求如果带来不同的上游 stream URL，例如 seek 后 `StartTimeTicks` 变化，会重启本地会话。

## 协议

当前仓库还没有声明开源协议。确认使用 MIT、Apache-2.0 或其他协议后，可以补充 `LICENSE` 文件并把顶部 badge 改成正式协议。
