//go:build !windows

package vmhost

import (
	"os/exec"
	"syscall"
	"time"
)

// detach puts qemu in its own session: the provider agent may be restarted or
// closed while a buyer's VM is still running, and the VM must outlive it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return nil // already gone
	}
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != nil {
			return nil
		}
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
