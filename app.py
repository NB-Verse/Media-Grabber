"""
Media Grabber - a small web app to download video or audio from
YouTube, Instagram, Facebook and 1,000+ other sites.

The heavy lifting is done by yt-dlp (https://github.com/yt-dlp/yt-dlp),
the most actively maintained downloader available. FFmpeg is used for
merging, audio conversion and cutting clips.

Run:  python app.py      then open http://127.0.0.1:8000
"""

import os
import re
import shutil
import tempfile
import threading
import time
import uuid
from pathlib import Path

from flask import Flask, jsonify, request, send_file, send_from_directory

import yt_dlp
from yt_dlp.utils import DownloadCancelled, DownloadError, download_range_func

# --------------------------------------------------------------------------
# Settings (all optional, set as environment variables)
# --------------------------------------------------------------------------
HOST = os.environ.get("HOST", "127.0.0.1")
PORT = int(os.environ.get("PORT", "8000"))
COOKIES_FILE = os.environ.get("COOKIES_FILE", "").strip()            # path to a cookies.txt
COOKIES_FROM_BROWSER = os.environ.get("COOKIES_FROM_BROWSER", "").strip()  # chrome / firefox / edge ...
FFMPEG_LOCATION = os.environ.get("FFMPEG_LOCATION", "").strip()
CORS_ORIGINS = [o.strip() for o in os.environ.get("CORS_ORIGINS", "").split(",") if o.strip()]
MAX_PARALLEL = int(os.environ.get("MAX_PARALLEL_DOWNLOADS", "2"))
KEEP_FILES_MINUTES = int(os.environ.get("KEEP_FILES_MINUTES", "60"))

BASE_DIR = Path(__file__).resolve().parent
WORK_DIR = Path(os.environ.get("DOWNLOAD_DIR", Path(tempfile.gettempdir()) / "media-grabber"))
WORK_DIR.mkdir(parents=True, exist_ok=True)

app = Flask(__name__)

JOBS = {}
JOBS_LOCK = threading.Lock()
SLOTS = threading.Semaphore(MAX_PARALLEL)


# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------
def has_ffmpeg():
    if FFMPEG_LOCATION:
        return Path(FFMPEG_LOCATION).exists()
    return shutil.which("ffmpeg") is not None


def has_js_runtime():
    return any(shutil.which(x) for x in ("deno", "node", "bun", "qjs"))


def base_options():
    """Options shared by every yt-dlp call."""
    opts = {
        "quiet": True,
        "no_warnings": True,
        "noprogress": True,
        "noplaylist": True,
        "playlist_items": "1",
        "retries": 10,
        "fragment_retries": 10,
        "extractor_retries": 3,
        "socket_timeout": 30,
        "concurrent_fragment_downloads": 4,
        "windowsfilenames": True,
        "trim_file_name": 120,
    }
    if FFMPEG_LOCATION:
        opts["ffmpeg_location"] = FFMPEG_LOCATION
    if COOKIES_FILE and Path(COOKIES_FILE).exists():
        opts["cookiefile"] = COOKIES_FILE
    elif COOKIES_FROM_BROWSER:
        opts["cookiesfrombrowser"] = (COOKIES_FROM_BROWSER,)
    return opts


def valid_url(url):
    return bool(url) and re.match(r"^https?://", url, re.I) is not None and len(url) < 2000


def parse_time(value):
    """Turn '1:02:03', '02:03', '123' or '123.5' into seconds. Empty -> None."""
    if value is None:
        return None
    value = str(value).strip()
    if not value:
        return None
    parts = value.split(":")
    if len(parts) > 3:
        raise ValueError(f"'{value}' is not a valid time")
    try:
        seconds = 0.0
        for p in parts:
            seconds = seconds * 60 + float(p)
    except ValueError:
        raise ValueError(f"'{value}' is not a valid time")
    if seconds < 0:
        raise ValueError("Time cannot be negative")
    return seconds


def fmt_time(seconds):
    seconds = int(seconds)
    h, rem = divmod(seconds, 3600)
    m, s = divmod(rem, 60)
    return f"{h}h{m:02d}m{s:02d}s" if h else f"{m}m{s:02d}s"


