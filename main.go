// Media Grabber helper.
//
// One small program that:
//   - serves the Media Grabber page at http://127.0.0.1:8000
//   - on first run, downloads the tools it needs (yt-dlp, Deno, FFmpeg) into its own folder
//   - keeps yt-dlp up to date by itself, and retries a failed download after updating
//   - runs the downloads and hands the finished file to the browser
//
// Nothing needs to be installed. Build:  go build -o MediaGrabber.exe .
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed index.html
var webFiles embed.FS

// Set at build time with -ldflags "-X main.appVersion=..."
var appVersion = "dev"

// ---------------------------------------------------------------------------
// Settings (all optional environment variables)
// ---------------------------------------------------------------------------
var (
	listenHost     = getenv("HOST", "127.0.0.1")
	fixedPort      = getenv("PORT", "")
	noBrowser      = getenv("MG_NO_BROWSER", "") != ""
	maxParallel    = atoi(getenv("MAX_PARALLEL_DOWNLOADS", "2"), 2)
	keepMinutes    = atoi(getenv("KEEP_FILES_MINUTES", "60"), 60)
	candidatePorts = []int{8000, 8765, 17800} // the page looks for the helper on these
)

var (
	dataDir string // where tools, settings and temporary downloads live
	binDir  string
	jobsDir string
)

// ---------------------------------------------------------------------------
// Tool state
// ---------------------------------------------------------------------------
var (
	ytdlpPath    string
	ffmpegDir    string // folder holding ffmpeg; "" when it is on PATH
	hasFFmpeg    bool
	hasDeno      bool
	ytdlpVersion atomic.Value // string
	lastUpdate   atomic.Int64 // unix seconds
	updating     atomic.Bool

	// Downloads hold a read lock while yt-dlp runs; the updater takes the write lock
	// only when nothing is running (TryLock), so it never blocks anyone.
	toolLock sync.RWMutex
)

type setupStatus struct {
	mu      sync.Mutex
	ready   bool
	stage   string
	percent float64 // -1 = unknown
	err     string
}

var setup = &setupStatus{stage: "Starting", percent: -1}

func (s *setupStatus) set(stage string, pct float64) {
	s.mu.Lock()
	s.stage, s.percent, s.err = stage, pct, ""
	s.mu.Unlock()
}

func (s *setupStatus) fail(err error) {
	s.mu.Lock()
	s.err = err.Error()
	s.mu.Unlock()
}

func (s *setupStatus) done() {
	s.mu.Lock()
	s.ready, s.stage, s.percent, s.err = true, "Ready", 100, ""
	s.mu.Unlock()
}

func (s *setupStatus) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *setupStatus) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pct any
	if s.percent >= 0 {
		pct = s.percent
	}
	return map[string]any{"stage": s.stage, "percent": pct, "error": s.err}
}

// ---------------------------------------------------------------------------
// First-run setup: download the tools
// ---------------------------------------------------------------------------
func runSetup() {
	for {
		err := setupOnce()
		if err == nil {
			setup.done()
			logf("Everything is ready. yt-dlp version %s.", currentVersion())
			break
		}
		setup.fail(err)
		logf("Setup problem: %v. Trying again in 30 seconds.", err)
		time.Sleep(30 * time.Second)
	}
	go updateLoop()
}

func setupOnce() error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	// 1. yt-dlp (always our own copy, so it can update itself)
	ytdlpPath = filepath.Join(binDir, "yt-dlp"+exeSuffix)
	if !fileExists(ytdlpPath) {
		url := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/" + ytdlpAsset()
		if err := downloadFile(url, ytdlpPath, "Getting the video downloader (yt-dlp)"); err != nil {
			return fmt.Errorf("couldn't download yt-dlp: %w", err)
		}
		_ = os.Chmod(ytdlpPath, 0o755)
	}

	// 2. Deno (yt-dlp needs it for YouTube)
	localDeno := filepath.Join(binDir, "deno"+exeSuffix)
	if _, err := exec.LookPath("deno"); err == nil || fileExists(localDeno) {
		hasDeno = true
	} else if triple := denoTriple(); triple != "" {
		zipPath := filepath.Join(binDir, "deno.zip")
		url := "https://github.com/denoland/deno/releases/latest/download/deno-" + triple + ".zip"
		if err := downloadFile(url, zipPath, "Getting the YouTube helper (Deno)"); err != nil {
			return fmt.Errorf("couldn't download Deno: %w", err)
		}
		err := extractZip(zipPath, func(name string) string {
			if path.Base(name) == "deno"+exeSuffix {
				return localDeno
			}
			return ""
		})
		_ = os.Remove(zipPath)
		if err != nil {
			return fmt.Errorf("couldn't unpack Deno: %w", err)
		}
		_ = os.Chmod(localDeno, 0o755)
		hasDeno = true
	}

	// 3. FFmpeg (joins video + audio, converts to MP3, cuts clips)
	localFF := filepath.Join(binDir, "ffmpeg"+exeSuffix)
	if fileExists(localFF) {
		ffmpegDir, hasFFmpeg = binDir, true
	} else if _, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpegDir, hasFFmpeg = "", true
	} else if url := ffmpegURL(); url != "" {
		zipPath := filepath.Join(binDir, "ffmpeg.zip")
		if err := downloadFile(url, zipPath, "Getting the video converter (FFmpeg)"); err != nil {
			return fmt.Errorf("couldn't download FFmpeg: %w", err)
		}
		setup.set("Unpacking FFmpeg", -1)
		err := extractZip(zipPath, ffmpegPicker(binDir))
		_ = os.Remove(zipPath)
		if err != nil {
			return fmt.Errorf("couldn't unpack FFmpeg: %w", err)
		}
		ffmpegDir, hasFFmpeg = binDir, true
	}

	setup.set("Checking the video downloader", -1)
	ytdlpVersion.Store(readVersion())
	return nil
}

