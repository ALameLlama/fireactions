package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/hostinger/fireactions/helper/stringid"
	"github.com/opencontainers/image-spec/identity"
	"github.com/rs/zerolog"
	"github.com/sirupsen/logrus"
)

const (
	defaultSnapshotter = "devmapper"
)

// Pool owns Firecracker VMs for one configured image profile.
type Pool struct {
	config         *PoolConfig
	containerd     *containerd.Client
	imageManager   *imageManager
	pendingCreates atomic.Int32
	pendingDeletes atomic.Int32
	machinesMu     *sync.Mutex
	machines       map[string]*Machine
	logger         *zerolog.Logger
	replicas       atomic.Int32
	isActive       bool
	scaleTrigger   chan struct{}
	stopCh         chan struct{}
	doneCh         chan struct{}
	cleanupWg      sync.WaitGroup
	ctx            context.Context
	cancel         context.CancelFunc
	nextCID        *atomic.Uint32
	l              *sync.Mutex
}

// PoolConfig represents the configuration of a Pool.
type PoolConfig struct {
	Name            string             `yaml:"name" validate:"required"`
	Replicas        int                `yaml:"replicas" validate:"min=0"`
	Image           string             `yaml:"image" validate:"required"`
	ImagePullPolicy string             `yaml:"image_pull_policy" validate:"required,oneof=Always Never IfNotPresent"`
	Firecracker     *FirecrackerConfig `yaml:"firecracker" validate:"required"`
}

// NewPool creates a new Pool.
func NewPool(logger *zerolog.Logger, config *PoolConfig, imageManager *imageManager, containerdClient *containerd.Client, nextCID *atomic.Uint32) (*Pool, error) {
	l := logger.With().Str("pool", config.Name).Logger()

	ctx, cancel := context.WithCancel(context.Background())

	p := &Pool{
		config:       config,
		l:            &sync.Mutex{},
		machinesMu:   &sync.Mutex{},
		machines:     make(map[string]*Machine),
		isActive:     true,
		containerd:   containerdClient,
		imageManager: imageManager,
		logger:       &l,
		scaleTrigger: make(chan struct{}, 1),
		stopCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
		nextCID:      nextCID,
	}

	p.replicas.Store(int32(config.Replicas))

	if _, err := os.Stat(p.GetDir()); os.IsNotExist(err) {
		if err := os.MkdirAll(p.GetDir(), 0755); err != nil {
			return nil, fmt.Errorf("creating pool directory: %w", err)
		}

		p.logger.Debug().Msgf("Pool directory created at %s", p.GetDir())
	}

	metricPoolMachinesCurrent.
		WithLabelValues(p.config.Name).Set(float64(p.GetCurrentSize()))
	metricPoolMachinesDesired.
		WithLabelValues(p.config.Name).Set(float64(p.config.Replicas))
	metricPoolStatus.
		WithLabelValues(p.config.Name).Set(1)

	metricPoolsTotal.Inc()

	return p, nil
}

// Run starts the pool. Starting the pool will start the scaling process.
func (p *Pool) Run() {
	defer close(p.doneCh) // Signal that Run() has exited

	// Trigger initial scale
	p.TriggerScale()

	for {
		select {
		case <-p.scaleTrigger:
		case <-time.After(2 * time.Second):
		case <-p.stopCh:
			return
		case <-p.ctx.Done():
			return
		}

		// Check if we should stop before scaling (non-blocking check)
		select {
		case <-p.ctx.Done():
			return
		case <-p.stopCh:
			return
		default:
		}

		curSize := p.GetCurrentSize()
		desiredReplicas := p.GetReplicas()
		pendingCreates := int(p.pendingCreates.Load())
		pendingDeletes := int(p.pendingDeletes.Load())
		netPending := pendingCreates - pendingDeletes
		metricPoolMachinesCurrent.
			WithLabelValues(p.config.Name).Set(float64(curSize))
		metricPoolMachinesDesired.
			WithLabelValues(p.config.Name).Set(float64(desiredReplicas))
		metricPoolMachinesPending.
			WithLabelValues(p.config.Name).Set(float64(netPending))

		if !p.isActive {
			p.logger.Debug().Msgf("Pool %s is paused, skipping scaling", p.config.Name)
			continue
		}

		// Scale to desired replicas
		if err := p.Scale(p.ctx, desiredReplicas); err != nil {
			// Don't log errors if context was cancelled (pool is stopping)
			if p.ctx.Err() == nil {
				p.logger.Error().Err(err).Msg("Failed to scale pool")
			}
		}
	}
}

