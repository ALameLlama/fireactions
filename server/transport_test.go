package server

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestUnixTransportRejectsUntrustedAncestorsBeforePublishing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise protected socket setup")
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		uid   int
		mode  os.FileMode
		alias string
	}{
		{name: "user-owned", uid: 65534, mode: 0755},
		{name: "user-owned-sticky", uid: 65534, mode: os.ModeSticky | 0777},
		{name: "root-owned-writable", mode: 0777},
		{name: "alias-to-user-owned", uid: 65534, mode: 0755, alias: "target"},
		{name: "alias-under-user-owned", uid: 65534, mode: 0755, alias: "parent"},
		{name: "alias-target-dotdot", uid: 65534, mode: 0755, alias: "dotdot"},
		{name: "user-owned-alias", mode: 0755, alias: "owner"},
		{name: "missing-descendants", uid: 65534, mode: 0755, alias: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := socketTestDir(t)
			unsafe := filepath.Join(root, "unsafe")
			safe := filepath.Join(root, "safe")
			for _, dir := range []string{unsafe, safe} {
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "run"), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(dir, "run"), 0750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chown(unsafe, test.uid, os.Getgid()); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(unsafe, test.mode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(unsafe, "run", "plugin.sock")
			switch test.alias {
			case "target", "dotdot":
				target := unsafe
				if test.alias == "dotdot" {
					target = "unsafe/../safe"
				}
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(alias, "run", "plugin.sock")
			case "parent":
				alias := filepath.Join(unsafe, "alias")
				if err := os.Symlink(safe, alias); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(alias, "run", "plugin.sock")
			case "owner":
				if err := os.Chmod(unsafe, os.ModeSticky|0777); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(unsafe, "alias")
				if err := os.Symlink(safe, alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Lchown(alias, 65534, os.Getgid()); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(alias, "run", "plugin.sock")
			case "missing":
				path = filepath.Join(unsafe, "new", "run", "plugin.sock")
			}
			dirs := []string{unsafe, safe, filepath.Join(unsafe, "run"), filepath.Join(safe, "run")}
			before := make([]os.FileInfo, len(dirs))
			for i, dir := range dirs {
				before[i], err = os.Stat(dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			listener, err := listenUnixSocket(path, group.Name)
			if listener != nil {
				_ = listener.Close()
				t.Fatal("published a listener through an untrusted ancestor")
			}
			if err == nil {
				t.Fatal("expected rejection of an untrusted ancestor")
			}
			for _, dir := range []string{filepath.Join(unsafe, "run"), filepath.Join(safe, "run")} {
				if _, err := os.Lstat(filepath.Join(dir, "plugin.sock")); !os.IsNotExist(err) {
					t.Fatalf("socket path was published in %s: %v", dir, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(unsafe, "new")); !os.IsNotExist(err) {
				t.Fatalf("created a directory beneath an untrusted ancestor: %v", err)
			}
			for i, dir := range dirs {
				assertSocketDirectoryUnchanged(t, dir, before[i])
			}
		})
	}
}

func TestUnixTransportAcceptsTrustedAncestorAlias(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise protected socket setup")
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		target   string
		absolute bool
	}{
		{"relative", "real", false},
		{"absolute", "real", true},
		{"long relative", strings.Repeat("r", 108), false},
		{"long absolute", strings.Repeat("r", 108), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := socketTestDir(t)
			real := filepath.Join(root, tc.target)
			if err := os.Mkdir(real, 0755); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(real)
			if err != nil {
				t.Fatal(err)
			}
			target := tc.target
			if tc.absolute {
				target = real
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(alias, "run", "plugin.sock")
			canonicalPath := filepath.Join(real, "run", "plugin.sock")
			if len(path) > 107 {
				t.Fatalf("alias socket path has %d bytes, want at most 107", len(path))
			}
			if len(tc.target) > 107 && len(canonicalPath) <= 107 {
				t.Fatalf("canonical socket path has %d bytes, want over 107", len(canonicalPath))
			}
			listener, err := listenUnixSocket(path, group.Name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			conn, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			if _, err := listenUnixSocket(path, group.Name); err == nil {
				t.Fatal("expected a live socket conflict through the alias")
			}
			// Closing must still remove the canonical socket if the alias moves.
			if err := os.Rename(alias, filepath.Join(root, "moved-alias")); err != nil {
				t.Fatal(err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(canonicalPath); !os.IsNotExist(err) {
				t.Fatalf("canonical socket remains after close: %v", err)
			}
			assertSocketDirectoryUnchanged(t, real, before)
		})
	}
}

func TestUnixTransportConfiguredGroupConnectsThroughNewAncestors(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise configured-group access")
	}
	const gid = 65534
	group, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		t.Fatal(err)
	}
	root := socketTestDir(t)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "nested", "run", "plugin.sock")
	listener, err := listenUnixSocket(path, group.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	assertSocketDirectoryUnchanged(t, root, before)
	for _, dir := range []string{filepath.Join(root, "nested"), filepath.Dir(path)} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != 0 || stat.Gid != gid || info.Mode().Perm() != 0750 {
			t.Fatalf("new directory %s has owner/mode %d:%d %o, want 0:%d 0750",
				dir, stat.Uid, stat.Gid, info.Mode().Perm(), gid)
		}
	}
	// A go-build directory can prevent a non-root child from executing the
	// original test binary. Copy it into the traversable, root-owned fixture.
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	binary := filepath.Join(root, "client")
	destination, err := os.OpenFile(binary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := os.Chmod(binary, 0755); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, err = conn.Write([]byte("ok"))
			_ = conn.Close()
		}
		accepted <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestUnixTransportGroupClientHelper$")
	cmd.Env = append(os.Environ(), "FIREACTIONS_SOCKET_CLIENT="+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: gid}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configured-group client failed: %v\n%s", err, output)
	}
	if err := <-accepted; err != nil {
		t.Fatalf("listener failed to serve the configured-group client: %v", err)
	}
}

func TestUnixTransportGroupClientHelper(t *testing.T) {
	path := os.Getenv("FIREACTIONS_SOCKET_CLIENT")
	if path == "" {
		return
	}
	if os.Geteuid() != 65534 || os.Getegid() != 65534 {
		t.Fatalf("client credentials = %d:%d, want 65534:65534", os.Geteuid(), os.Getegid())
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if string(reply[:]) != "ok" {
		t.Fatalf("listener reply = %q, want ok", reply)
	}
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
