FROM public.ecr.aws/docker/library/golang:1.26-bookworm AS builder

WORKDIR /src

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/emby-transcoder ./cmd/emby-transcoder

FROM public.ecr.aws/docker/library/debian:bookworm-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		ca-certificates \
		ffmpeg \
		intel-media-va-driver \
		mesa-va-drivers \
		vainfo \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/emby-transcoder /usr/local/bin/emby-transcoder

RUN mkdir -p /app/config /var/lib/emby-transcoder/transcode

EXPOSE 8097

ENTRYPOINT ["/usr/local/bin/emby-transcoder"]
CMD ["-config", "/app/config/config.json"]