// Stop stops the pool. Stopping the pool will stop all the VMs in the pool.
func (p *Pool) Stop() {
	p.logger.Debug().Msgf("Stopping pool %s", p.config.Name)
	p.cancel()

	// Signal the Start() loop to exit (non-blocking)
	select {
	case p.stopCh <- struct{}{}:
	default:
		// Channel already has a value or Start() already exited
	}

	// Wait for Run() loop to exit cleanly with a timeout
	select {
	case <-p.doneCh:
	case <-time.After(5 * time.Second):
		p.logger.Warn().Msg("Timeout waiting for Run() to exit")
	}

	p.logger.Debug().Msgf("Stopping %d machines in pool %s", len(p.machines), p.config.Name)

	p.machinesMu.Lock()
	machines := make([]*Machine, 0, len(p.machines))
	for _, machine := range p.machines {
		machines = append(machines, machine)
	}
	p.machinesMu.Unlock()

	// Stop all machines - cleanup goroutines will handle the rest
	for _, machine := range machines {
		vmID := machine.Cfg.VMID

		err := machine.StopVMM()
		if err != nil {
			p.logger.Error().Err(err).Msgf("Failed to stop Firecracker VM %s", vmID)
		}

		p.logger.Debug().Msgf("Stopped Firecracker VM %s", vmID)
	}

	cleanupDone := make(chan struct{})
	go func() {
		p.cleanupWg.Wait()
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
	case <-time.After(35 * time.Second):
		p.logger.Warn().Msg("Timeout waiting for cleanup goroutines to finish")
	}

	p.logger.Debug().Msgf("Pool %s stopped", p.config.Name)
}

// GetDir returns the directory where the pool sockets and logs are stored.
func (p *Pool) GetDir() string {
	return fmt.Sprintf("/var/lib/fireactions/pools/%s", p.config.Name)
}

// Scale scales the pool to the desired size.
func (p *Pool) Scale(ctx context.Context, desiredReplicas int) error {
	p.l.Lock()
	defer p.l.Unlock()

	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	default:
	}

	curSize := p.GetCurrentSize()
	pendingCreates := int(p.pendingCreates.Load())
	pendingDeletes := int(p.pendingDeletes.Load())

	// Calculate effective size accounting for in-flight operations
	effectiveSize := curSize + pendingCreates - pendingDeletes
	delta := desiredReplicas - effectiveSize

	if delta == 0 {
		return nil
	}

	if delta > 0 {
		p.scaleUp(
			ctx, delta, desiredReplicas, curSize, pendingCreates, pendingDeletes)
	} else {
		p.scaleDown(
			ctx, -delta, desiredReplicas, curSize, pendingCreates, pendingDeletes)
	}

	return nil
}

