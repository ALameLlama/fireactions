package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/errdefs"
	"github.com/containernetworking/cni/libcni"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/hostinger/fireactions/helper/stringid"
	"github.com/hostinger/fireactions/internal/executor"
	"github.com/hostinger/fireactions/internal/guest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// Machine holds host metadata and owns every resource of a disposable VM.
// Initial metadata may be supplied in literals; after publication, use the
// metadata helpers rather than accessing lifecycle fields directly.
type Machine struct {
	*firecracker.Machine

	Name          string
	Pool          string
	CreatedAt     time.Time
	State         string
	AgentVersion  string
	EnvironmentID string

	metadataMu     sync.RWMutex
	info           executor.VMInfo
	vsockCID       uint32
	vsockPath      string
	guestMu        sync.Mutex
	guestConn      *grpc.ClientConn
	guestClient    *guest.Client
	guestClosed    bool
	resources      *ownedResources
	cleanupQueued  atomic.Bool
	vmmCancel      context.CancelFunc
	readySpec      executor.ReadySpec
	startupTimeout time.Duration
	defaultUser    string
}

var _ executor.VM = (*Machine)(nil)

type MachineMetadata struct {
	State         string
	AgentVersion  string
	EnvironmentID string
}

func (m *Machine) Metadata() MachineMetadata {
	m.metadataMu.RLock()
	defer m.metadataMu.RUnlock()
	return MachineMetadata{State: m.State, AgentVersion: m.AgentVersion, EnvironmentID: m.EnvironmentID}
}

func (m *Machine) SetState(state, environmentID string) {
	m.metadataMu.Lock()
	m.State, m.EnvironmentID = state, environmentID
	m.metadataMu.Unlock()
}

func (m *Machine) SetAgentVersion(version string) {
	m.metadataMu.Lock()
	m.AgentVersion = version
	m.metadataMu.Unlock()
}

func (m *Machine) ID() string { return m.Name }

func (m *Machine) Info() executor.VMInfo {
	info := m.info
	info.ImageEnv = maps.Clone(info.ImageEnv)
	if m.defaultUser != "" {
		info.DefaultUser = m.defaultUser
	}
	return info
}

