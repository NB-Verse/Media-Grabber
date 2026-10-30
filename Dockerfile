FROM python:3.12-slim

# FFmpeg for merging, converting and cutting. Deno so yt-dlp can handle YouTube.
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=denoland/deno:bin /deno /usr/local/bin/deno

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py index.html ./

ENV HOST=0.0.0.0 PORT=8000 DOWNLOAD_DIR=/tmp/media-grabber
EXPOSE 8000

# One worker (download jobs live in memory) with several threads.
CMD gunicorn -w 1 --threads 8 --timeout 0 -b 0.0.0.0:${PORT} app:app