func (p *Pool) scaleUp(ctx context.Context, count, desiredReplicas, curSize, pendingCreates, pendingRemovals int) {
	p.logger.Debug().Msgf("Scaling up by %d VMs (target: %d, current: %d, pending creates: %d, pending removals: %d)",
		count, desiredReplicas, curSize, pendingCreates, pendingRemovals)

	for i := 0; i < count; i++ {
		p.pendingCreates.Add(1)

		go func() {
			defer p.pendingCreates.Add(-1)

			select {
			case <-p.ctx.Done():
				return
			default:
			}

			start := time.Now()
			if err := p.createMachine(ctx); err != nil {
				metricScaleOperations.WithLabelValues(p.config.Name, "up", "failure").Inc()
				p.logger.Error().Err(err).Msg("Failed to create machine")
				return
			}

			duration := time.Since(start).Seconds()
			metricScaleOperations.WithLabelValues(p.config.Name, "up", "success").Inc()
			metricScaleDuration.WithLabelValues(p.config.Name, "up").Observe(duration)
		}()
	}
}

func (p *Pool) scaleDown(ctx context.Context, count, desiredReplicas, curSize, pendingCreates, pendingDeletes int) {
	if count > curSize {
		count = curSize
	}

	p.logger.Debug().Msgf("Scaling down by %d VMs (target: %d, current: %d, pending creates: %d, pending deletes: %d)",
		count, desiredReplicas, curSize, pendingCreates, pendingDeletes)

	for i := 0; i < count; i++ {
		p.pendingDeletes.Add(1)

		go func() {
			defer p.pendingDeletes.Add(-1)

			select {
			case <-p.ctx.Done():
				return
			default:
			}

			start := time.Now()
			if err := p.deleteMachine(ctx); err != nil {
				metricScaleOperations.WithLabelValues(p.config.Name, "down", "failure").Inc()
				p.logger.Error().Err(err).Msg("Failed to delete machine")
				return
			}

			duration := time.Since(start).Seconds()
			metricScaleOperations.WithLabelValues(p.config.Name, "down", "success").Inc()
			metricScaleDuration.WithLabelValues(p.config.Name, "down").Observe(duration)
		}()
	}
}

// Pause pauses the pool. Pausing the pool will prevent the pool from scaling.
func (p *Pool) Pause() {
	if !p.isActive {
		return
	}

	p.logger.Debug().Msgf("Pool %s state changed to paused", p.config.Name)
	p.isActive = false
}

// Resume resumes the pool. Resuming the pool will allow the pool to scale.
func (p *Pool) Resume() {
	if p.isActive {
		return
	}

	p.logger.Debug().Msgf("Pool %s state changed to active", p.config.Name)
	p.isActive = true
}

// SetReplicas updates the desired replica count for the pool in a thread-safe manner.
func (p *Pool) SetReplicas(replicas int) {
	p.replicas.Store(int32(replicas))
	p.TriggerScale()
}

// TriggerScale sends a non-blocking notification to trigger scaling.
func (p *Pool) TriggerScale() {
	select {
	case p.scaleTrigger <- struct{}{}:
	default:
	}
}

// GetReplicas returns the desired replica count for the pool in a thread-safe manner.
func (p *Pool) GetReplicas() int {
	return int(p.replicas.Load())
}

// GetCurrentSize returns the current size of the pool.
func (p *Pool) GetCurrentSize() int {
	p.machinesMu.Lock()
	defer p.machinesMu.Unlock()
	return len(p.machines)
}

func (p *Pool) ListMachines(ctx context.Context) ([]*Machine, error) {
	p.machinesMu.Lock()
	defer p.machinesMu.Unlock()

	machines := make([]*Machine, 0, len(p.machines))
	for _, machine := range p.machines {
		machines = append(machines, machine)
	}

	return machines, nil
}

func (p *Pool) GetMachine(name string) (*Machine, error) {
	p.machinesMu.Lock()
	defer p.machinesMu.Unlock()

	machine, ok := p.machines[name]
	if !ok {
		return nil, fmt.Errorf("machine not found: %s", name)
	}

	return machine, nil
}