// ffmpegPicker chooses what to unpack from the FFmpeg zip: the programs and DLLs in bin/,
// but not the video player (ffplay), which isn't needed.
func ffmpegPicker(dest string) func(string) string {
	return func(name string) string {
		dir, base := path.Base(path.Dir(name)), path.Base(name)
		if dir != "bin" || base == "ffplay.exe" {
			return ""
		}
		if strings.HasSuffix(base, ".exe") || strings.HasSuffix(base, ".dll") {
			return filepath.Join(dest, base)
		}
		return ""
	}
}

func ytdlpAsset() string {
	switch runtime.GOOS {
	case "windows":
		return "yt-dlp.exe"
	case "darwin":
		return "yt-dlp_macos"
	default:
		if runtime.GOARCH == "arm64" {
			return "yt-dlp_linux_aarch64"
		}
		return "yt-dlp_linux"
	}
}

func denoTriple() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64", "windows/arm64":
		return "x86_64-pc-windows-msvc"
	case "darwin/arm64":
		return "aarch64-apple-darwin"
	case "darwin/amd64":
		return "x86_64-apple-darwin"
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu"
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu"
	}
	return ""
}

func ffmpegURL() string {
	// Ready-made FFmpeg builds are published as zip files for Windows only.
	// On Mac/Linux FFmpeg comes from the system (brew / apt).
	if runtime.GOOS == "windows" {
		return "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl-shared.zip"
	}
	return ""
}

var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: 60 * time.Second,
	IdleConnTimeout:       90 * time.Second,
}}

type progressWriter struct {
	label     string
	total     int64
	done      int64
	lastShown int
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.total > 0 {
		pct := float64(p.done) / float64(p.total) * 100
		setup.set(p.label, pct)
		if step := int(pct) / 20; step > p.lastShown {
			p.lastShown = step
			logf("  %s: %d%%", p.label, step*20)
		}
	}
	return len(b), nil
}

func downloadFile(url, dest, label string) error {
	setup.set(label, 0)
	logf("%s (one time only)...", label)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "MediaGrabber/"+appVersion)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the download server answered %q", resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	pw := &progressWriter{label: label, total: resp.ContentLength}
	_, err = io.Copy(io.MultiWriter(f, pw), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(dest)
	return os.Rename(tmp, dest)
}

// extractZip writes every entry for which pick() returns a destination path.
func extractZip(zipPath string, pick func(name string) string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	found := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		dest := pick(f.Name)
		if dest == "" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(dest + ".part")
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		_ = os.Remove(dest)
		if err := os.Rename(dest+".part", dest); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return errors.New("the expected files were not in the download")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Keeping yt-dlp up to date
// ---------------------------------------------------------------------------
func currentVersion() string {
	v, _ := ytdlpVersion.Load().(string)
	return v
}

func readVersion() string {
	cmd := exec.Command(ytdlpPath, "--version")
	prepareCmd(cmd)
	cmd.Env = toolEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func updateLoop() {
	for {
		if updateYtDlp() {
			time.Sleep(12 * time.Hour)
		} else {
			time.Sleep(10 * time.Minute) // something was downloading; try again a bit later
		}
	}
}

// updateYtDlp asks yt-dlp to update itself. Returns true when an update check ran.
// It only runs when no download is in progress.
func updateYtDlp() bool {
	if !updating.CompareAndSwap(false, true) {
		return false
	}
	defer updating.Store(false)
	if !toolLock.TryLock() {
		return false
	}
	defer toolLock.Unlock()

	before := currentVersion()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ytdlpPath, "-U")
	prepareCmd(cmd)
	cmd.Env = toolEnv()
	_, _ = cmd.CombinedOutput()
	lastUpdate.Store(time.Now().Unix())
	after := readVersion()
	if after != "" {
		ytdlpVersion.Store(after)
	}
	if after != "" && after != before {
		logf("Updated the video downloader: %s -> %s", before, after)
	}
	return true
}

// updateAndCheck runs an update after a failure. Returns true if yt-dlp actually changed,
// meaning it is worth trying the failed download again.
func updateAndCheck() bool {
	if time.Since(time.Unix(lastUpdate.Load(), 0)) < 20*time.Minute {
		return false
	}
	before := currentVersion()
	return updateYtDlp() && currentVersion() != before
}

// ---------------------------------------------------------------------------
// Running yt-dlp
// ---------------------------------------------------------------------------
func toolEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+3)
	for _, e := range env {
		if strings.HasPrefix(strings.ToUpper(e), "PATH=") {
			e = e[:5] + binDir + string(os.PathListSeparator) + e[5:]
		}
		out = append(out, e)
	}
	return append(out, "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
}