def friendly_error(err):
    """Translate yt-dlp errors into plain English."""
    msg = re.sub(r"\x1b\[[0-9;]*m", "", str(err))
    msg = msg.replace("ERROR: ", "")
    low = msg.lower()
    if "sign in to confirm" in low or "not a bot" in low:
        return ("YouTube is asking to confirm you're not a bot. This usually happens on cloud "
                "servers. Run the app on your own computer, or add browser cookies (see README).")
    if "login required" in low or "log in" in low or "login_required" in low or "rate-limit" in low:
        return ("This site wants you to be logged in to see this post. Add your browser "
                "cookies (see README, 'Cookies') and try again.")
    if "private" in low:
        return "This video is private, so it can't be downloaded."
    if "unsupported url" in low:
        return "This link isn't supported. Check that it's a direct link to a post or video."
    if "age" in low and "restrict" in low:
        return "This video is age-restricted. Add browser cookies from a signed-in account (see README)."
    if "not available" in low and "country" in low:
        return "This video isn't available in your country."
    if "ffmpeg" in low and ("not found" in low or "not installed" in low):
        return "FFmpeg is missing. Install it (see README) to merge, convert or cut files."
    if "http error 404" in low or "does not exist" in low:
        return "Nothing was found at this link. It may have been deleted."
    if "unable to download webpage" in low or "timed out" in low or "connection" in low:
        return "Couldn't reach the site. Check your internet connection and try again."
    return msg.strip()[:400] or "Something went wrong."


def first_entry(info):
    """Instagram carousels / playlists come back as a list - use the first item."""
    if info and info.get("_type") in ("playlist", "multi_video") and info.get("entries"):
        for e in info["entries"]:
            if e:
                return e
    return info


# --------------------------------------------------------------------------
# Download job
# --------------------------------------------------------------------------
def build_download_options(job, req):
    mode = req["mode"]
    quality = req.get("quality", "best")
    start, end = req.get("start"), req.get("end")
    precise = req.get("precise", True)
    ffmpeg = has_ffmpeg()

    opts = base_options()
    opts["paths"] = {"home": str(job["dir"]), "temp": str(job["dir"] / "tmp")}

    clip_tag = ""
    if start is not None or end is not None:
        if not ffmpeg:
            raise ValueError("Cutting a part of the video needs FFmpeg. Install it first (see README).")
        s = start or 0
        e = end if end is not None else float("inf")
        opts["download_ranges"] = download_range_func(None, [(s, e)])
        opts["force_keyframes_at_cuts"] = bool(precise)
        clip_tag = f" (clip {fmt_time(s)}-{fmt_time(e) if e != float('inf') else 'end'})"

    opts["outtmpl"] = {"default": "%(title).90B" + clip_tag.replace("%", "%%") + ".%(ext)s"}

    if mode == "audio":
        audio_format = req.get("audio_format", "mp3")
        bitrate = str(req.get("audio_bitrate", "192"))
        if ffmpeg:
            opts["format"] = "ba/b"
            opts["postprocessors"] = [{
                "key": "FFmpegExtractAudio",
                "preferredcodec": audio_format,
                "preferredquality": bitrate if audio_format == "mp3" else "0",
            }]
        else:
            # No FFmpeg: grab the best m4a audio stream as-is.
            opts["format"] = "ba[ext=m4a]/ba/b"
    else:
        height = None if quality in ("best", "", None) else int(quality)
        if ffmpeg:
            opts["format"] = "bv*+ba/b"
            sort = [f"res:{height}"] if height else ["res"]
            # Prefer MP4/M4A so the file plays everywhere (phones, Windows, WhatsApp).
            sort += ["vcodec:h264", "ext:mp4:m4a"]
            opts["format_sort"] = sort
            opts["merge_output_format"] = "mp4"
        else:
            # No FFmpeg: only single files that already contain video + audio.
            opts["format"] = f"b[height<={height}]/b" if height else "b"
    return opts