func (p *Pool) createMachine(ctx context.Context) error {
	image, err := p.imageManager.ensureImage(
		ctx,
		p.config.Image,
		p.config.ImagePullPolicy,
	)
	if err != nil {
		return fmt.Errorf("ensuring image: %w", err)
	}

	vmID := fmt.Sprintf("%s-%s", p.config.Name, stringid.New())

	leaseCtx, leaseCtxCancel, err := p.containerd.WithLease(ctx,
		leases.WithID(fmt.Sprintf("fireactions/pools/%s/%s", p.config.Name, vmID)))
	if err != nil {
		return fmt.Errorf("containerd: creating lease: %w", err)
	}

	// Track if we successfully created the machine to determine cleanup responsibility
	var machineCreated bool
	defer func() {
		if !machineCreated {
			// Clean up lease if machine creation failed
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = leaseCtxCancel(cleanupCtx)
		}
	}()

	snapshotMounts, err := p.createSnapshot(leaseCtx, image, vmID)
	if err != nil {
		return fmt.Errorf("containerd: creating snapshot: %w", err)
	}

	machineLogFile, err := os.Create(filepath.Join(p.GetDir(), fmt.Sprintf("%s.log", vmID)))
	if err != nil {
		return fmt.Errorf("creating log file: %w", err)
	}
	defer machineLogFile.Close()

	machineCmd := firecracker.VMCommandBuilder{}.
		WithSocketPath(filepath.Join(p.GetDir(), fmt.Sprintf("%s.sock", vmID))).
		WithStderr(machineLogFile).
		WithStdout(machineLogFile).
		WithBin(p.config.Firecracker.BinaryPath).
		Build(ctx)

	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetOutput(io.Discard)

	vsockPath := filepath.Join(p.GetDir(), fmt.Sprintf("%s.vsock", vmID))
	vsockCID := p.nextCID.Add(1)

	rootfsDrive := models.Drive{
		DriveID:      firecracker.String("rootfs"),
		PathOnHost:   &snapshotMounts[0].Source,
		IsRootDevice: firecracker.Bool(true),
		IsReadOnly:   firecracker.Bool(false),
	}

	if rootfsConfig := p.config.Firecracker.Rootfs; rootfsConfig != nil {
		rootfsDrive.RateLimiter = rootfsConfig.RateLimiter.toSDK()
	}

	networkInterface := firecracker.NetworkInterface{
		AllowMMDS:        false,
		CNIConfiguration: &firecracker.CNIConfiguration{NetworkName: "fireactions", IfName: "eth0", ConfDir: "/etc/cni/net.d", BinPath: []string{"/opt/cni/bin"}},
	}

	if networkInterfaceConfig := p.config.Firecracker.NetworkInterface; networkInterfaceConfig != nil {
		networkInterface.InRateLimiter = networkInterfaceConfig.InRateLimiter.toSDK()
		networkInterface.OutRateLimiter = networkInterfaceConfig.OutRateLimiter.toSDK()
	}

	fcMachine, err := firecracker.NewMachine(ctx, firecracker.Config{
		VMID:            vmID,
		SocketPath:      filepath.Join(p.GetDir(), fmt.Sprintf("%s.sock", vmID)),
		KernelImagePath: p.config.Firecracker.KernelImagePath,
		KernelArgs:      p.config.Firecracker.KernelArgs,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &p.config.Firecracker.MachineConfig.VcpuCount,
			MemSizeMib: &p.config.Firecracker.MachineConfig.MemSizeMib,
		},
		Drives:            []models.Drive{rootfsDrive},
		NetworkInterfaces: []firecracker.NetworkInterface{networkInterface},
		VsockDevices:      []firecracker.VsockDevice{{Path: vsockPath, CID: vsockCID}},
		ForwardSignals:    []os.Signal{},
		LogPath:           filepath.Join(p.GetDir(), fmt.Sprintf("%s.firecracker.log", vmID)),
		LogLevel:          "Debug",
	}, firecracker.WithProcessRunner(machineCmd), firecracker.WithLogger(logrus.NewEntry(logger)))
	if err != nil {
		return fmt.Errorf("firecracker: creating machine: %w", err)
	}

	vmmCtx, vmmCancel := context.WithCancel(p.ctx)
	if err := fcMachine.Start(vmmCtx); err != nil {
		vmmCancel()
		return fmt.Errorf("firecracker: starting machine: %w", err)
	}

	// Mark machine as successfully created
	machineCreated = true

	p.logger.Info().Msgf("Successfully created Firecracker VM %s", vmID)

	machine := &Machine{
		Machine:     fcMachine,
		Name:        vmID,
		State:       "idle",
		Pool:        p.config.Name,
		CreatedAt:   time.Now().UTC(),
		vsockCID:    vsockCID,
		vsockPath:   vsockPath,
		leaseCancel: leaseCtxCancel,
		vmmCtx:      vmmCtx,
		vmmCancel:   vmmCancel,
	}

	p.machinesMu.Lock()
	p.machines[vmID] = machine
	p.machinesMu.Unlock()

	// Start cleanup goroutine
	p.cleanupWg.Add(1)
	go func() {
		defer p.cleanupWg.Done()

		waitDone := make(chan error, 1)
		go func() {
			waitDone <- machine.Wait(context.Background())
		}()

		select {
		case <-waitDone:
			// Machine exited normally
		case <-p.ctx.Done():
			// Pool is stopping, wait up to 30s for machine to fully exit
			select {
			case <-waitDone:
			case <-time.After(30 * time.Second):
				p.logger.Warn().Msgf("Timeout waiting for machine %s to exit during pool shutdown", vmID)
			}
		}

		p.machinesMu.Lock()
		_, exists := p.machines[vmID]
		if exists {
			delete(p.machines, vmID)
		}
		p.machinesMu.Unlock()

		machine.vmmCancel()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := machine.leaseCancel(ctx)
		if err != nil && !errdefs.IsNotFound(err) {
			p.logger.Error().Err(err).Msgf("Failed to remove Containerd lease for Firecracker VM %s", vmID)
		}

		p.logger.Info().Msgf("Successfully cleaned up exited Firecracker VM %s", vmID)
	}()

	return nil
}