func commonArgs() []string {
	args := []string{
		"--ignore-config", "--no-warnings",
		"--no-playlist", "--playlist-items", "1",
		"--retries", "10", "--fragment-retries", "10", "--extractor-retries", "3",
		"--socket-timeout", "30", "--concurrent-fragments", "4",
		"--windows-filenames", "--trim-filenames", "120", "--no-mtime",
	}
	if ffmpegDir != "" {
		args = append(args, "--ffmpeg-location", ffmpegDir)
	}
	return append(args, cookieArgs()...)
}

var errorLine = regexp.MustCompile(`(?m)^ERROR: (.+)$`)

func lastError(stderr string) string {
	m := errorLine.FindAllStringSubmatch(stderr, -1)
	if len(m) > 0 {
		return strings.TrimSpace(m[len(m)-1][1])
	}
	s := strings.TrimSpace(stderr)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}

// friendlyError turns yt-dlp's messages into plain English.
// kind is "login" when logging in would help, so the page can open the login help.
func friendlyError(msg string) (text, kind string) {
	low := strings.ToLower(msg)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(low, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("cookie") && has("decrypt", "could not copy", "database", "keyring", "permission"):
		return "Couldn't read your login from that browser. Close the browser completely and try again, or pick Firefox, or upload a cookies.txt file in the login help below.", "login"
	case has("sign in to confirm", "not a bot"):
		return "YouTube wants to check that you're not a robot. Open the login help below and choose a browser where you're signed in to YouTube, then try again.", "login"
	case has("login required", "log in", "login_required", "rate-limit", "rate limit", "logged-in", "authentication"):
		return "This site only shows this post to people who are logged in. Open the login help below, choose the browser you use for this site, and try again.", "login"
	case has("private"):
		return "This video is private. It can only be downloaded if your account can see it. Use the login help below.", "login"
	case has("age") && has("restrict", "confirm your age", "inappropriate"):
		return "This video is age-restricted. Use the login help below with a signed-in account.", "login"
	case has("unsupported url"):
		return "This link isn't supported. Check that it opens a single post or video.", ""
	case has("country"):
		return "This video isn't available in your country.", ""
	case has("http error 404", "does not exist", "has been removed", "no longer available", "video unavailable"):
		return "Nothing was found at this link. It may have been deleted or made private.", ""
	case has("ffmpeg") && has("not found", "not installed"):
		return "The video converter (FFmpeg) is missing. Close Media Grabber and open it again to finish setup.", ""
	case has("unable to download webpage", "timed out", "connection", "getaddrinfo", "network"):
		return "Couldn't reach the site. Check your internet connection and try again.", ""
	case has("live event will begin", "premieres in"):
		return "This video hasn't started yet. Try again after it goes live.", ""
	}
	if msg == "" {
		return "Something went wrong. Please try again.", ""
	}
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return msg, ""
}

// worthUpdating says whether a failure might be fixed by a newer yt-dlp
// (site changes), rather than being about the video itself or the user's login.
func worthUpdating(msg string) bool {
	if msg == "" {
		return false
	}
	_, kind := friendlyError(msg)
	if kind == "login" {
		return false
	}
	low := strings.ToLower(msg)
	for _, w := range []string{"unsupported url", "country", "404", "does not exist", "removed", "no longer available",
		"video unavailable", "timed out", "connection", "getaddrinfo", "network", "live event", "premieres", "cancel"} {
		if strings.Contains(low, w) {
			return false
		}
	}
	return true
}

func setCancelKill(cmd *exec.Cmd) {
	cmd.Cancel = func() error { killTree(cmd); return nil }
	cmd.WaitDelay = 5 * time.Second
}

// ---------------------------------------------------------------------------
// Video details
// ---------------------------------------------------------------------------
func fetchInfo(url string) (map[string]any, string, error) {
	run := func() (map[string]any, string, error) {
		toolLock.RLock()
		defer toolLock.RUnlock()
		args := append([]string{"-J"}, commonArgs()...)
		args = append(args, "--", url)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, ytdlpPath, args...)
		prepareCmd(cmd)
		setCancelKill(cmd)
		cmd.Env = toolEnv()
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return nil, "timed out", err
			}
			return nil, lastError(stderr.String()), err
		}
		var data map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
			return nil, "Couldn't read the video details.", err
		}
		return data, "", nil
	}
	data, msg, err := run()
	if err != nil && worthUpdating(msg) && updateAndCheck() {
		data, msg, err = run()
	}
	return data, msg, err
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func num(m map[string]any, key string) (float64, bool) {
	v, ok := m[key].(float64)
	return v, ok
}