def run_job(job_id):
    job = JOBS[job_id]
    req = job["request"]

    def progress_hook(d):
        if job["cancel"]:
            raise DownloadCancelled("Cancelled")
        if d["status"] == "downloading":
            total = d.get("total_bytes") or d.get("total_bytes_estimate")
            done = d.get("downloaded_bytes") or 0
            job["stage"] = "Downloading"
            job["percent"] = round(done / total * 100, 1) if total else None
            job["speed"] = d.get("speed")
            job["eta"] = d.get("eta")
            job["downloaded"] = done
            job["total"] = total
        elif d["status"] == "finished":
            job["percent"] = 100

    def pp_hook(d):
        if job["cancel"]:
            raise DownloadCancelled("Cancelled")
        if d["status"] == "started":
            names = {
                "FFmpegMerger": "Joining video and audio",
                "FFmpegExtractAudio": "Converting to audio",
                "FFmpegVideoConvertor": "Converting video",
                "FFmpegCopyStream": "Cutting your clip",
            }
            job["stage"] = names.get(d.get("postprocessor"), "Finishing up")
            job["percent"] = None

    with SLOTS:
        if job["cancel"]:
            job["status"] = "cancelled"
            return
        job["status"] = "running"
        job["stage"] = "Cutting your clip" if req.get("start") is not None or req.get("end") is not None else "Starting"
        try:
            opts = build_download_options(job, req)
            opts["progress_hooks"] = [progress_hook]
            opts["postprocessor_hooks"] = [pp_hook]
            try:
                with yt_dlp.YoutubeDL(opts) as ydl:
                    ydl.download([req["url"]])
            except DownloadError as e:
                # Second chance: fall back to the simplest "best" format.
                if "requested format is not available" in str(e).lower() and not job["cancel"]:
                    opts.pop("format_sort", None)
                    opts["format"] = "b" if req["mode"] == "video" else "ba/b"
                    with yt_dlp.YoutubeDL(opts) as ydl:
                        ydl.download([req["url"]])
                else:
                    raise

            files = [p for p in job["dir"].iterdir()
                     if p.is_file() and not p.name.endswith((".part", ".ytdl", ".temp"))]
            if not files:
                raise RuntimeError("The download finished but no file was produced.")
            final = max(files, key=lambda p: p.stat().st_size)
            job["file"] = final
            job["filename"] = final.name
            job["size"] = final.stat().st_size
            job["status"] = "done"
            job["stage"] = "Ready"
            job["percent"] = 100
        except DownloadCancelled:
            job["status"] = "cancelled"
            job["stage"] = "Cancelled"
        except Exception as e:  # noqa: BLE001
            job["status"] = "cancelled" if job["cancel"] else "error"
            job["error"] = friendly_error(e) if isinstance(e, DownloadError) else str(e)
        finally:
            job["finished_at"] = time.time()
            shutil.rmtree(job["dir"] / "tmp", ignore_errors=True)


def cleanup_loop():
    """Delete finished downloads after KEEP_FILES_MINUTES."""
    while True:
        time.sleep(300)
        cutoff = time.time() - KEEP_FILES_MINUTES * 60
        with JOBS_LOCK:
            old = [jid for jid, j in JOBS.items() if j.get("finished_at") and j["finished_at"] < cutoff]
            for jid in old:
                shutil.rmtree(JOBS[jid]["dir"], ignore_errors=True)
                JOBS.pop(jid, None)


# --------------------------------------------------------------------------
# Routes
# --------------------------------------------------------------------------
@app.after_request
def add_cors(resp):
    origin = request.headers.get("Origin")
    if origin and ("*" in CORS_ORIGINS or origin in CORS_ORIGINS):
        resp.headers["Access-Control-Allow-Origin"] = origin
        resp.headers["Access-Control-Allow-Headers"] = "Content-Type"
        resp.headers["Access-Control-Allow-Methods"] = "GET, POST, OPTIONS"
        resp.headers["Access-Control-Expose-Headers"] = "Content-Disposition"
    return resp


@app.route("/")
def index():
    return send_from_directory(BASE_DIR, "index.html")


@app.route("/api/health")
def health():
    return jsonify({
        "ok": True,
        "ytdlp_version": yt_dlp.version.__version__,
        "ffmpeg": has_ffmpeg(),
        "js_runtime": has_js_runtime(),
        "cookies": bool((COOKIES_FILE and Path(COOKIES_FILE).exists()) or COOKIES_FROM_BROWSER),
    })


