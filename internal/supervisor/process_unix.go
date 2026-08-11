//go:build linux || darwin

package supervisor

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureProcessTree(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(command *exec.Cmd) error {
	return signalProcessTree(command, syscall.SIGTERM)
}

func killProcessTree(command *exec.Cmd) error {
	return signalProcessTree(command, syscall.SIGKILL)
}

func signalProcessTree(command *exec.Cmd, signal syscall.Signal) error {
	if command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
