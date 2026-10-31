# Media Grabber

Save videos and audio from YouTube, Instagram, Facebook, X (Twitter), Vimeo and more than 1,000 other sites.

- **Video:** pick the quality you want, up to 4K where the site offers it. Files are saved as MP4, so they play on any phone or computer.
- **Audio only:** save as MP3, M4A or WAV.
- **Only a part:** drag the slider or type a start and end time (for example `1:30` to `2:45`), and only that part is downloaded.
- **Login help:** for Instagram or Facebook posts that need you to be logged in, Media Grabber can use the login from your browser.
- **Stays working by itself:** the downloader inside updates itself. If a download fails because a site changed, Media Grabber updates and tries again on its own.

---

## How to use it (Windows)

1. Go to this project's **Releases** page and download **`MediaGrabber.exe`**.
2. Double-click it. Nothing else needs to be installed: no Python, no extra software.
3. If Windows shows "Windows protected your PC", click **More info**, then **Run anyway**. (This appears for any new program that isn't from a big publisher.)
4. **The first time only**, it sets itself up by downloading its tools (about 150 MB). This takes a minute or two. Your browser then opens Media Grabber.
5. Paste a link, choose video or audio, and click Download.

Keep the black Media Grabber window open while you download, and close it when you're done. Next time, just double-click `MediaGrabber.exe` again.

**Mac:** download `MediaGrabber-mac-apple-chip` (M1 or newer) or `MediaGrabber-mac-intel`. Install FFmpeg once with `brew install ffmpeg`.
**Linux:** download `MediaGrabber-linux`, then run `chmod +x MediaGrabber-linux && ./MediaGrabber-linux`. Install FFmpeg with `sudo apt install ffmpeg`.

---

## Putting it on GitHub

1. Create a new repository on GitHub and upload all the files in this folder (including the hidden `.github` folder).
2. GitHub builds `MediaGrabber.exe` by itself and publishes it under **Releases** a few minutes later. You can watch it in the **Actions** tab. It rebuilds every time you change the program or the page.
3. *(Optional)* Turn on GitHub Pages: **Settings → Pages → Deploy from a branch → main → / (root)**. Your page at `https://<your-name>.github.io/<repo>/` then shows a **Download for Windows** button. If Media Grabber is already running on the visitor's computer, the page takes them straight to it.

If the Actions build doesn't start, go to **Settings → Actions → General** and make sure Actions are allowed and "Workflow permissions" is set to **Read and write**.

---

## When a download fails

| What you see | What to do |
|---|---|
| "Only shown to people who are logged in" (Instagram, Facebook) | Open **"Instagram or Facebook asking you to log in?"** at the bottom of the page and choose the browser you use for that site. **Firefox works best on Windows.** Chrome and Edge often lock their login data while they're open, so close them fully first, or use a `cookies.txt` file. |
| YouTube "check that you're not a robot" | Same as above. Choose a browser where you're signed in to YouTube. |
| "This video is private" | Only works if your own account can see it, with the login option above. |
| "Couldn't reach the site" | Check your internet connection. |
| Something else, after it used to work | Close Media Grabber and open it again. It checks for updates every time it starts and every 12 hours while it runs. |

Your login stays on your computer. Media Grabber never sees or stores your password.

---

## Where things are kept

Everything Media Grabber downloads for itself lives in one folder, which you can delete at any time to start fresh:

- Windows: `%LOCALAPPDATA%\MediaGrabber`
- Mac: `~/Library/Application Support/MediaGrabber`
- Linux: `~/.local/share/media-grabber`

The files you download go to your browser's normal Downloads folder. The working copies inside Media Grabber's folder are cleaned up after an hour.

---

## Safety

- Media Grabber only listens on your own computer (`127.0.0.1`). Other devices can't reach it unless you turn that on (see Settings below).
- Other websites can't use it. Only the Media Grabber page it shows you can start downloads.
- Its tools come straight from their official GitHub pages: [yt-dlp](https://github.com/yt-dlp/yt-dlp), [Deno](https://github.com/denoland/deno) and [FFmpeg builds by yt-dlp](https://github.com/yt-dlp/FFmpeg-Builds).

---

## Settings (optional, for advanced users)

Set these as environment variables before starting Media Grabber.

| Setting | What it does | Default |
|---|---|---|
| `PORT` | Port to use | tries 8000, then 8765, then 17800 |
| `HOST` | Set to `0.0.0.0` to use it from your phone on the same Wi-Fi | `127.0.0.1` |
| `MAX_PARALLEL_DOWNLOADS` | How many downloads run at the same time | `2` |
| `KEEP_FILES_MINUTES` | How long finished files are kept for saving | `60` |
| `MG_DATA_DIR` | Where Media Grabber keeps its tools | see above |
| `MG_NO_BROWSER` | Set to `1` to stop the browser opening by itself | off |

**Running on a server with Docker:**

```bash
docker build -t media-grabber .
docker run -p 8000:8000 -v media-grabber-data:/data media-grabber
```

Keep in mind that YouTube blocks cloud servers much more often than home internet connections.

---

## For developers

The whole program is in `main.go` (plus two small files for Windows and for Mac/Linux). The page is `index.html`, and it's built into the program. Nothing outside Go's standard library is used.

```bash
go test ./...
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o MediaGrabber.exe .
```

| Address | What it does |
|---|---|
| `GET /api/health` | Whether it's ready, setup progress, tool versions |
| `POST /api/info` | Title, thumbnail, length and qualities for a link |
| `POST /api/download` | Starts a download and returns a job id |
| `GET /api/progress/{id}` | Progress of a download |
| `POST /api/cancel/{id}` | Cancels a download |
| `GET /api/file/{id}` | The finished file |
| `GET/POST /api/settings` | Login browser setting |
| `POST/DELETE /api/cookies` | Upload or remove a cookies.txt file |

---

## Please use it responsibly

Only download videos you own, that are free to reuse, or that you have permission to save. Downloading may go against some sites' terms, and re-uploading other people's content can break copyright law.