@app.route("/api/info", methods=["POST", "OPTIONS"])
def info():
    if request.method == "OPTIONS":
        return "", 204
    url = (request.get_json(silent=True) or {}).get("url", "").strip()
    if not valid_url(url):
        return jsonify({"error": "Please paste a full link starting with http:// or https://"}), 400
    try:
        with yt_dlp.YoutubeDL(base_options()) as ydl:
            data = ydl.extract_info(url, download=False)
    except DownloadError as e:
        return jsonify({"error": friendly_error(e)}), 422
    except Exception as e:  # noqa: BLE001
        return jsonify({"error": friendly_error(e)}), 500

    multiple = data.get("_type") in ("playlist", "multi_video")
    item = first_entry(data) or {}
    formats = item.get("formats") or []
    heights = sorted({f["height"] for f in formats
                      if f.get("height") and f.get("vcodec") not in (None, "none")}, reverse=True)
    # "none" means the site told us there's no picture (e.g. a podcast). Unknown counts as video.
    if formats:
        has_video = bool(heights) or any(f.get("vcodec") != "none" for f in formats)
    else:
        has_video = item.get("vcodec") != "none"

    thumb = item.get("thumbnail")
    if not thumb and item.get("thumbnails"):
        thumb = item["thumbnails"][-1].get("url")

    return jsonify({
        "title": item.get("title") or data.get("title") or "Untitled",
        "uploader": item.get("uploader") or item.get("channel") or data.get("uploader"),
        "duration": item.get("duration"),
        "thumbnail": thumb,
        "site": item.get("extractor_key") or data.get("extractor_key"),
        "is_live": bool(item.get("is_live")),
        "heights": heights,
        "has_video": has_video,
        "multiple_items": multiple,
    })


@app.route("/api/download", methods=["POST", "OPTIONS"])
def start_download():
    if request.method == "OPTIONS":
        return "", 204
    body = request.get_json(silent=True) or {}
    url = str(body.get("url", "")).strip()
    if not valid_url(url):
        return jsonify({"error": "Please paste a full link starting with http:// or https://"}), 400

    mode = body.get("mode", "video")
    if mode not in ("video", "audio"):
        return jsonify({"error": "Mode must be video or audio"}), 400
    audio_format = body.get("audio_format", "mp3")
    if audio_format not in ("mp3", "m4a", "opus", "wav"):
        return jsonify({"error": "Unsupported audio format"}), 400
    quality = str(body.get("quality", "best"))
    if quality != "best" and not quality.isdigit():
        return jsonify({"error": "Unsupported quality"}), 400

    try:
        start = parse_time(body.get("start"))
        end = parse_time(body.get("end"))
    except ValueError as e:
        return jsonify({"error": str(e)}), 400
    if start is not None and end is not None and end <= start:
        return jsonify({"error": "The end time must be after the start time."}), 400

    job_id = uuid.uuid4().hex
    job_dir = WORK_DIR / job_id
    job_dir.mkdir(parents=True, exist_ok=True)
    job = {
        "id": job_id, "dir": job_dir, "status": "queued", "stage": "Waiting in line",
        "percent": None, "speed": None, "eta": None, "downloaded": None, "total": None,
        "error": None, "file": None, "filename": None, "size": None, "cancel": False,
        "created_at": time.time(), "finished_at": None,
        "request": {
            "url": url, "mode": mode, "quality": quality,
            "audio_format": audio_format, "audio_bitrate": body.get("audio_bitrate", "192"),
            "start": start, "end": end, "precise": bool(body.get("precise", True)),
        },
    }
    with JOBS_LOCK:
        JOBS[job_id] = job
    threading.Thread(target=run_job, args=(job_id,), daemon=True).start()
    return jsonify({"job_id": job_id})


@app.route("/api/progress/<job_id>")
def progress(job_id):
    job = JOBS.get(job_id)
    if not job:
        return jsonify({"error": "Unknown download"}), 404
    keys = ("status", "stage", "percent", "speed", "eta", "downloaded", "total", "error", "filename", "size")
    return jsonify({k: job[k] for k in keys})


@app.route("/api/cancel/<job_id>", methods=["POST", "OPTIONS"])
def cancel(job_id):
    if request.method == "OPTIONS":
        return "", 204
    job = JOBS.get(job_id)
    if not job:
        return jsonify({"error": "Unknown download"}), 404
    job["cancel"] = True
    return jsonify({"ok": True})


@app.route("/api/file/<job_id>")
def get_file(job_id):
    job = JOBS.get(job_id)
    if not job or job["status"] != "done" or not job["file"] or not Path(job["file"]).exists():
        return jsonify({"error": "File not ready or already removed"}), 404
    return send_file(job["file"], as_attachment=True, download_name=job["filename"])


threading.Thread(target=cleanup_loop, daemon=True).start()

if __name__ == "__main__":
    print(f"\n  Media Grabber running at  http://{'127.0.0.1' if HOST in ('0.0.0.0', '') else HOST}:{PORT}\n")
    if not has_ffmpeg():
        print("  ! FFmpeg not found - audio conversion, merging and clipping are limited.")
    if not has_js_runtime():
        print("  ! Deno not found - YouTube downloads may fail. Install Deno (see README).")
    app.run(host=HOST, port=PORT, threaded=True)
