package agent

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestResolveExecutableUsesProvidedPath(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tool")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveExecutable("tool", dir, "/")
	if err != nil || got != binary {
		t.Fatalf("resolveExecutable() = %q, %v", got, err)
	}
	if _, err = resolveExecutable("tool", "/missing", "/"); err == nil {
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
