//go:build windows

package vmhost

import (
	"os/exec"
	"strconv"
	"syscall"
)

func detach(cmd *exec.Cmd) {
	const (
		createNewProcessGroup = 0x00000200
		detachedProcess       = 0x00000008
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}

func killProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	// taskkill /T also takes down the qemu child processes qemu may spawn for
	// its block/network backends.
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
