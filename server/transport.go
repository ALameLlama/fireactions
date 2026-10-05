package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("socket directory must be a real directory")
	}
	if err := os.Chown(dir, 0, gid); err != nil {
		return nil, fmt.Errorf("set socket directory ownership: %w", err)
	}
	if err := os.Chmod(dir, 0750); err != nil {
		return nil, fmt.Errorf("set socket directory mode: %w", err)
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
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale Unix socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Unix socket path: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	if err := os.Chown(path, 0, gid); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("set socket ownership: %w", err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("set socket mode: %w", err)
	}
	return &ownedUnixListener{Listener: listener, path: path}, nil
}

type ownedUnixListener struct {
	net.Listener
	path string
}

func (l *ownedUnixListener) Close() error {
	err := l.Listener.Close()
	if removeErr := os.Remove(l.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
		err = removeErr
	}
	return err
}
