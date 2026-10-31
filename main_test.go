package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestFFmpegPicker(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "ff.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	for _, name := range []string{
		"ffmpeg-x/bin/ffmpeg.exe", "ffmpeg-x/bin/ffprobe.exe", "ffmpeg-x/bin/ffplay.exe",
		"ffmpeg-x/bin/avcodec-63.dll", "ffmpeg-x/doc/readme.txt", "ffmpeg-x/lib/avcodec.lib",
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(name))
	}
	zw.Close()
	f.Close()

	out := filepath.Join(dir, "bin")
	_ = os.MkdirAll(out, 0o755)
	if err := extractZip(zipPath, ffmpegPicker(out)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(out)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{"avcodec-63.dll", "ffmpeg.exe", "ffprobe.exe"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]float64{"90": 90, "1:30": 90, "0:01:30": 90, "1:02:03": 3723, "12.5": 12.5}
	for in, want := range cases {
		got, err := parseTime(in)
		if err != nil || got == nil || *got != want {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "1:2:3:4", "-5"} {
		if _, err := parseTime(bad); err == nil {
			t.Errorf("parseTime(%q) should fail", bad)
		}
	}
	if got, err := parseTime(""); got != nil || err != nil {
		t.Errorf("empty time should mean 'not set'")
	}
}

func TestFriendlyErrorAndUpdates(t *testing.T) {
	if _, kind := friendlyError("[Instagram] abc: Requested content is not available, rate-limit reached or login required"); kind != "login" {
		t.Error("Instagram login error should point to login help")
	}
	if worthUpdating("[Instagram] abc: login required") {
		t.Error("login problems are not fixed by updating")
	}
	if !worthUpdating("[youtube] abc: Unable to extract nsig function") {
		t.Error("extractor breakage should trigger an update")
	}
	if worthUpdating("Unsupported URL: https://example.com") {
		t.Error("unsupported links are not fixed by updating")
	}
}
