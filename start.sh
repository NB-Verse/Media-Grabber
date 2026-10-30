#!/usr/bin/env bash
# Media Grabber - Mac / Linux starter.  Run:  ./start.sh
set -e
cd "$(dirname "$0")"

command -v python3 >/dev/null || { echo "Please install Python 3.10 or newer first."; exit 1; }

if ! command -v ffmpeg >/dev/null; then
  echo "FFmpeg not found. Install it with:"
  echo "  Mac:    brew install ffmpeg"
  echo "  Ubuntu: sudo apt install ffmpeg"
fi
if ! command -v deno >/dev/null; then
  echo "Deno not found (needed for YouTube). Install it with:"
  echo "  curl -fsSL https://deno.land/install.sh | sh"
fi

[ -d .venv ] || python3 -m venv .venv
source .venv/bin/activate

echo "Updating the downloader to the latest version..."
pip install -q --upgrade pip
pip install -q --upgrade -r requirements.txt

( sleep 2; (open http://127.0.0.1:8000 || xdg-open http://127.0.0.1:8000) >/dev/null 2>&1 ) &
python app.py
