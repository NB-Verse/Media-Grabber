//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

const exeSuffix = ".exe"

const createNoWindow = 0x08000000

// prepareCmd stops tool windows (yt-dlp, FFmpeg) from flashing up on screen.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// killTree stops yt-dlp and everything it started (FFmpeg etc.).
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	prepareCmd(k)
	if err := k.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

func openBrowser(url string) error {
	c := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	prepareCmd(c)
	return c.Start()
}

// platformInit turns off "QuickEdit" in the console window. Otherwise a stray click inside the
// window pauses the whole program until a key is pressed, which looks like a frozen download.
func platformInit() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getStdHandle := kernel32.NewProc("GetStdHandle")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")
	setTitle := kernel32.NewProc("SetConsoleTitleW")

	const stdInputHandle = ^uintptr(10 - 1) // (DWORD)-10
	const enableQuickEdit, enableExtended = 0x0040, 0x0080
	h, _, _ := getStdHandle.Call(stdInputHandle)
	var mode uint32
	if r, _, _ := getMode.Call(h, uintptr(unsafe.Pointer(&mode))); r != 0 {
		_, _, _ = setMode.Call(h, uintptr((mode&^enableQuickEdit)|enableExtended))
	}
	if t, err := syscall.UTF16PtrFromString("Media Grabber - keep this window open"); err == nil {
		_, _, _ = setTitle.Call(uintptr(unsafe.Pointer(t)))
	}
}

func waitBeforeExit() {
	fmt.Println("  Press Enter to close this window.")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