func summarizeInfo(data map[string]any) map[string]any {
	item := data
	multiple := false
	if t := str(data, "_type"); t == "playlist" || t == "multi_video" {
		multiple = true
		if entries, ok := data["entries"].([]any); ok {
			for _, e := range entries {
				if m, ok := e.(map[string]any); ok {
					item = m
					break
				}
			}
		}
	}

	heightSet := map[int]bool{}
	anyVideo := false
	formats, _ := item["formats"].([]any)
	for _, f := range formats {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		vcodec := str(fm, "vcodec")
		if vcodec != "none" {
			anyVideo = true
		}
		if h, ok := num(fm, "height"); ok && h > 0 && vcodec != "none" {
			heightSet[int(h)] = true
		}
	}
	heights := []int{}
	for h := range heightSet {
		heights = append(heights, h)
	}
	for i := 1; i < len(heights); i++ { // sort high to low
		for j := i; j > 0 && heights[j] > heights[j-1]; j-- {
			heights[j], heights[j-1] = heights[j-1], heights[j]
		}
	}
	hasVideo := len(heights) > 0 || anyVideo
	if len(formats) == 0 {
		hasVideo = str(item, "vcodec") != "none"
	}

	thumb := str(item, "thumbnail")
	if thumb == "" {
		if ts, ok := item["thumbnails"].([]any); ok && len(ts) > 0 {
			if m, ok := ts[len(ts)-1].(map[string]any); ok {
				thumb = str(m, "url")
			}
		}
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := str(item, k); v != "" {
				return v
			}
			if v := str(data, k); v != "" {
				return v
			}
		}
		return ""
	}
	var duration any
	if d, ok := num(item, "duration"); ok {
		duration = d
	}
	isLive, _ := item["is_live"].(bool)
	title := first("title")
	if title == "" {
		title = "Untitled"
	}
	return map[string]any{
		"title":          title,
		"uploader":       first("uploader", "channel"),
		"duration":       duration,
		"thumbnail":      thumb,
		"site":           first("extractor_key"),
		"is_live":        isLive,
		"heights":        heights,
		"has_video":      hasVideo,
		"multiple_items": multiple,
	}
}

// ---------------------------------------------------------------------------
// Download jobs
// ---------------------------------------------------------------------------
type dlRequest struct {
	URL          string
	Mode         string
	Quality      string
	AudioFormat  string
	AudioBitrate string
	Start, End   *float64
	Precise      bool
}

func (r dlRequest) isClip() bool { return r.Start != nil || r.End != nil }

type job struct {
	mu        sync.Mutex
	id, dir   string
	req       dlRequest
	status    string
	stage     string
	percent   *float64
	speed     *float64
	eta       *float64
	done      *float64
	total     *float64
	errMsg    string
	errKind   string
	file      string
	filename  string
	size      int64
	cancelled bool
	cmd       *exec.Cmd
	finished  time.Time
}

var (
	jobs   = map[string]*job{}
	jobsMu sync.Mutex
	slots  chan struct{}
)

func (j *job) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	f := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	var errMsg any
	if j.errMsg != "" {
		errMsg = j.errMsg
	}
	var name any
	if j.filename != "" {
		name = j.filename
	}
	var size any
	if j.size > 0 {
		size = j.size
	}
	return map[string]any{
		"status": j.status, "stage": j.stage, "percent": f(j.percent), "speed": f(j.speed),
		"eta": f(j.eta), "downloaded": f(j.done), "total": f(j.total),
		"error": errMsg, "error_kind": j.errKind, "filename": name, "size": size,
	}
}

func (j *job) update(fn func()) {
	j.mu.Lock()
	fn()
	j.mu.Unlock()
}

func fmtSeconds(s float64) string { return strconv.FormatFloat(s, 'f', -1, 64) }

func shortTime(s float64) string {
	t := int(s)
	h, m, sec := t/3600, (t%3600)/60, t%60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, sec)
	}
	return fmt.Sprintf("%dm%02ds", m, sec)
}

const progressTemplate = "download:[MG] %(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|" +
	"%(progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s|%(progress.fragment_index)s|%(progress.fragment_count)s"

