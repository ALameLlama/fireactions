package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerLockExclusiveAndReleased(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := acquireOwnerLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireOwnerLock(dir); err == nil {
		second.Close()
		first.Close()
		t.Fatal("two daemons acquired the same state directory")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireOwnerLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("state permissions: %v", info.Mode())
	}
}

func TestOwnerLockRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := acquireOwnerLock(link); err == nil {
		lock.Close()
		t.Fatal("followed state directory symlink")
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("modified symlink target: %v %v", info, err)
	}
}
