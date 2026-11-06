//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package credentials

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func privateInfo(info os.FileInfo, dir bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return false
	}
	if dir {
		return info.IsDir() && info.Mode().Perm() == 0700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600 && stat.Nlink == 1
}
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }

// macOS's keychain provider starts /usr/bin/security. Cancel the helper's
// process group so an unlock prompt cannot outlive the bounded operation.
func boundHelper(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