func buildArgs(j *job, simple bool) []string {
	r := j.req
	args := []string{
		"--quiet", "--progress", "--newline",
		"--progress-template", progressTemplate,
		"--progress-template", "postprocess:[MGPP] %(progress.status)s|%(progress.postprocessor)s",
		"-P", "home:" + j.dir, "-P", "temp:" + filepath.Join(j.dir, "tmp"),
	}
	args = append(args, commonArgs()...)

	tag := ""
	if r.isClip() {
		s := 0.0
		if r.Start != nil {
			s = *r.Start
		}
		end, endLabel := "inf", "end"
		if r.End != nil {
			end, endLabel = fmtSeconds(*r.End), shortTime(*r.End)
		}
		args = append(args, "--download-sections", "*"+fmtSeconds(s)+"-"+end)
		if r.Precise {
			args = append(args, "--force-keyframes-at-cuts")
		}
		tag = fmt.Sprintf(" (clip %s-%s)", shortTime(s), endLabel)
	}
	args = append(args, "-o", "%(title).90B"+strings.ReplaceAll(tag, "%", "%%")+".%(ext)s")

	if r.Mode == "audio" {
		if hasFFmpeg {
			quality := "0"
			if r.AudioFormat == "mp3" {
				quality = r.AudioBitrate + "K"
			}
			args = append(args, "-f", "ba/b", "-x", "--audio-format", r.AudioFormat, "--audio-quality", quality)
		} else {
			args = append(args, "-f", "ba[ext=m4a]/ba/b")
		}
	} else {
		height := 0
		if r.Quality != "best" {
			height, _ = strconv.Atoi(r.Quality)
		}
		switch {
		case simple:
			args = append(args, "-f", "b")
		case hasFFmpeg:
			sortBy := "res"
			if height > 0 {
				sortBy = "res:" + strconv.Itoa(height)
			}
			// Prefer MP4 so the file plays everywhere (phones, Windows, WhatsApp).
			args = append(args, "-f", "bv*+ba/b", "-S", sortBy+",vcodec:h264,ext:mp4:m4a", "--merge-output-format", "mp4")
		case height > 0:
			args = append(args, "-f", fmt.Sprintf("b[height<=%d]/b", height))
		default:
			args = append(args, "-f", "b")
		}
	}
	return append(args, "--", r.URL)
}

var errCancelled = errors.New("cancelled")

func (j *job) attempt(args []string) (string, error) {
	toolLock.RLock()
	defer toolLock.RUnlock()

	cmd := exec.Command(ytdlpPath, args...)
	prepareCmd(cmd)
	cmd.Env = toolEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	j.mu.Lock()
	if j.cancelled {
		j.mu.Unlock()
		return "", errCancelled
	}
	err = cmd.Start()
	if err == nil {
		j.cmd = cmd
	}
	j.mu.Unlock()
	if err != nil {
		return "", err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		j.parseLine(strings.TrimSpace(sc.Text()))
	}
	err = cmd.Wait()
	j.update(func() { j.cmd = nil })
	if j.isCancelled() {
		return "", errCancelled
	}
	return lastError(stderr.String()), err
}

func (j *job) isCancelled() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelled
}

func parseNum(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

func (j *job) parseLine(line string) {
	switch {
	case strings.HasPrefix(line, "[MG] "):
		p := strings.Split(strings.TrimPrefix(line, "[MG] "), "|")
		if len(p) < 8 {
			return
		}
		j.update(func() {
			if p[0] == "finished" {
				hundred := 100.0
				j.percent = &hundred
				return
			}
			if p[0] != "downloading" {
				return
			}
			j.stage = "Downloading"
			if j.req.isClip() {
				j.stage = "Downloading your clip"
			}
			done, total := parseNum(p[1]), parseNum(p[2])
			if total == nil {
				total = parseNum(p[3])
			}
			j.done, j.total, j.speed, j.eta = done, total, parseNum(p[4]), parseNum(p[5])
			j.percent = nil
			if done != nil && total != nil && *total > 0 {
				v := *done / *total * 100
				j.percent = &v
			} else if fi, fc := parseNum(p[6]), parseNum(p[7]); fi != nil && fc != nil && *fc > 0 {
				v := *fi / *fc * 100
				j.percent = &v
			}
		})
	case strings.HasPrefix(line, "[MGPP] "):
		p := strings.Split(strings.TrimPrefix(line, "[MGPP] "), "|")
		if len(p) < 2 || p[0] != "started" {
			return
		}
		name := p[1]
		stage := "Finishing up"
		switch {
		case strings.Contains(name, "Merger"):
			stage = "Joining video and audio"
		case strings.Contains(name, "ExtractAudio"):
			stage = "Converting to audio"
		case strings.Contains(name, "VideoConvertor"), strings.Contains(name, "VideoRemuxer"):
			stage = "Converting video"
		}
		j.update(func() { j.stage, j.percent = stage, nil })
	}
}

func (j *job) resetDir() {
	entries, _ := os.ReadDir(j.dir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(j.dir, e.Name()))
	}
}

