//go:build !windows

package main

import (
	"os/exec"
	"runtime"
	"syscall"
)

const exeSuffix = ""

// prepareCmd puts the tool in its own process group so Cancel can stop it and its children.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

func openBrowser(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

func platformInit()   {}
func waitBeforeExit() {}
