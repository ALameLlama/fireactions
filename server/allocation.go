package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The reservation lives in the private journal namespace. It establishes staging
// authority before mkdir; final paths gain authority only through a full marker.
func (s *StateStore) allocationPaths(r *stateRecord) (stage, reservation string) {
	name := r.InstanceID + "-" + r.VMID
	return filepath.Join(filepath.Dir(filepath.Dir(r.APISocketPath)), ".staging-"+name), filepath.Join(s.dir, ".allocation-"+name)
}

func (s *StateStore) checkAllocationAncestors(path string) error {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("unsafe allocation ancestor: %s", parent)
		}
		if parent == s.root {
			return nil
		}
		if parent == "/" {
			return fmt.Errorf("allocation path escapes state directory")
		}
	}
}

func (s *StateStore) prepareResourceDirectory(r *stateRecord) error {
	stage, reservation := s.allocationPaths(r)
	final := filepath.Dir(r.APISocketPath)
	for _, path := range []string{final, stage, reservation} {
		if err := s.checkAllocationAncestors(path); err != nil {
			return err
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("allocation path already exists or cannot be checked: %s", path)
		}
	}
	if err := writeOwnershipMarker(reservation, r); err != nil {
		return err
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		return err
	}
	if err := writeOwnershipMarker(filepath.Join(stage, ".owner"), r); err != nil {
		return err
	}
	if err := s.checkProvisioning(r.VMID); err != nil {
		return err
	}
	// os.Rename can replace a foreign empty directory. NOREPLACE cannot.
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, final, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("publishing marked VM directory: %w", err)
	}
	parent, err := os.Open(filepath.Dir(final))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

// markerPrefix validates partial writes only inside the exclusive reservation
// namespace or a staging directory already authorized by a complete reservation.
func (s *StateStore) markerPrefix(path string, r *stateRecord) (present, complete bool, err error) {
	if err := s.checkAllocationAncestors(path); err != nil {
		return false, false, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if alreadyGone(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat, root unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, false, err
	}
	if err := unix.Stat(s.root, &root); err != nil {
		return false, false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != root.Uid || stat.Mode&0077 != 0 {
		return false, false, fmt.Errorf("allocation marker ownership conflict")
	}
	expected := ownershipMarker(r)
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected)+1)))
	if err != nil {
		return false, false, err
	}
	if !strings.HasPrefix(expected, string(data)) {
		return false, false, fmt.Errorf("allocation marker identity conflict")
	}
	return true, string(data) == expected, nil
}

func (s *StateStore) removeAllocationDirectory(path string, r *stateRecord, staging bool) error {
	if err := s.checkAllocationAncestors(path); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if alreadyGone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), path)
	defer directory.Close()
	var stat, root unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if err := unix.Stat(s.root, &root); err != nil {
		return err
	}
	if stat.Uid != root.Uid || stat.Mode&0077 != 0 {
		return fmt.Errorf("allocation directory ownership conflict")
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ".owner" && (!staging || !strings.HasPrefix(entry.Name(), ".ownership-")) {
			return fmt.Errorf("allocation directory contains foreign paths")
		}
		if _, _, err := s.markerPrefix(filepath.Join(path, entry.Name()), r); err != nil {
			return err
		}
	}
	_, complete, err := s.markerPrefix(filepath.Join(path, ".owner"), r)
	if err != nil {
		return err
	}
	if !staging {
		if !complete {
			return fmt.Errorf("final allocation directory lacks ownership")
		}
		return s.removeOwnedResourceDirectory(path, r)
	}
	for _, entry := range entries {
		if err := unix.Unlinkat(fd, entry.Name(), 0); err != nil && !alreadyGone(err) {
			return err
		}
	}
	parentFD, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, filepath.Base(path), &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if alreadyGone(err) {
			return nil
		}
		return err
	}
	if current.Dev != stat.Dev || current.Ino != stat.Ino {
		return fmt.Errorf("allocation directory changed during cleanup")
	}
	if err := unix.Unlinkat(parentFD, filepath.Base(path), unix.AT_REMOVEDIR); err != nil && !alreadyGone(err) {
		return err
	}
	return unix.Fsync(parentFD)
}

func (s *StateStore) cleanupAllocationStaging(r *stateRecord) error {
	stage, reservation := s.allocationPaths(r)
	present, complete, err := s.markerPrefix(reservation, r)
	if err != nil || !present {
		return err
	}
	if complete {
		if err := s.removeAllocationDirectory(stage, r, true); err != nil {
			return err
		}
	}
	// A partial reservation never authorizes staging or final directory removal.
	if err := os.Remove(reservation); err != nil && !alreadyGone(err) {
		return err
	}
	return s.syncDirectory()
}

func (s *StateStore) recoverPreallocation(r *stateRecord) (bool, error) {
	if err := s.cleanupAllocationStaging(r); err != nil {
		return false, err
	}
	if r.AllocationReady || r.VMMPID != 0 {
		return false, nil
	}
	for _, path := range []string{r.NetNSPath, r.NetNSPath + ".owner"} {
		if _, err := os.Lstat(path); err == nil {
			return false, nil
		} else if !alreadyGone(err) {
			return false, err
		}
	}
	final := filepath.Dir(r.APISocketPath)
	deleting, err := ownedMarker(deletionMarkerPath(final, r), r)
	if err != nil {
		return false, err
	}
	if deleting {
		return true, s.removeOwnedResourceDirectory(final, r)
	}
	owned, err := ownedMarker(filepath.Join(final, ".owner"), r)
	if err != nil || !owned {
		// No allocation preceded the durable barrier. A foreign final path is
		// not ours, even if it is root-owned and empty; retire only the intent.
		return true, nil
	}
	return true, s.removeAllocationDirectory(final, r, false)
}