// removeMachine removes a single machine from the pool.
func (p *Pool) deleteMachine(_ context.Context) error {
	p.machinesMu.Lock()

	// Find a machine to remove (pick the first one)
	var targetMachine *Machine
	var targetName string
	for name, machine := range p.machines {
		targetMachine = machine
		targetName = name
		break
	}

	if targetMachine == nil {
		p.machinesMu.Unlock()
		return fmt.Errorf("no machines available to scale down")
	}

	// Remove from map immediately to prevent selecting the same machine multiple times
	delete(p.machines, targetName)
	p.machinesMu.Unlock()

	err := targetMachine.StopVMM()
	if err != nil {
		p.logger.Warn().Err(err).Msgf("Failed to stop VM %s", targetName)
		return err
	}

	p.logger.Info().Msgf("Successfully removed VM %s", targetName)
	return nil
}

// createSnapshot creates a snapshot of the specified image.
func (p *Pool) createSnapshot(ctx context.Context, image containerd.Image, snapshotID string) ([]mount.Mount, error) {
	snapshotService := p.containerd.SnapshotService(defaultSnapshotter)
	snapshotExists := true
	_, err := snapshotService.Stat(ctx, snapshotID)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, err
		}

		snapshotExists = false
	}

	if !snapshotExists {
		imageContent, err := image.RootFS(ctx)
		if err != nil {
			return nil, fmt.Errorf("image: rootfs: %w", err)
		}

		_, err = snapshotService.Prepare(ctx, snapshotID, identity.ChainID(imageContent).String())
		if err != nil {
			return nil, fmt.Errorf("prepare: %w", err)
		}
	}

	mounts, err := snapshotService.Mounts(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("mounts: %w", err)
	}

	return mounts, nil
}

func init() {
	_ = log.SetLevel("panic")
}
