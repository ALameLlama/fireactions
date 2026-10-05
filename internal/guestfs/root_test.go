package guestfs

import (
	"errors"
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
		want   string
		bad    bool
	}{
		{entry: "nested/link", target: "../sibling", want: "sibling"},
		{entry: "link", target: "child", want: "child"},
		{entry: "link", target: "../escape", bad: true},
		{entry: "link", target: "/etc/passwd", bad: true},
		{entry: "link", target: "\x00", bad: true},
	} {
		got, err := CleanLinkTarget(test.entry, test.target)
		if test.bad {
			if !errors.Is(err, ErrInvalidPath) {
				t.Errorf("CleanLinkTarget(%q, %q) error = %v, want ErrInvalidPath", test.entry, test.target, err)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("CleanLinkTarget(%q, %q) = %q, %v; want %q", test.entry, test.target, got, err, test.want)
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
