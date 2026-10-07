package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"golang.org/x/sys/unix"
)

func TestResolveExecutableUsesProvidedPath(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tool")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveExecutable("tool", dir, "/", func(string) error { return nil })
	if err != nil || got != binary {
		t.Fatalf("resolveExecutable() = %q, %v", got, err)
	}
	if _, err = resolveExecutable("tool", "/missing", "/", func(string) error { return nil }); err == nil {
		t.Fatal("missing PATH executable succeeded")
	}
}

func TestProcessGroupCancellationWithoutCgroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap 'wait; exit 0' TERM; sleep 30 & echo ready; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ready := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() && scanner.Text() == "ready" {
			ready <- struct{}{}
		}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		_ = signalProcessGroup(cmd.Process.Pid, unix.SIGKILL)
		t.Fatal("process group did not report descendant readiness")
	}
	if err := signalProcessGroup(cmd.Process.Pid, unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("process group did not terminate cleanly: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = signalProcessGroup(cmd.Process.Pid, unix.SIGKILL)
		t.Fatal("process group descendant survived termination")
	}
}

// Small reads make the bounded queue fill deterministically, independent of
// the host's pipe capacity or scheduling of the burst writer.
type smallProcessPipe struct{ *processPipe }

func (p smallProcessPipe) Read(buf []byte) (int, error) {
	return p.processPipe.Read(buf[:min(len(buf), 4096)])
}

func TestProcessPipeDrainsBurstAfterConsumerPause(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe := &processPipe{File: r}
	output := make(chan processOutput, processQueueDepth)
	done := make(chan struct{}, 1)
	go readProcessPipe(ctx, smallProcessPipe{pipe}, agentv1.ExecStream_STDOUT, output, done)
	want := bytes.Repeat([]byte("burst\n"), ((processQueueDepth+2)*4096)/6+1)
	// Keep the burst within the queue, one blocked reader and one PIPE_BUF.
	want = want[:(processQueueDepth+2)*4096]
	written := make(chan error, 1)
	go func() {
		_, err := w.Write(want)
		_ = w.Close()
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("burst writer failed to exit while output was queued")
	}
	pipe.beginDrain()
	time.Sleep(processPipeDrain + 250*time.Millisecond)
	var got bytes.Buffer
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case item := <-output:
			if item.err != nil {
				t.Fatal(item.err)
			}
			got.Write(item.data)
		case <-done:
			for len(output) > 0 {
				item := <-output
				if item.err != nil {
					t.Fatal(item.err)
				}
				got.Write(item.data)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("paused output consumer received %d bytes, want %d", got.Len(), len(want))
			}
			return
		case <-deadline.C:
			t.Fatal("draining burst output stalled after consumer resumed")
		}
	}
}

func TestProcessPipeCompletesWithLiveInheritedWriter(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := exec.Command("sh", "-c", "sleep 30 & exit 0")
	cmd.Stdout = w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer signalProcessGroup(cmd.Process.Pid, unix.SIGKILL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe := &processPipe{File: r}
	output := make(chan processOutput, processQueueDepth)
	done := make(chan struct{}, 1)
	go readProcessPipe(ctx, pipe, agentv1.ExecStream_STDOUT, output, done)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	pipe.beginDrain()
	select {
	case <-done:
	case <-time.After(processPipeDrain + time.Second):
		t.Fatal("live inherited writer prevented output completion")
	}
	for len(output) > 0 {
		if item := <-output; item.err != nil {
			t.Fatalf("idle drain reported a read failure: %v", item.err)
		}
	}
	if err := signalProcessGroup(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("successful output drain terminated the background descendant: %v", err)
	}
}

func TestProcessPipeCancellationWithQueuedOutput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe := &processPipe{File: r}
	output := make(chan processOutput, 1)
	done := make(chan struct{}, 1)
	go readProcessPipe(ctx, pipe, agentv1.ExecStream_STDOUT, output, done)
	if _, err := w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(output) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("reader did not queue output")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := w.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = r.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("backpressured reader did not honor cancellation")
	}
}

func TestResolveExecutableTargetIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root with permission to launch UID/GID 65534 and supplementary groups")
	}
	credential := &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{65533}}
	probe := exec.Command("true")
	probe.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	if err := probe.Run(); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skip("requires mapped target UID/GIDs and CAP_SETUID/CAP_SETGID")
		}
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		setup     func(*testing.T, string, string)
		wantFirst bool
	}{
		{"owner-only executable", func(t *testing.T, dir, file string) {
			if err := os.Chmod(file, 0700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"inaccessible parent", func(t *testing.T, dir, file string) {
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"supplementary group", func(t *testing.T, dir, file string) {
			for _, path := range []string{dir, file} {
				if err := os.Chown(path, 0, 65533); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0050); err != nil {
					t.Fatal(err)
				}
			}
		}, true},
		{"ACL denies named user", func(t *testing.T, dir, file string) {
			// Linux POSIX ACL xattr: version, then tag/permissions/id entries.
			acl := make([]byte, 4+5*8)
			binary.LittleEndian.PutUint32(acl, 2)
			for i, entry := range []struct {
				tag, perm uint16
				id        uint32
			}{
				{1, 7, ^uint32(0)}, {2, 0, 65534}, {4, 5, ^uint32(0)},
				{16, 5, ^uint32(0)}, {32, 5, ^uint32(0)},
			} {
				at := 4 + i*8
				binary.LittleEndian.PutUint16(acl[at:], entry.tag)
				binary.LittleEndian.PutUint16(acl[at+2:], entry.perm)
				binary.LittleEndian.PutUint32(acl[at+4:], entry.id)
			}
			if err := unix.Setxattr(file, "system.posix_acl_access", acl, 0); err != nil {
				if errors.Is(err, unix.EOPNOTSUPP) {
					t.Skip("test filesystem does not support POSIX ACLs")
				}
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := os.MkdirTemp("", "fireactions-exec-permission-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			if err := os.Chmod(root, 0755); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{"first", "second"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, dir, "tool"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			first := filepath.Join(root, "first", "tool")
			tc.setup(t, filepath.Dir(first), first)
			var cmd *exec.Cmd
			got, err := resolveExecutable("tool", "first:second", root, func(path string) error {
				cmd = exec.Command(path)
				cmd.Dir = root
				cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
				return cmd.Start()
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(root, "second", "tool")
			if tc.wantFirst {
				want = first
			}
			if got != want {
				t.Fatalf("target-identity PATH resolution = %q, want %q", got, want)
			}
		})
	}
}

func TestResolveExecutableDoesNotFallbackOnInvalidImage(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "first", "tool"), []byte("not an executable image\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "second", "tool"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := resolveExecutable("tool", "first:second", root, func(path string) error {
		return exec.Command(path).Run()
	})
	if !errors.Is(err, unix.ENOEXEC) {
		t.Fatalf("invalid image = %v, want ENOEXEC without PATH fallback or shell interpretation", err)
	}
}

func TestResolveExecutableSkipsInaccessibleOwnedCandidate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may execute an owned file with only the other execute bit")
	}
	root := t.TempDir()
	for _, dir := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "tool"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// The candidate has an execute bit, but its owner cannot execute it.
	// Kernel permission checks must reject it instead of selecting it solely
	// from stat.Mode & 0111 and failing the entire PATH lookup.
	if err := os.Chmod(filepath.Join(root, "first", "tool"), 0001); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	got, err := resolveExecutable("tool", "first:second", root, func(path string) error {
		cmd = exec.Command(path)
		cmd.Dir = root
		return cmd.Start()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "second", "tool"); got != want {
		t.Fatalf("owned candidate PATH resolution = %q, want %q", got, want)
	}
}

func TestResolveExecutableRootExecuteRules(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise execute permission overrides")
	}
	root := t.TempDir()
	for _, dir := range []string{"first", "second"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "tool"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	first := filepath.Join(root, "first", "tool")
	for _, tc := range []struct {
		mode      os.FileMode
		wantFirst bool
	}{{0010, true}, {0644, false}} {
		if err := os.Chmod(first, tc.mode); err != nil {
			t.Fatal(err)
		}
		got, err := resolveExecutable("tool", "first:second", root, func(path string) error {
			return exec.Command(path).Run()
		})
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, "second", "tool")
		if tc.wantFirst {
			want = first
		}
		if got != want {
			t.Fatalf("root PATH resolution with mode %o = %q, want %q", tc.mode, got, want)
		}
	}
}
