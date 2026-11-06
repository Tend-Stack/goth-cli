package credentials

import (
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
)

// Windows disk state contains only an origin/backend marker and lock, never a
// credential. Native Credential Manager is required until ACL fallback is proven.
func privateInfo(info os.FileInfo, dir bool) bool {
	if dir {
		return info.IsDir()
	}
	return info.Mode().IsRegular()
}

// Credential Manager is called in-process; CommandContext kills the helper.
func boundHelper(cmd *exec.Cmd) {}
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
