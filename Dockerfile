# Media Grabber on a server (optional). Most people should just run MediaGrabber.exe on their own computer.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod *.go index.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.appVersion=docker" -o /media-grabber .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /media-grabber /usr/local/bin/media-grabber

# yt-dlp and Deno are downloaded into /data on first start and kept up to date automatically.
ENV HOST=0.0.0.0 PORT=8000 MG_NO_BROWSER=1 MG_DATA_DIR=/data
VOLUME /data
EXPOSE 8000
CMD ["media-grabber"]