func runJob(j *job) {
	slots <- struct{}{}
	defer func() { <-slots }()
	defer j.update(func() { j.finished = time.Now() })
	defer os.RemoveAll(filepath.Join(j.dir, "tmp"))

	if j.isCancelled() {
		j.update(func() { j.status, j.stage = "cancelled", "Cancelled" })
		return
	}
	j.update(func() {
		j.status, j.stage = "running", "Starting"
		if j.req.isClip() {
			j.stage = "Finding your clip"
		}
	})

	if j.req.isClip() && !hasFFmpeg {
		j.update(func() {
			j.status = "error"
			j.errMsg = "Cutting a part needs the video converter (FFmpeg), which isn't installed yet."
		})
		return
	}

	msg, err := j.attempt(buildArgs(j, false))
	if err != nil && err != errCancelled && strings.Contains(strings.ToLower(msg), "requested format is not available") {
		j.resetDir()
		msg, err = j.attempt(buildArgs(j, true))
	}
	if err != nil && err != errCancelled && worthUpdating(msg) {
		j.update(func() { j.stage, j.percent = "Updating the downloader and trying again", nil })
		if updateAndCheck() {
			j.resetDir()
			msg, err = j.attempt(buildArgs(j, false))
		}
	}

	if err == errCancelled || j.isCancelled() {
		j.update(func() { j.status, j.stage = "cancelled", "Cancelled" })
		return
	}
	if err != nil {
		text, kind := friendlyError(msg)
		logf("Download failed: %s", msg)
		j.update(func() { j.status, j.errMsg, j.errKind = "error", text, kind })
		return
	}

	var best os.DirEntry
	var bestSize int64
	entries, _ := os.ReadDir(j.dir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") || strings.HasSuffix(name, ".temp") {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() >= bestSize {
			best, bestSize = e, info.Size()
		}
	}
	if best == nil {
		j.update(func() { j.status, j.errMsg = "error", "The download finished but no file was made. Please try again." })
		return
	}
	j.update(func() {
		j.file = filepath.Join(j.dir, best.Name())
		j.filename, j.size = best.Name(), bestSize
		hundred := 100.0
		j.status, j.stage, j.percent = "done", "Ready", &hundred
	})
}

