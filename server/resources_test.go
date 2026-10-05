package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/containernetworking/cni/libcni"
	"github.com/firecracker-microvm/firecracker-go-sdk"

	"golang.org/x/sys/unix"
)

func TestEffectiveCNIConfigOverridesOnlyHostLocalResolver(t *testing.T) {
	resolver := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolver, []byte("nameserver 127.0.0.1\nnameserver 10.10.0.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"cniVersion":"1.0.0","name":"fireactions","plugins":[{"type":"bridge","bridge":"fa0","ipam":{"type":"host-local","subnet":"10.88.0.0/16","rangeStart":"10.88.0.10","resolvConf":"old"}},{"type":"firewall","backend":"iptables"}]}`)
	effective, err := effectiveCNIConfig(input, resolver)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(effective, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(input, &want); err != nil {
		t.Fatal(err)
	}
	want["plugins"].([]any)[0].(map[string]any)["ipam"].(map[string]any)["resolvConf"] = resolver
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effective CNI policy changed beyond resolver path: %#v", got)
	}
	if _, err := libcni.ConfListFromBytes(effective); err != nil {
		t.Fatalf("effective CNI list is invalid: %v", err)
	}
	if err := os.WriteFile(resolver, []byte("nameserver 127.0.0.1\nnameserver ::1\nnameserver 9.9.9.9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := effectiveCNIConfig(input, resolver); err == nil {
		t.Fatal("loopback-only resolver was accepted")
	}
}
func TestOwnedResourcesRetriesOnlyUnfinishedSteps(t *testing.T) {
	for failure := range 5 {
		t.Run(strconv.Itoa(failure), func(t *testing.T) {
			var order []int
			failed := false
			r := &ownedResources{}
			for i := range 5 {
				r.steps = append(r.steps, cleanupStep{name: fmt.Sprint(i), run: func(ctx context.Context) error {
					if ctx.Err() != nil {
						t.Error("cleanup inherited cancellation")
					}
					order = append(order, i)
					if i == failure && !failed {
						failed = true
						return errors.New("transient cleanup failure")
					}
					return nil
				}})
			}
			if err := r.destroy(); err == nil {
				t.Fatal("failed destruction was reported complete")
			}
			var expected []int
			for i := range failure + 1 {
				expected = append(expected, i)
			}
			if !reflect.DeepEqual(order, expected) {
				t.Fatalf("cleanup crossed a failed prerequisite: %v", order)
			}
			if err := r.destroy(); err != nil {
				t.Fatal(err)
			}
			for i := failure; i < 5; i++ {
				expected = append(expected, i)
			}
			if !reflect.DeepEqual(order, expected) {
				t.Fatalf("retry repeated completed work: %v", order)
			}
			if err := r.destroy(); err != nil || !reflect.DeepEqual(order, expected) {
				t.Fatalf("completed destruction was not idempotent: %v, %v", err, order)
			}
		})
	}
}

func TestConcurrentDestructionHasOneOwner(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	r := &ownedResources{steps: []cleanupStep{{name: "owned removal", run: func(context.Context) error {
		calls.Add(1)
		close(entered)
		<-release
		return nil
	}}}}
	results := make(chan error, 24)
	go func() { results <- r.destroy() }()
	<-entered
	var started sync.WaitGroup
	started.Add(23)
	for range 23 {
		go func() {
			started.Done()
			results <- r.destroy()
		}()
	}
	started.Wait()
	close(release)
	for range 24 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("resource teardown ran %d times", calls.Load())
	}
}

func TestDestroyedFilesWaitForSuccessfulStop(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	if err := os.WriteFile(owned, []byte("live disk"), 0600); err != nil {
		t.Fatal(err)
	}
	canStop := false
	r := &ownedResources{steps: []cleanupStep{
		{name: "stop", run: func(context.Context) error {
			if !canStop {
				return errors.New("live VMM could not be stopped")
			}
			return nil
		}},
		{name: "owned path", run: func(context.Context) error { return os.Remove(owned) }},
	}}
	if err := r.destroy(); err == nil {
		t.Fatal("live VMM teardown unexpectedly succeeded")
	}
	if _, err := os.Stat(owned); err != nil {
		t.Fatalf("live VM's owned path was lost: %v", err)
	}
	canStop = true
	if err := r.destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful cleanup retained owned path: %v", err)
	}
}

