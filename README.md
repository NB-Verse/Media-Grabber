# Media Grabber

A simple one-page web app to save videos and audio from YouTube, Instagram, Facebook, X (Twitter), Vimeo and more than 1,000 other sites.

**What it does**

- Paste a link and see the title, thumbnail and length before you download.
- Save the **video** in the quality you choose (up to 4K where the site offers it). Files are saved as MP4 so they play on any phone or computer.
- Save **audio only** as MP3, M4A or WAV.
- Save **only a part** of a long video. Drag the slider or type a start and end time (for example `1:30` to `2:45`), and only that part is downloaded.
- See a live progress bar, and cancel at any time.

The downloading is done by [yt-dlp](https://github.com/yt-dlp/yt-dlp), the most widely used and most frequently updated downloader. It is updated every few days when sites change, which is why it fails far less often than most online downloader websites.

---

## Why it needs a small server (and can't be GitHub Pages only)

YouTube, Instagram and Facebook block web pages from fetching their videos directly from the browser. Every working downloader has a small program running behind it that fetches the file for you. In this project that program is `app.py`. It runs on your own computer, and the page opens in your browser.

**Running it on your own computer is the best option.** YouTube often blocks downloads that come from cloud servers ("Sign in to confirm you're not a bot"), but it rarely blocks a normal home or office internet connection.

---

## Quick start

### Windows

1. Install Python from <https://www.python.org/downloads/>. During setup, tick **"Add python.exe to PATH"**.
2. Download this project (green **Code** button → **Download ZIP**) and unzip it.
3. Double-click **`start.bat`**.

The first run installs everything it needs (FFmpeg, Deno and yt-dlp) and opens the app at <http://127.0.0.1:8000>. After that, double-click `start.bat` whenever you want to use it. It also updates yt-dlp each time, which keeps downloads working.

### Mac / Linux

```bash
git clone https://github.com/<your-username>/media-grabber.git
cd media-grabber
chmod +x start.sh
./start.sh
```

If it says FFmpeg or Deno is missing, run the install command it shows, then run `./start.sh` again.

### Docker (any computer or server)

```bash
docker build -t media-grabber .
docker run -p 8000:8000 media-grabber
```

Then open <http://localhost:8000>.

---

## What each part needs

| Feature | Needs |
|---|---|
| Downloading from most sites | Python + yt-dlp |
| YouTube | Deno (yt-dlp uses it to read YouTube's player) |
| HD video with sound, MP3, cutting a part | FFmpeg |

The page shows a yellow notice if FFmpeg or Deno is missing.

---

## When a download fails

**"Log in required" (Instagram, Facebook) or "Confirm you're not a bot" (YouTube)**
Some posts can only be seen when you're logged in. You can let the app use the login from your browser:

```bash
# Windows (PowerShell)
$env:COOKIES_FROM_BROWSER="firefox"; python app.py

# Mac / Linux
COOKIES_FROM_BROWSER=chrome python app.py
```

Use `chrome`, `edge`, `firefox`, `brave` or `safari`. Firefox works most reliably on Windows, because Chrome locks its cookie file while it is open. You can also export a `cookies.txt` file with a browser extension and set `COOKIES_FILE=path/to/cookies.txt`.

Keep your cookies private. Never upload `cookies.txt` to GitHub (it's already in `.gitignore`).

**It used to work and suddenly stopped**
Sites change often. Update yt-dlp:

```bash
pip install -U "yt-dlp[default]"
```

(`start.bat` and `start.sh` already do this every time.)

**Private videos** can't be downloaded unless your account can see them and you've added cookies.

---

## Settings

All optional. Set them as environment variables before starting.

| Setting | What it does | Default |
|---|---|---|
| `PORT` | Port the app runs on | `8000` |
| `HOST` | Use `0.0.0.0` to open it to other devices on your Wi-Fi | `127.0.0.1` |
| `COOKIES_FROM_BROWSER` | Use your browser login (see above) | off |
| `COOKIES_FILE` | Path to a `cookies.txt` file | off |
| `MAX_PARALLEL_DOWNLOADS` | How many downloads run at once | `2` |
| `KEEP_FILES_MINUTES` | How long finished files are kept before cleanup | `60` |
| `DOWNLOAD_DIR` | Where files are prepared | system temp folder |
| `CORS_ORIGINS` | Allow a page on another site to use this server (see below) | off |

### Using it from your phone

Start it with `HOST=0.0.0.0` on your computer, then open `http://<your-computer's-IP>:8000` on your phone while both are on the same Wi-Fi.

### Hosting the page on GitHub Pages

You can turn on GitHub Pages for this repo (Settings → Pages → deploy from `main`). The page needs a running server to do the downloading, so:

1. Run `app.py` or the Docker image on a server, with `CORS_ORIGINS=https://<your-username>.github.io`.
2. Open `https://<your-username>.github.io/media-grabber/?api=https://your-server-address`

The page remembers the server address after the first visit. Keep in mind that YouTube is more likely to block cloud servers than home connections.

### Quick links

Open `http://127.0.0.1:8000/?url=<video link>` to load a video straight away. This is handy for a phone shortcut or a browser bookmark.

---

## How it works

```
index.html  →  the page you see (one file: layout, style and logic)
app.py      →  the small server. It asks yt-dlp for the video details,
               downloads the file, uses FFmpeg to join/convert/cut it,
               then hands it to your browser.
```

| Address | Purpose |
|---|---|
| `POST /api/info` | Get title, thumbnail, length and available qualities |
| `POST /api/download` | Start a download, returns a job id |
| `GET /api/progress/<id>` | Progress of a download |
| `POST /api/cancel/<id>` | Cancel a download |
| `GET /api/file/<id>` | Save the finished file |
| `GET /api/health` | Shows whether FFmpeg and Deno are installed |

---

## Please use it responsibly

Only download videos you own, that are free to reuse, or that you have permission to save. Downloading may go against the terms of some sites, and re-uploading other people's content can break copyright law.
