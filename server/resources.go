package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containernetworking/cni/libcni"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/cni/vmconf"
	"golang.org/x/sys/unix"
)

const resourceCleanupTimeout = 30 * time.Second

// cleanupStep retains successful work across retry attempts. Later steps must not
// run after a failed prerequisite (in particular, never detach a live VM's disk).
type cleanupStep struct {
	name string
	run  func(context.Context) error
	done bool
}

type cleanupAttempt struct {
	done chan struct{}
	err  error
}

// ownedResources serializes exit-monitor, explicit removal and pool-stop cleanup.
// Failed attempts preserve their unfinished steps; a later caller retries them.
type ownedResources struct {
	mu          sync.Mutex
	active      *cleanupAttempt
	complete    bool
	steps       []cleanupStep
	cniConfig   []byte
	cniBinPaths []string
	cniCacheDir string
	netnsPath   string
	snapshotID  string
	leaseID     string
}

func (r *ownedResources) destroy() error {
	r.mu.Lock()
	if r.complete {
		r.mu.Unlock()
		return nil
	}
	if a := r.active; a != nil {
		r.mu.Unlock()
		<-a.done
		return a.err
	}
	a := &cleanupAttempt{done: make(chan struct{})}
	r.active = a
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), resourceCleanupTimeout)
	defer cancel()
	for i := range r.steps {
		step := &r.steps[i]
		if step.done {
			continue
		}
		if err := ctx.Err(); err != nil {
			a.err = err
			break
		}
		if err := step.run(ctx); err != nil && !alreadyGone(err) {
			a.err = fmt.Errorf("%s: %w", step.name, err)
			break
		}
		step.done = true
	}

	r.mu.Lock()
	r.complete = a.err == nil
	r.active = nil
	close(a.done)
	r.mu.Unlock()
	return a.err
}

func alreadyGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) || errdefs.IsNotFound(err)
}

// Replace the SDK's SetupNetwork handler: its automatic CNI cleanup captures
// startup cancellation and runs before the owning host can stop a failed VMM.
// Keeping ADD/DEL under the same resource owner gives teardown one ordered,
// retryable path, while reusing the SDK's VM-specific CNI result translation.
func setupOwnedNetwork(ctx context.Context, machine *firecracker.Machine, r *ownedResources) error {
	network := &machine.Cfg.NetworkInterfaces[0]
	config := network.CNIConfiguration
	plugin := libcni.NewCNIConfigWithCacheDir(config.BinPath, config.CacheDir, nil)
	result, err := plugin.AddNetworkList(ctx, config.NetworkConfig, &libcni.RuntimeConf{
		ContainerID: machine.Cfg.VMID,
		NetNS:       r.netnsPath,
		IfName:      config.IfName,
		Args:        config.Args,
	})
	if err != nil {
		return fmt.Errorf("adding CNI network: %w", err)
	}
	static, err := vmconf.StaticNetworkConfFrom(result, machine.Cfg.VMID)
	if err != nil {
		return fmt.Errorf("parsing VM network configuration: %w", err)
	}
	network.StaticConfiguration = &firecracker.StaticNetworkConfiguration{
		HostDevName: static.TapName,
		MacAddress:  static.VMMacAddr,
	}
	if static.VMIPConfig != nil {
		nameservers := static.VMNameservers
		if len(nameservers) > 2 {
			nameservers = nameservers[:2]
		}
		network.StaticConfiguration.IPConfiguration = &firecracker.IPConfiguration{
			IPAddr:      static.VMIPConfig.Address,
			Gateway:     static.VMIPConfig.Gateway,
			Nameservers: nameservers,
			IfName:      config.VMIfName,
		}
	}
	return nil
}

func deleteOwnedNetwork(ctx context.Context, r *ownedResources, vmID string) error {
	conf, err := libcni.ConfListFromBytes(r.cniConfig)
	if err != nil {
		return fmt.Errorf("parsing saved CNI configuration: %w", err)
	}
	plugin := libcni.NewCNIConfigWithCacheDir(r.cniBinPaths, r.cniCacheDir, nil)
	return plugin.DelNetworkList(ctx, conf, &libcni.RuntimeConf{
		ContainerID: vmID,
		NetNS:       r.netnsPath,
		IfName:      "eth0",
	})
}

// A private mounted namespace is created before invoking the SDK so the SDK
// cannot unmount it ahead of our independently retried CNI cleanup.
func createOwnedNetNS(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return true, err
	}
	result := make(chan error, 1)
	go func() {
		// Never unlock: Go discards this namespace-bearing OS thread.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			result <- err
			return
		}
		result <- unix.Mount("/proc/thread-self/ns/net", path, "none", unix.MS_BIND, "")
	}()
	return true, <-result
}

func removeOwnedNetNS(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil &&
		!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return os.Remove(path)
}

// processIdentity uses both kernel start time and the intended VMM binary/socket.
// The pidfd is opened before rechecking identity, so PID reuse cannot redirect a
// subsequent signal. No signal is sent when any ownership evidence conflicts.
type processIdentity struct {
	pid       int
	startTime string
	binary    string
	apiSocket string
}

func processStartTime(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	// The first field following comm is field 3; starttime is field 22.
	if len(fields) < 20 {
		return "", fmt.Errorf("incomplete process stat")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", fmt.Errorf("invalid process start time: %w", err)
	}
	return fields[19], nil
}

func (p processIdentity) verify() error {
	start, err := processStartTime(p.pid)
	if err != nil {
		return err
	}
	if start != p.startTime || p.startTime == "" {
		return fmt.Errorf("VMM process identity changed")
	}
	binary, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", p.pid))
	if err != nil {
		return err
	}
	if binary != p.binary {
		return fmt.Errorf("VMM executable identity changed")
	}
	args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p.pid))
	if err != nil {
		return err
	}
	argv := strings.Split(string(args), "\x00")
	for i, arg := range argv {
		if (arg == "--api-sock" && i+1 < len(argv) && argv[i+1] == p.apiSocket) ||
			arg == "--api-sock="+p.apiSocket {
			return nil
		}
	}
	return fmt.Errorf("VMM API socket identity changed")
}

func (p processIdentity) stop(ctx context.Context) error {
	fd, err := unix.PidfdOpen(p.pid, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := p.verify(); err != nil {
		// An exited process can remain a zombie until the SDK reaps it.
		ready, pollErr := pidfdExited(fd, 0)
		if pollErr == nil && ready {
			return nil
		}
		return err
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && !alreadyGone(err) {
		return err
	}
	grace := time.Now().Add(2 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		exited, err := pidfdExited(fd, 50)
		if err != nil || exited {
			return err
		}
		if time.Now().After(grace) {
			break
		}
	}
	// Recheck live identity before escalation; the pidfd itself is stable.
	if err := p.verify(); err != nil {
		exited, pollErr := pidfdExited(fd, 0)
		if pollErr == nil && exited {
			return nil
		}
		return err
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !alreadyGone(err) {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		exited, err := pidfdExited(fd, 50)
		if err != nil || exited {
			return err
		}
	}
}

func pidfdExited(fd, timeoutMillis int) (bool, error) {
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	_, err := unix.Poll(poll, timeoutMillis)
	if errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, err
}

func resolveVMMBinary(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
