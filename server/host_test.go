package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckHostHonorsCanceledContextBeforeHostAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckHost(ctx, DefaultConfig()); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckHost error = %v, want context.Canceled", err)
	}
}

func TestCheckKernelImage(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(regular, []byte("kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	regularLink := filepath.Join(dir, "regular-link")
	if err := os.Symlink(regular, regularLink); err != nil {
		t.Fatal(err)
	}
	directoryLink := filepath.Join(dir, "directory-link")
	if err := os.Symlink(dir, directoryLink); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	brokenLink := filepath.Join(dir, "broken-link")
	if err := os.Symlink(missing, brokenLink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		wantErr string
		missing bool
	}{
		{"readable regular file", regular, "", false},
		{"symlink to regular file", regularLink, "", false},
		{"directory", dir, "not a regular file", false},
		{"symlink to directory", directoryLink, "not a regular file", false},
		{"FIFO without a writer", fifo, "not a regular file", false},
		{"missing file", missing, "not readable", true},
		{"broken symlink", brokenLink, "not readable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKernelImage(tc.path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkKernelImage(%q): %v", tc.path, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkKernelImage(%q) error = %v, want %q", tc.path, err, tc.wantErr)
			}
			if tc.missing && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("checkKernelImage(%q) error = %v, want os.ErrNotExist", tc.path, err)
			}
		})
	}
}