func TestVMMProcessHelper(t *testing.T) {
	mode := os.Getenv("FIREACTIONS_VMM_PROCESS_HELPER")
	if mode == "" {
		return
	}
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	fmt.Println("ready")
	for {
		time.Sleep(time.Hour)
	}
}

func startOwnedProcess(t *testing.T, ignoreTERM bool) (*exec.Cmd, processIdentity) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "api.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestVMMProcessHelper$", "--", "--api-sock", socket)
	mode := "normal"
	if ignoreTERM {
		mode = "ignore-term"
	}
	cmd.Env = append(os.Environ(), "FIREACTIONS_VMM_PROCESS_HELPER="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("unexpected process readiness message %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process helper did not become ready")
	}
	start, err := processStartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	return cmd, processIdentity{pid: cmd.Process.Pid, startTime: start, binary: binary, apiSocket: socket}
}

func TestIdentityCheckedStopAndAlreadyGone(t *testing.T) {
	cmd, identity := startOwnedProcess(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := identity.stop(ctx); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := identity.stop(ctx); err != nil && !alreadyGone(err) {
		t.Fatalf("already exited process is not idempotent: %v", err)
	}
}

func TestIdentityCheckedStopEscalatesAfterGrace(t *testing.T) {
	cmd, identity := startOwnedProcess(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	if err := identity.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 1900*time.Millisecond {
		t.Fatal("force-stop did not allow SIGTERM grace")
	}
	_ = cmd.Wait()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || status.Signal() != syscall.SIGKILL {
		t.Fatalf("stubborn VMM was not force-killed: %v", cmd.ProcessState)
	}
}

func TestIdentityMismatchDoesNotSignalUnrelatedProcess(t *testing.T) {
	for _, evidence := range []string{"start-time", "binary", "socket"} {
		t.Run(evidence, func(t *testing.T) {
			cmd, identity := startOwnedProcess(t, false)
			switch evidence {
			case "start-time":
				identity.startTime = "0"
			case "binary":
				identity.binary += ".unrelated"
			case "socket":
				identity.apiSocket += ".unrelated"
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := identity.stop(ctx); err == nil {
				t.Fatal("conflicting ownership evidence was accepted")
			}
			fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			exited, err := pidfdExited(fd, 100)
			if err != nil || exited {
				t.Fatalf("unrelated live process was signalled: exited=%v, err=%v", exited, err)
			}
		})
	}
}

func TestOwnedNetworkAddObservesCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "blocked"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	conf, err := libcni.ConfListFromBytes([]byte(`{"cniVersion":"1.0.0","name":"fireactions","plugins":[{"type":"blocked"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := &ownedResources{netnsPath: filepath.Join(root, "owned-netns")}
	machine := &firecracker.Machine{Cfg: firecracker.Config{
		VMID: "owned-vm",
		NetworkInterfaces: []firecracker.NetworkInterface{{CNIConfiguration: &firecracker.CNIConfiguration{
			NetworkConfig: conf, BinPath: []string{root}, CacheDir: filepath.Join(root, "cache"), IfName: "eth0",
		}}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := setupOwnedNetwork(ctx, machine, r); err == nil {
		t.Fatal("cancelled network allocation was reported successful")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("CNI subprocess ignored allocation cancellation")
	}
}

func TestNetworkNamespaceConflictDoesNotAdoptUnownedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(path, []byte("unrelated resource"), 0600); err != nil {
		t.Fatal(err)
	}
	owned, err := createOwnedNetNS(path)
	if err == nil || owned {
		t.Fatalf("pre-existing namespace path was adopted: owned=%v, err=%v", owned, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "unrelated resource" {
		t.Fatalf("unowned namespace path was changed: %q, %v", data, err)
	}
	if err := removeOwnedNetNS(filepath.Join(t.TempDir(), "already-gone")); err != nil {
		t.Fatalf("already absent namespace is not idempotent: %v", err)
	}
}
