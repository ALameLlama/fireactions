package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func listenUnixSocket(path, group string) (net.Listener, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("server must run as root to protect its Unix socket")
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return nil, fmt.Errorf("lookup socket group %q: %w", group, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return nil, fmt.Errorf("invalid socket group ID %q: %w", g.Gid, err)
	}
	if err := prepareUnixSocketDirectory(path, gid); err != nil {
		return nil, err
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("socket path exists and is not a Unix socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("Unix socket is already active")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
			return nil, fmt.Errorf("cannot determine whether existing socket is stale: %w", dialErr)
		}
		if err := removeOwnedUnixSocket(path, info); err != nil {
			return nil, fmt.Errorf("remove stale Unix socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Unix socket path: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("inspect new Unix socket: %w", err)
	}
	owned := &ownedUnixListener{Listener: listener, path: path, info: info}
	if err := os.Chown(path, 0, gid); err != nil {
		_ = owned.Close()
		return nil, fmt.Errorf("set socket ownership: %w", err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		_ = owned.Close()
		return nil, fmt.Errorf("set socket mode: %w", err)
	}
	return owned, nil
}

func sharedSocketDirectory(dir string) bool {
	switch dir {
	case "/", "/run", "/var/run", "/tmp", "/var/tmp":
		return true
	default:
		return false
	}
}

func prepareUnixSocketDirectory(path string, gid int) error {
	dir := filepath.Dir(path)
	if sharedSocketDirectory(dir) {
		return fmt.Errorf("socket_path must use a dedicated directory, not %q", dir)
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0750); err != nil {
			return fmt.Errorf("create socket directory parent: %w", err)
		}
		if err := os.Mkdir(dir, 0750); err == nil {
			directory, err := os.Open(dir)
			if err != nil {
				return fmt.Errorf("open socket directory: %w", err)
			}
			defer directory.Close()
			if err := directory.Chown(0, gid); err != nil {
				return fmt.Errorf("set socket directory ownership: %w", err)
			}
			if err := directory.Chmod(0750); err != nil {
				return fmt.Errorf("set socket directory mode: %w", err)
			}
		} else if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create socket directory: %w", err)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("inspect socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("socket directory must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve socket directory: %w", err)
	}
	if sharedSocketDirectory(resolved) {
		return fmt.Errorf("socket_path must use a dedicated directory, not %q", resolved)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || int(stat.Gid) != gid || info.Mode().Perm() != 0750 ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("existing socket directory must be owned by root:%d with mode 0750", gid)
	}
	return nil
}

func removeOwnedUnixSocket(path string, owned os.FileInfo) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || !os.SameFile(owned, info) {
		return nil
	}
	return os.Remove(path)
}

type ownedUnixListener struct {
	net.Listener
	path      string
	info      os.FileInfo
	closeOnce sync.Once
	closeErr  error
}

func (l *ownedUnixListener) Close() error {
	l.closeOnce.Do(func() {
		l.closeErr = l.Listener.Close()
		if err := removeOwnedUnixSocket(l.path, l.info); err != nil && l.closeErr == nil {
			l.closeErr = err
		}
	})
	return l.closeErr
}
