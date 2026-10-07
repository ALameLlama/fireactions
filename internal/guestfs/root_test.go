package guestfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkspacePaths(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot() error = %v", err)
	}
	defer root.Close()

	tests := []struct {
		name string
		want string
	}{
		{name: "/workspace", want: "."},
		{name: "/workspace/", want: "."},
		{name: "/workspace///nested/file", want: "nested/file"},
		{name: "nested/file", want: "nested/file"},
		{name: ".", want: "."},
	}
	for _, test := range tests {
		got, err := root.ResolvePath(test.name)
		if err != nil || got != test.want {
			t.Errorf("ResolvePath(%q) = %q, %v; want %q", test.name, got, err, test.want)
		}
	}
	for _, name := range []string{"", "\x00", "/etc/passwd", "/workspace-other/file", "../file", "a/../file", "/workspace/../etc/passwd"} {
		if _, err := root.ResolvePath(name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ResolvePath(%q) error = %v, want ErrInvalidPath", name, err)
		}
	}
}

func TestArchivePathAndSymlinkBoundaries(t *testing.T) {
	for _, name := range []string{"", "/absolute", "../escape", "a/../escape", "a/\x00b"} {
		if _, err := CleanArchiveName(name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanArchiveName(%q) error = %v, want ErrInvalidPath", name, err)
		}
	}
	if got, err := CleanArchiveName("./nested/file"); err != nil || got != "nested/file" {
		t.Fatalf("CleanArchiveName() = %q, %v", got, err)
	}
	for _, test := range []struct {
		entry  string
		target string
		bad    bool
	}{
		{entry: "nested/link", target: "../sibling"},
		{entry: "link", target: "child"},
		{entry: "link", target: "../escape", bad: true},
		{entry: "link", target: "/etc/passwd", bad: true},
		{entry: "link", target: "\x00", bad: true},
		{entry: "link", target: "missing/../child", bad: true},
	} {
		err := ValidateLinkTargets(map[string]string{test.entry: test.target}, func(name string) (fs.FileMode, string, error) {
			if name == "nested" {
				return fs.ModeDir, "", nil
			}
			return 0, "", os.ErrNotExist
		})
		if test.bad {
			if !errors.Is(err, ErrInvalidPath) {
				t.Errorf("ValidateLinkTargets(%q, %q) error = %v, want ErrInvalidPath", test.entry, test.target, err)
			}
		} else if err != nil {
			t.Errorf("ValidateLinkTargets(%q, %q) error = %v", test.entry, test.target, err)
		}
	}
}

func TestEnsureDirectoryRejectsEscapingSymlink(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(workspace)
	if err != nil {
		t.Fatalf("OpenRoot() error = %v", err)
	}
	defer root.Close()
	if err := root.EnsureDirectory("escape/created", uint32(os.Getuid()), uint32(os.Getgid()), 0755); err == nil {
		t.Fatal("EnsureDirectory() accepted an escaping symlink")
	}
	if _, err := os.Lstat(filepath.Join(outside, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaping symlink created an outside directory: %v", err)
	}
}

func TestOpenRootRejectsProcFilesystem(t *testing.T) {
	root, err := OpenRoot("/proc")
	if err == nil {
		_ = root.Close()
		t.Fatal("OpenRoot(/proc) unexpectedly accepted procfs")
	}
	if !errors.Is(err, ErrUnsafeFilesystem) {
		t.Fatalf("OpenRoot(/proc) error = %v, want ErrUnsafeFilesystem", err)
	}
}

func TestRootedLinkTargetsFollowComponentsBeforeParentTraversal(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(workspace, "pivot")); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, links := range []map[string]string{
		{"escape": "pivot/../outside"},
		{"a": "b", "b": "a"},
		{"escape": "missing/../outside"},
	} {
		if err := root.ValidateLinkTargets(links); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("ValidateLinkTargets(%v) = %v, want ErrInvalidPath", links, err)
		}
	}
	if err := root.ValidateLinkTargets(map[string]string{"nested/safe": "../missing", "dangling": "missing/child"}); err != nil {
		t.Fatalf("safe relative and dangling links rejected: %v", err)
	}
}
