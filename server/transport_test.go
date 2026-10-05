package server

import (
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestUnixTransportRejectsPathConflict(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise protected socket setup")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(current.Gid)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSocket(path, group.Name); err == nil {
		t.Fatal("expected a non-socket path conflict")
	}
}

func TestUnixTransportProtectsSocketAndRejectsLiveConflict(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for socket ownership and mode checks")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(current.Gid)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugin.sock")
	listener, err := listenUnixSocket(path, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0660 {
		t.Fatalf("socket mode = %o, want 0660", info.Mode().Perm())
	}
	socketStat := info.Sys().(*syscall.Stat_t)
	if socketStat.Uid != 0 || int(socketStat.Gid) != gid {
		t.Fatalf("socket owner = %d:%d, want 0:%d", socketStat.Uid, socketStat.Gid, gid)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	dirStat := dirInfo.Sys().(*syscall.Stat_t)
	if dirInfo.Mode().Perm() != 0750 || dirStat.Uid != 0 || int(dirStat.Gid) != gid {
		t.Fatalf("socket directory owner/mode = %d:%d %o", dirStat.Uid, dirStat.Gid, dirInfo.Mode().Perm())
	}
	if _, err := listenUnixSocket(path, group.Name); err == nil {
		t.Fatal("expected a live socket conflict")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remains after close: %v", err)
	}
}
