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
	dir := filepath.Join(socketTestDir(t), "run")
	if err := os.Mkdir(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSocket(path, group.Name); err == nil {
		t.Fatal("expected a non-socket path conflict")
	}
	assertSocketDirectoryUnchanged(t, dir, before)
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
	dir := filepath.Join(socketTestDir(t), "run")
	// Let Fireactions create the dedicated directory.
	path := filepath.Join(dir, "plugin.sock")
	listener, err := listenUnixSocket(path, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
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
	before := dirInfo
	if _, err := listenUnixSocket(path, group.Name); err == nil {
		t.Fatal("expected a live socket conflict")
	}
	assertSocketDirectoryUnchanged(t, dir, before)
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

func assertSocketDirectoryUnchanged(t *testing.T, dir string, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldStat := before.Sys().(*syscall.Stat_t)
	newStat := after.Sys().(*syscall.Stat_t)
	if after.Mode() != before.Mode() || newStat.Uid != oldStat.Uid || newStat.Gid != oldStat.Gid {
		t.Fatalf("directory changed from %d:%d %o to %d:%d %o",
			oldStat.Uid, oldStat.Gid, before.Mode(), newStat.Uid, newStat.Gid, after.Mode())
	}
}

func TestUnixTransportRejectsSharedDirectoryWithoutChangingIt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise protected socket setup")
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, conflict := range []string{"none", "file", "live socket"} {
		t.Run(conflict, func(t *testing.T) {
			dir := filepath.Join(socketTestDir(t), "shared")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "plugin.sock")
			switch conflict {
			case "file":
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "live socket":
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			}
			before, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := listenUnixSocket(path, group.Name); err == nil {
				t.Fatal("expected rejection of an existing shared directory")
			}
			assertSocketDirectoryUnchanged(t, dir, before)
			if conflict == "live socket" {
				conn, err := net.Dial("unix", path)
				if err != nil {
					t.Fatalf("conflict rejection damaged active socket: %v", err)
				}
				_ = conn.Close()
			}
		})
	}
}

func TestUnixTransportRejectsSharedDirectoryAlias(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise protected socket setup")
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat("/run")
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(socketTestDir(t), "root")
	if err := os.Symlink("/", alias); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSocket(filepath.Join(alias, "run", "plugin.sock"), group.Name); err == nil {
		t.Fatal("expected rejection of an alias for a shared socket directory")
	}
	assertSocketDirectoryUnchanged(t, "/run", before)
}

func TestUnixTransportAcceptsProtectedExistingDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise protected socket setup")
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(socketTestDir(t), "dedicated")
	path := filepath.Join(dir, "plugin.sock")
	listener, err := listenUnixSocket(path, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err = listenUnixSocket(path, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	assertSocketDirectoryUnchanged(t, dir, before)
}

func TestOwnedUnixListenerClosePreservesReplacementSocket(t *testing.T) {
	for _, replaceBeforeClose := range []bool{false, true} {
		name := "replace-after-close"
		if replaceBeforeClose {
			name = "replace-before-close"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketTestDir(t), "plugin.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			listener.(*net.UnixListener).SetUnlinkOnClose(false)
			info, err := os.Lstat(path)
			if err != nil {
				_ = listener.Close()
				t.Fatal(err)
			}
			owned := &ownedUnixListener{Listener: listener, path: path, info: info}
			t.Cleanup(func() { _ = owned.Close() })
			if replaceBeforeClose {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := owned.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("owned socket remained after close: %v", err)
				}
			}
			replacement, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = replacement.Close() })
			replacementInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := owned.Close(); err != nil {
					t.Fatal(err)
				}
				current, err := os.Lstat(path)
				if err != nil || !os.SameFile(replacementInfo, current) {
					t.Fatalf("close removed or replaced the replacement socket: %v", err)
				}
				conn, err := net.Dial("unix", path)
				if err != nil {
					t.Fatalf("replacement socket stopped accepting connections: %v", err)
				}
				_ = conn.Close()
			}
		})
	}
}

func socketTestDir(t *testing.T) string {
	t.Helper()
	// Keep Unix socket paths short regardless of the test or subtest name.
	dir, err := os.MkdirTemp("", "fa-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
