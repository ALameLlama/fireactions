package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerLockExclusiveAndReleased(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("state ownership requires root")
	}
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

func TestOwnerLockRejectsSymlinkAncestorWithoutMutation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-state", true: "existing-state"}[existing], func(t *testing.T) {
			base := t.TempDir()
			target := filepath.Join(base, "target")
			if err := os.Mkdir(target, 0755); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(target, "state")
			if existing {
				if err := os.Mkdir(state, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "sentinel"), []byte("preserve"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(base, "alias")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if lock, err := acquireOwnerLock(filepath.Join(link, "state")); err == nil {
				lock.Close()
				t.Fatal("followed symlinked state ancestor")
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatalf("target mode changed: %v %v", info, err)
			}
			if !existing {
				if _, err := os.Lstat(state); !os.IsNotExist(err) {
					t.Fatalf("created state through rejected ancestor: %v", err)
				}
				return
			}
			info, err = os.Stat(state)
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatalf("state mode changed: %v %v", info, err)
			}
			entries, err := os.ReadDir(state)
			if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
				t.Fatalf("target files changed: %v %v", entries, err)
			}
			if data, err := os.ReadFile(filepath.Join(state, "sentinel")); err != nil || string(data) != "preserve" {
				t.Fatalf("target contents changed: %q %v", data, err)
			}
		})
	}
}