func cleanupLoop() {
	for {
		time.Sleep(5 * time.Minute)
		cutoff := time.Now().Add(-time.Duration(keepMinutes) * time.Minute)
		jobsMu.Lock()
		for id, j := range jobs {
			j.mu.Lock()
			old := !j.finished.IsZero() && j.finished.Before(cutoff)
			j.mu.Unlock()
			if old {
				_ = os.RemoveAll(j.dir)
				delete(jobs, id)
			}
		}
		jobsMu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Login settings (cookies)
// ---------------------------------------------------------------------------
type settingsData struct {
	CookiesBrowser string `json:"cookies_browser"`
}

var (
	settings   settingsData
	settingsMu sync.Mutex
	browsers   = []string{"firefox", "chrome", "edge", "brave", "opera", "vivaldi", "chromium", "safari"}
)

func settingsPath() string { return filepath.Join(dataDir, "settings.json") }
func cookiesPath() string  { return filepath.Join(dataDir, "cookies.txt") }

func loadSettings() {
	b, err := os.ReadFile(settingsPath())
	if err == nil {
		_ = json.Unmarshal(b, &settings)
	}
}

func saveSettings() error {
	b, _ := json.MarshalIndent(settings, "", "  ")
	return os.WriteFile(settingsPath(), b, 0o600)
}

func cookieArgs() []string {
	settingsMu.Lock()
	browser := settings.CookiesBrowser
	settingsMu.Unlock()
	if browser != "" {
		return []string{"--cookies-from-browser", browser}
	}
	if fileExists(cookiesPath()) {
		return []string{"--cookies", cookiesPath()}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Web server
// ---------------------------------------------------------------------------
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func validURL(u string) bool {
	low := strings.ToLower(u)
	return len(u) < 2000 && (strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://"))
}

func parseTime(v any) (*float64, error) {
	var s string
	switch t := v.(type) {
	case nil:
		return nil, nil
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		s = strings.TrimSpace(t)
	default:
		return nil, errors.New("Please write times like 1:30 or 0:01:30.")
	}
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return nil, fmt.Errorf("'%s' is not a valid time. Use 1:30 or 0:01:30.", s)
	}
	total := 0.0
	for _, p := range parts {
		n, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("'%s' is not a valid time. Use 1:30 or 0:01:30.", s)
		}
		total = total*60 + n
	}
	return &total, nil
}

func requireReady(w http.ResponseWriter) bool {
	if !setup.isReady() {
		jsonError(w, http.StatusServiceUnavailable, "Media Grabber is still getting ready. Please wait a moment.")
		return false
	}
	return true
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"app": "media-grabber", "ok": true, "version": appVersion, "os": runtime.GOOS,
		"ready": setup.isReady(), "setup": setup.snapshot(),
		"ytdlp_version": currentVersion(), "ffmpeg": hasFFmpeg, "js_runtime": hasDeno,
	})
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	if !requireReady(w) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	url := strings.TrimSpace(body.URL)
	if !validURL(url) {
		jsonError(w, 400, "Please paste a full link starting with http:// or https://")
		return
	}
	data, msg, err := fetchInfo(url)
	if err != nil {
		text, kind := friendlyError(msg)
		writeJSON(w, 422, map[string]any{"error": text, "error_kind": kind})
		return
	}
	writeJSON(w, 200, summarizeInfo(data))
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if !requireReady(w) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		jsonError(w, 400, "Bad request.")
		return
	}
	get := func(k, def string) string {
		switch v := body[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return def
	}
	req := dlRequest{
		URL: strings.TrimSpace(get("url", "")), Mode: get("mode", "video"), Quality: get("quality", "best"),
		AudioFormat: get("audio_format", "mp3"), AudioBitrate: get("audio_bitrate", "192"), Precise: true,
	}
	if p, ok := body["precise"].(bool); ok {
		req.Precise = p
	}
	switch {
	case !validURL(req.URL):
		jsonError(w, 400, "Please paste a full link starting with http:// or https://")
		return
	case req.Mode != "video" && req.Mode != "audio":
		jsonError(w, 400, "Choose video or audio.")
		return
	case !map[string]bool{"mp3": true, "m4a": true, "opus": true, "wav": true}[req.AudioFormat]:
		jsonError(w, 400, "That audio format isn't supported.")
		return
	case !map[string]bool{"128": true, "192": true, "256": true, "320": true}[req.AudioBitrate]:
		jsonError(w, 400, "That audio quality isn't supported.")
		return
	}
	if req.Quality != "best" {
		if n, err := strconv.Atoi(req.Quality); err != nil || n <= 0 || n > 10000 {
			jsonError(w, 400, "That quality isn't supported.")
			return
		}
	}
	var err error
	if req.Start, err = parseTime(body["start"]); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if req.End, err = parseTime(body["end"]); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if req.Start != nil && req.End != nil && *req.End <= *req.Start {
		jsonError(w, 400, "The end time must be after the start time.")
		return
	}

	id := randomID()
	j := &job{id: id, dir: filepath.Join(jobsDir, id), req: req, status: "queued", stage: "Waiting in line"}
	if err := os.MkdirAll(j.dir, 0o755); err != nil {
		jsonError(w, 500, "Couldn't create a folder for the download.")
		return
	}
	jobsMu.Lock()
	jobs[id] = j
	jobsMu.Unlock()
	go runJob(j)
	writeJSON(w, 200, map[string]any{"job_id": id})
}

func findJob(w http.ResponseWriter, r *http.Request) *job {
	jobsMu.Lock()
	j := jobs[r.PathValue("id")]
	jobsMu.Unlock()
	if j == nil {
		jsonError(w, 404, "This download wasn't found. It may have been cleaned up. Please start it again.")
	}
	return j
}

func handleProgress(w http.ResponseWriter, r *http.Request) {
	if j := findJob(w, r); j != nil {
		writeJSON(w, 200, j.snapshot())
	}
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	j := findJob(w, r)
	if j == nil {
		return
	}
	j.mu.Lock()
	j.cancelled = true
	cmd := j.cmd
	j.mu.Unlock()
	if cmd != nil {
		killTree(cmd)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleFile(w http.ResponseWriter, r *http.Request) {
	j := findJob(w, r)
	if j == nil {
		return
	}
	j.mu.Lock()
	file, name, status := j.file, j.filename, j.status
	j.mu.Unlock()
	if status != "done" || file == "" {
		jsonError(w, 404, "The file isn't ready yet.")
		return
	}
	f, err := os.Open(file)
	if err != nil {
		jsonError(w, 404, "The file was already cleaned up. Please download it again.")
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settingsMu.Lock()
	browser := settings.CookiesBrowser
	settingsMu.Unlock()
	writeJSON(w, 200, map[string]any{"cookies_browser": browser, "has_cookies_file": fileExists(cookiesPath()), "browsers": browsers})
}

func handlePostSettings(w http.ResponseWriter, r *http.Request) {
	var body settingsData
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		jsonError(w, 400, "Bad request.")
		return
	}
	if body.CookiesBrowser != "" && !contains(browsers, body.CookiesBrowser) {
		jsonError(w, 400, "That browser isn't supported.")
		return
	}
	settingsMu.Lock()
	settings.CookiesBrowser = body.CookiesBrowser
	err := saveSettings()
	settingsMu.Unlock()
	if err != nil {
		jsonError(w, 500, "Couldn't save the setting.")
		return
	}
	handleGetSettings(w, r)
}

func handlePostCookies(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil || !strings.Contains(string(b), "\t") {
		jsonError(w, 400, "This doesn't look like a cookies.txt file. Export it with a 'cookies.txt' browser extension.")
		return
	}
	if err := os.WriteFile(cookiesPath(), b, 0o600); err != nil {
		jsonError(w, 500, "Couldn't save the file.")
		return
	}
	handleGetSettings(w, r)
}

func handleDeleteCookies(w http.ResponseWriter, r *http.Request) {
	_ = os.Remove(cookiesPath())
	handleGetSettings(w, r)
}

func handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	b, _ := webFiles.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func isLoopbackHost(h string) bool {
	return h == "127.0.0.1" || h == "localhost" || h == "::1" || h == ""
}

// guard keeps other websites from using the helper. Only the page the helper serves itself may
// use it. The one exception is /api/health, which any page may read so the GitHub Pages copy of
// the page can find the helper and send you to it.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block "DNS rebinding" tricks when the helper only listens on this computer.
		if isLoopbackHost(listenHost) {
			host := r.Host
			if h, _, err := net.SplitHostPort(r.Host); err == nil {
				host = h
			}
			host = strings.Trim(host, "[]")
			if !isLoopbackHost(host) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		origin := r.Header.Get("Origin")
		if r.URL.Path == "/api/health" {
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET")
				if r.Header.Get("Access-Control-Request-Private-Network") != "" {
					w.Header().Set("Access-Control-Allow-Private-Network", "true")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && origin != "" && origin != "http://"+r.Host {
			jsonError(w, http.StatusForbidden, "Only the Media Grabber page can use this helper.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Start-up
// ---------------------------------------------------------------------------
func defaultDataDir() string {
	if d := os.Getenv("MG_DATA_DIR"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "MediaGrabber")
		}
	case "darwin":
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, "Library", "Application Support", "MediaGrabber")
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "share", "media-grabber")
	}
	return filepath.Join(os.TempDir(), "media-grabber")
}

func portsToTry() []int {
	if fixedPort != "" {
		return []int{atoi(fixedPort, 8000)}
	}
	return candidatePorts
}

// alreadyRunning returns the address of a Media Grabber that is already running, if any.
func alreadyRunning() string {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for _, p := range portsToTry() {
		base := fmt.Sprintf("http://127.0.0.1:%d", p)
		resp, err := client.Get(base + "/api/health")
		if err != nil {
			continue
		}
		var h map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&h)
		resp.Body.Close()
		if h["app"] == "media-grabber" {
			return base
		}
	}
	return ""
}

func main() {
	platformInit()
	fmt.Println()
	fmt.Println("  Media Grabber " + appVersion)
	fmt.Println("  ------------------------------------------")

	if base := alreadyRunning(); base != "" {
		fmt.Println("  Media Grabber is already running. Opening it in your browser...")
		if !noBrowser {
			_ = openBrowser(base)
		}
		time.Sleep(2 * time.Second)
		return
	}

	dataDir = defaultDataDir()
	binDir = filepath.Join(dataDir, "bin")
	jobsDir = filepath.Join(dataDir, "jobs")
	_ = os.RemoveAll(jobsDir) // leftovers from last time
	for _, d := range []string{dataDir, binDir, jobsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fatal("Couldn't create the folder %s: %v", d, err)
		}
	}
	loadSettings()
	if maxParallel < 1 {
		maxParallel = 1
	}
	slots = make(chan struct{}, maxParallel)

	var ln net.Listener
	var err error
	port := 0
	for _, p := range portsToTry() {
		ln, err = net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(p)))
		if err == nil {
			port = p
			break
		}
	}
	if ln == nil {
		fatal("Couldn't start: ports %v are all in use (%v). Close the other program or set PORT.", portsToTry(), err)
	}

	showHost := listenHost
	if !isLoopbackHost(listenHost) {
		showHost = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d", showHost, port)
	fmt.Println("  Running at  " + url)
	fmt.Println("  Keep this window open while you download.")
	fmt.Println("  Close it when you're done.")
	fmt.Println("  ------------------------------------------")
	fmt.Println()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handlePage)
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("OPTIONS /api/health", handleHealth)
	mux.HandleFunc("POST /api/info", handleInfo)
	mux.HandleFunc("POST /api/download", handleDownload)
	mux.HandleFunc("GET /api/progress/{id}", handleProgress)
	mux.HandleFunc("POST /api/cancel/{id}", handleCancel)
	mux.HandleFunc("GET /api/file/{id}", handleFile)
	mux.HandleFunc("GET /api/settings", handleGetSettings)
	mux.HandleFunc("POST /api/settings", handlePostSettings)
	mux.HandleFunc("POST /api/cookies", handlePostCookies)
	mux.HandleFunc("DELETE /api/cookies", handleDeleteCookies)

	go runSetup()
	go cleanupLoop()
	if !noBrowser {
		go func() {
			time.Sleep(800 * time.Millisecond)
			if err := openBrowser(url); err != nil {
				logf("Open %s in your browser.", url)
			}
		}()
	}

	srv := &http.Server{Handler: guard(mux), ReadHeaderTimeout: 20 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fatal("The helper stopped: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------
func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func atoi(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var idCounter atomic.Int64

func randomID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatInt(idCounter.Add(1), 36)
}

func logf(format string, args ...any) {
	fmt.Printf("  %s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func fatal(format string, args ...any) {
	fmt.Println()
	fmt.Println("  " + fmt.Sprintf(format, args...))
	waitBeforeExit()
	os.Exit(1)
}