// ConnectToGuestAgent waits for and verifies the private agent, then returns its
// machine-owned client. The connection is never exposed to callers.
func (m *Machine) ConnectToGuestAgent(ctx context.Context) (executor.Guest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.guestMu.Lock()
	if m.guestClosed {
		m.guestMu.Unlock()
		return nil, fmt.Errorf("guest connection is closed")
	}
	if m.guestConn == nil {
		dialer := func(ctx context.Context, _ string) (net.Conn, error) {
			return vsock.DialContext(ctx, m.vsockPath, 9001)
		}
		conn, err := grpc.NewClient("passthrough:vsock",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(dialer))
		if err != nil {
			m.guestMu.Unlock()
			return nil, fmt.Errorf("grpc dial: %w", err)
		}
		m.guestConn = conn
		m.guestClient = guest.New(conn)
	}
	conn, client := m.guestConn, m.guestClient
	m.guestMu.Unlock()
	startupTimeout := m.startupTimeout
	if startupTimeout <= 0 {
		startupTimeout = 2 * time.Minute
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	ready := m.readySpec
	if len(ready.Directories) == 0 {
		ready.Directories = []string{m.info.Layout.Root, m.info.Layout.Act, m.info.Layout.ToolCache, m.info.Layout.Temp}
	}
	if ready.DefaultUser == "" {
		ready.DefaultUser = m.Info().DefaultUser
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := startupCtx.Err(); err != nil {
			return nil, err
		}
		state := conn.GetState()
		if state == connectivity.Shutdown {
			return nil, executor.NewError(executor.Unavailable, "guest agent connection closed", nil)
		}
		if state == connectivity.Idle {
			conn.Connect()
		}
		if state == connectivity.Ready {
			version, err := client.Ready(startupCtx, ready)
			if err == nil {
				m.SetAgentVersion(version)
				return client, nil
			}
			if startupCtx.Err() != nil {
				return nil, startupCtx.Err()
			}
			if executor.KindOf(err) != executor.Unavailable {
				return nil, err
			}
		}
		select {
		case <-startupCtx.Done():
			return nil, startupCtx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Machine) Guest() executor.Guest {
	m.guestMu.Lock()
	defer m.guestMu.Unlock()
	return m.guestClient
}

func (m *Machine) GuestLogs(ctx context.Context, follow bool, tailLines int32, send func(string) error) error {
	client, err := m.ConnectToGuestAgent(ctx)
	if err != nil {
		return err
	}
	logs, ok := client.(interface {
		Logs(context.Context, bool, int32, func(string) error) error
	})
	if !ok {
		return fmt.Errorf("guest client does not provide logs")
	}
	return logs.Logs(ctx, follow, tailLines, send)
}

func (m *Machine) closeGuest() error {
	m.guestMu.Lock()
	defer m.guestMu.Unlock()
	m.guestClosed = true
	if m.guestConn == nil {
		return nil
	}
	err := m.guestConn.Close()
	m.guestConn = nil
	return err
}

// Destroy ignores caller cancellation for cleanup. Closing the control channel
// first cancels guest RPC scopes before any wait for the shared teardown attempt.
func (m *Machine) Destroy(_ context.Context) error {
	m.metadataMu.Lock()
	m.State = "removing"
	m.metadataMu.Unlock()
	_ = m.closeGuest()
	if m.resources == nil {
		return fmt.Errorf("machine has no owned resource record")
	}
	return m.resources.destroy()
}

func (m *Machine) GetAddr() string {
	if m.Machine == nil {
		return ""
	}
	interfaces := m.Cfg.NetworkInterfaces
	if len(interfaces) == 0 || interfaces[0].StaticConfiguration == nil || interfaces[0].StaticConfiguration.IPConfiguration == nil {
		return ""
	}
	return interfaces[0].StaticConfiguration.IPConfiguration.IPAddr.IP.String()
}

func allocateCID(counter *atomic.Uint32) (uint32, error) {
	if counter == nil {
		return 0, fmt.Errorf("missing vsock CID allocator")
	}
	for {
		current := counter.Load()
		// 0/1/2 and UINT32_MAX are reserved; never wrap the counter.
		if current >= ^uint32(0)-1 {
			return 0, fmt.Errorf("vsock CID space exhausted")
		}
		next := current + 1
		if next < 3 {
			next = 3
		}
		if counter.CompareAndSwap(current, next) {
			return next, nil
		}
	}
}

func imageVMInfo(image ocispec.Image) (executor.VMInfo, error) {
	if image.OS != "linux" {
		return executor.VMInfo{}, fmt.Errorf("unsupported guest OS %q", image.OS)
	}
	layout, err := executor.DefaultLayout(image.Architecture)
	if err != nil {
		return executor.VMInfo{}, err
	}
	imageEnv := make(map[string]string, len(image.Config.Env))
	for _, value := range image.Config.Env {
		key, value, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			return executor.VMInfo{}, fmt.Errorf("invalid image environment entry")
		}
		imageEnv[key] = value
	}
	user := image.Config.User
	if user == "" {
		user = executor.DefaultUser
	}
	return executor.VMInfo{
		Layout:      layout,
		ImageEnv:    imageEnv,
		DefaultUser: user,
	}, nil
}

func (p *Pool) provisionMachine(ctx context.Context) (_ *Machine, resultErr error) {
	image, err := p.imageManager.ensureImage(ctx, p.config.Image, p.config.ImagePullPolicy)
	if err != nil {
		return nil, fmt.Errorf("ensuring image: %w", err)
	}
	descriptor, err := image.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading image configuration descriptor: %w", err)
	}
	configJSON, err := content.ReadBlob(ctx, p.containerd.ContentStore(), descriptor)
	if err != nil {
		return nil, fmt.Errorf("reading image configuration: %w", err)
	}
	var imageConfig ocispec.Image
	if err := json.Unmarshal(configJSON, &imageConfig); err != nil {
		return nil, fmt.Errorf("decoding image configuration: %w", err)
	}
	info, err := imageVMInfo(imageConfig)
	if err != nil {
		return nil, err
	}
	if imageConfig.Architecture != runtime.GOARCH {
		return nil, fmt.Errorf("guest architecture does not match host")
	}
	binary, err := resolveVMMBinary(p.config.Firecracker.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("resolving Firecracker binary: %w", err)
	}
	cid, err := allocateCID(p.nextCID)
	if err != nil {
		return nil, err
	}
	vmID := p.config.Name + "-" + stringid.New()
	if p.stateDir == "" {
		return nil, fmt.Errorf("runtime state directory is not configured")
	}
	if err := os.MkdirAll(p.GetDir(), 0700); err != nil {
		return nil, fmt.Errorf("creating pool resource directory: %w", err)
	}
	if err := os.Chmod(p.GetDir(), 0700); err != nil {
		return nil, fmt.Errorf("protecting pool resource directory: %w", err)
	}
	resourceDir := filepath.Join(p.GetDir(), vmID)
	apiSocket := filepath.Join(resourceDir, "api.sock")
	if len(apiSocket) >= 108 {
		return nil, fmt.Errorf("VM socket path exceeds Unix socket limit")
	}
	r := &ownedResources{
		snapshotID:  vmID,
		leaseID:     "fireactions/pools/" + p.config.Name + "/" + vmID,
		cniBinPaths: []string{"/opt/cni/bin"},
		cniCacheDir: filepath.Join(resourceDir, "cni"),
		netnsPath:   filepath.Join("/var/run/netns", vmID),
	}
	m := &Machine{
		Name: vmID, Pool: p.config.Name, State: "provisioning", CreatedAt: time.Now().UTC(),
		info: info, vsockCID: cid, vsockPath: filepath.Join(resourceDir, "vsock"), resources: r,
		readySpec: p.ready, startupTimeout: p.startupTimeout, defaultUser: p.config.DefaultUser,
	}
	var leaseCancel func(context.Context) error
	var snapshotOwned, directoryOwned, netnsOwned, networkAttempted bool
	var cmd *exec.Cmd
	var process processIdentity
	var sdkStarted bool
	r.steps = []cleanupStep{
		{name: "stopping VMM", run: func(cleanupCtx context.Context) error {
			if cmd == nil || cmd.Process == nil {
				if m.vmmCancel != nil {
					m.vmmCancel()
				}
				return nil
			}
			if err := process.stop(cleanupCtx); err != nil && !alreadyGone(err) {
				return err
			}
			// Only release the SDK lifetime context after verified process exit:
			// its cancellation handler sends an otherwise unchecked SIGTERM.
			if m.vmmCancel != nil {
				m.vmmCancel()
			}
			if sdkStarted {
				// Wait also settles the SDK's own cleanup callbacks. A nonzero
				// process exit is not a failure to destroy an exited VMM.
				_ = m.Machine.Wait(cleanupCtx)
				return cleanupCtx.Err()
			}
			return nil
		}},
		{name: "deleting CNI network", run: func(cleanupCtx context.Context) error {
			if !networkAttempted {
				return nil
			}
			return deleteOwnedNetwork(cleanupCtx, r, vmID)
		}},
		{name: "removing network namespace", run: func(context.Context) error {
			if !netnsOwned {
				return nil
			}
			return removeOwnedNetNS(r.netnsPath)
		}},
		{name: "removing writable snapshot", run: func(cleanupCtx context.Context) error {
			if !snapshotOwned {
				return nil
			}
			return p.containerd.SnapshotService(defaultSnapshotter).Remove(cleanupCtx, r.snapshotID)
		}},
		{name: "deleting containerd lease", run: func(cleanupCtx context.Context) error {
			if leaseCancel == nil {
				return nil
			}
			return leaseCancel(cleanupCtx)
		}},
		{name: "removing VM paths", run: func(context.Context) error {
			if !directoryOwned {
				return nil
			}
			return os.RemoveAll(resourceDir)
		}},
	}
	defer func() {
		if resultErr == nil {
			return
		}
		if cleanupErr := m.Destroy(context.Background()); cleanupErr != nil {
			// Incomplete provisioning is still owned and must be retried.
			p.machinesMu.Lock()
			p.machines[vmID] = m
			p.machinesMu.Unlock()
			resultErr = errors.Join(resultErr, fmt.Errorf("unwinding VM resources: %w", cleanupErr))
		}
	}()
	if err := os.Mkdir(resourceDir, 0700); err != nil {
		return nil, fmt.Errorf("creating VM resource directory: %w", err)
	}
	directoryOwned = true
	lease := leases.Lease{ID: r.leaseID}
	// Record the intended unique resource even if a transport error loses the
	// allocation response. Never adopt or delete an AlreadyExists collision.
	leaseCancel = func(cleanupCtx context.Context) error {
		return p.containerd.LeasesService().Delete(cleanupCtx, lease)
	}
	if _, err := p.containerd.LeasesService().Create(ctx, leases.WithID(r.leaseID)); err != nil {
		if errdefs.IsAlreadyExists(err) {
			leaseCancel = nil
		}
		return nil, fmt.Errorf("creating containerd lease: %w", err)
	}
	leaseCtx := leases.WithLease(ctx, r.leaseID)
	rootfs, err := image.RootFS(leaseCtx)
	if err != nil {
		return nil, fmt.Errorf("reading image rootfs: %w", err)
	}
	snapshotOwned = true
	mounts, err := p.containerd.SnapshotService(defaultSnapshotter).Prepare(leaseCtx, r.snapshotID, identity.ChainID(rootfs).String())
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			snapshotOwned = false
		}
		return nil, fmt.Errorf("preparing writable snapshot: %w", err)
	}
	if len(mounts) == 0 || mounts[0].Source == "" {
		return nil, fmt.Errorf("writable snapshot has no block source")
	}
	block, err := os.Stat(mounts[0].Source)
	if err != nil {
		return nil, fmt.Errorf("checking writable snapshot block source: %w", err)
	}
	if block.Mode()&os.ModeDevice == 0 || block.Mode()&os.ModeCharDevice != 0 {
		return nil, fmt.Errorf("writable snapshot source is not a block device")
	}
	conf, err := libcni.LoadConfList("/etc/cni/net.d", "fireactions")
	if err != nil {
		return nil, fmt.Errorf("loading fireactions CNI network: %w", err)
	}
	effective, err := effectiveCNIConfig(conf.Bytes, p.resolverPath)
	if err != nil {
		return nil, err
	}
	conf, err = libcni.ConfListFromBytes(effective)
	if err != nil {
		return nil, fmt.Errorf("parsing effective CNI network: %w", err)
	}
	r.cniConfig = effective
	if err := os.MkdirAll(r.cniCacheDir, 0700); err != nil {
		return nil, fmt.Errorf("creating CNI cache directory: %w", err)
	}
	// Own the namespace ourselves: the SDK then sees a pre-existing namespace
	// and does not detach it in its exit callbacks before independent CNI DEL.
	netnsOwned, err = createOwnedNetNS(r.netnsPath)
	if err != nil {
		return nil, fmt.Errorf("creating network namespace: %w", err)
	}
	machineLog, err := os.OpenFile(filepath.Join(resourceDir, "vmm.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("creating VM log: %w", err)
	}
	defer machineLog.Close()
	// The VM lifetime is not the provisioning/request context. Owned destruction
	// uses a verified pidfd; CommandContext must not kill a reused numeric PID.
	cmd = firecracker.VMCommandBuilder{}.WithSocketPath(apiSocket).
		WithStderr(machineLog).WithStdout(machineLog).WithBin(binary).Build(context.Background())
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetOutput(io.Discard)
	rootDrive := models.Drive{
		DriveID: firecracker.String("rootfs"), PathOnHost: &mounts[0].Source,
		IsRootDevice: firecracker.Bool(true), IsReadOnly: firecracker.Bool(false),
	}
	if p.config.Firecracker.Rootfs != nil {
		rootDrive.RateLimiter = p.config.Firecracker.Rootfs.RateLimiter.toSDK()
	}
	iface := firecracker.NetworkInterface{AllowMMDS: false, CNIConfiguration: &firecracker.CNIConfiguration{
		NetworkConfig: conf, IfName: "eth0", VMIfName: "eth0", BinPath: r.cniBinPaths, CacheDir: r.cniCacheDir,
	}}
	if p.config.Firecracker.NetworkInterface != nil {
		iface.InRateLimiter = p.config.Firecracker.NetworkInterface.InRateLimiter.toSDK()
		iface.OutRateLimiter = p.config.Firecracker.NetworkInterface.OutRateLimiter.toSDK()
	}
	fcMachine, err := firecracker.NewMachine(ctx, firecracker.Config{
		VMID: vmID, SocketPath: apiSocket, NetNS: r.netnsPath,
		KernelImagePath: p.config.Firecracker.KernelImagePath, KernelArgs: p.config.Firecracker.KernelArgs,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &p.config.Firecracker.MachineConfig.VcpuCount,
			MemSizeMib: &p.config.Firecracker.MachineConfig.MemSizeMib,
		},
		Drives: []models.Drive{rootDrive}, NetworkInterfaces: []firecracker.NetworkInterface{iface},
		VsockDevices:   []firecracker.VsockDevice{{Path: m.vsockPath, CID: cid}},
		ForwardSignals: []os.Signal{}, LogPath: filepath.Join(resourceDir, "firecracker.log"), LogLevel: "Debug",
	}, firecracker.WithProcessRunner(cmd), firecracker.WithLogger(logrus.NewEntry(logger)))
	if err != nil {
		return nil, fmt.Errorf("creating Firecracker machine: %w", err)
	}
	m.Machine = fcMachine
	fcMachine.Handlers.FcInit = fcMachine.Handlers.FcInit.Swap(firecracker.Handler{
		Name: firecracker.SetupNetworkHandlerName,
		Fn: func(startCtx context.Context, machine *firecracker.Machine) error {
			networkAttempted = true
			return setupOwnedNetwork(startCtx, machine, r)
		},
	})
	vmmCtx, cancel := context.WithCancel(context.Background())
	m.vmmCancel = cancel
	stopStartup := context.AfterFunc(ctx, cancel)
	startErr := fcMachine.Start(vmmCtx)
	sdkStarted = startErr == nil
	stopStartup()
	if cmd.Process != nil {
		process = processIdentity{pid: cmd.Process.Pid, binary: binary, apiSocket: apiSocket}
		process.startTime, err = processStartTime(process.pid)
		if err != nil && !alreadyGone(err) {
			return nil, fmt.Errorf("capturing VMM process identity: %w", err)
		}
	}
	if startErr != nil {
		return nil, fmt.Errorf("starting Firecracker machine: %w", startErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.SetState("idle", "")
	return m, nil
}
