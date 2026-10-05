package server

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquireOwnerLock prevents separate daemons from allocating conflicting CIDs
// or cleaning each other's live VMs. The descriptor stays open until shutdown.
func acquireOwnerLock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("state path is not an owned directory")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(dir, "owner.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open owner lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), "owner.lock")
	if err = unix.Fchmod(fd, 0600); err != nil {
		file.Close()
		return nil, err
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("state directory already owned: %w", err)
	}
	return file, nil
}
