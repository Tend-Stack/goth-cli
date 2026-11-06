package workspace

import (
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
)

const noFollow = 0

func owned(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink == 0 }
func lock(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
func boundGit(cmd *exec.Cmd) {}

// Windows directory flush durability requires native acceptance; file writes
// still call Sync. Do not describe cross-builds as crash-recovery proof there.
func syncDirectory(*os.Root, string) error { return nil }
