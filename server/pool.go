package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/log"
	"github.com/hostinger/fireactions/internal/executor"
	"github.com/rs/zerolog"
)

const (
	defaultSnapshotter = "devmapper"
)

// Pool owns Firecracker VMs for one configured image profile.
type Pool struct {
	config           *PoolConfig
	containerd       *containerd.Client
	imageManager     *imageManager
	replicas         atomic.Int32
	machinesMu       *sync.Mutex
	machines         map[string]*Machine
	poolCleanup      map[*Machine]struct{} // guarded by machinesMu
	logger           *zerolog.Logger
	scaleTrigger     chan struct{}
	stopCh           chan struct{}
	doneCh           chan struct{}
	stopOnce         sync.Once
	runStarted       bool // guarded by l
	isActive         bool
	acquireWg        sync.WaitGroup
	workWg           sync.WaitGroup
	cleanupWg        sync.WaitGroup
	ctx              context.Context
	cancel           context.CancelFunc
	nextCID          *atomic.Uint32
	l                sync.Mutex
	scaleGeneration  uint64                    // guarded by l
	nextIdleCreate   uint64                    // guarded by l
	idleProvisioning map[uint64]*idleProvision // guarded by l
	stateDir         string
	resolverPath     string
	startupTimeout   time.Duration
	ready            executor.ReadySpec
}

type idleProvision struct {
	cancel     context.CancelFunc
	generation uint64
	cancelled  bool
}

type PoolConfig struct {
	Name            string             `yaml:"name" validate:"required"`
	Replicas        int                `yaml:"replicas" validate:"min=0"`
	Image           string             `yaml:"image" validate:"required"`
	ImagePullPolicy string             `yaml:"image_pull_policy" validate:"required,oneof=Always Never IfNotPresent"`
	DefaultUser     string             `yaml:"default_user"`
	Firecracker     *FirecrackerConfig `yaml:"firecracker" validate:"required"`
}

// configureRuntime injects daemon-owned paths and trusted guest readiness values
// before Run begins.
func (p *Pool) configureRuntime(stateDir, resolverPath string, startupTimeout time.Duration, ready executor.ReadySpec) {
	p.stateDir = stateDir
	p.resolverPath = resolverPath
	p.startupTimeout = startupTimeout
	p.ready = ready
}

// NewPool creates a new Pool.
func NewPool(logger *zerolog.Logger, config *PoolConfig, imageManager *imageManager, containerdClient *containerd.Client, nextCID *atomic.Uint32) (*Pool, error) {
	l := logger.With().Str("pool", config.Name).Logger()

	ctx, cancel := context.WithCancel(context.Background())

	p := &Pool{
		config:           config,
		machinesMu:       &sync.Mutex{},
		machines:         make(map[string]*Machine),
		poolCleanup:      make(map[*Machine]struct{}),
		idleProvisioning: make(map[uint64]*idleProvision),
		isActive:         true,
		containerd:       containerdClient,
		imageManager:     imageManager,
		logger:           &l,
		scaleTrigger:     make(chan struct{}, 1),
		stopCh:           make(chan struct{}, 1),
		doneCh:           make(chan struct{}),
		ctx:              ctx,
		cancel:           cancel,
		nextCID:          nextCID,
	}

	p.replicas.Store(int32(config.Replicas))
	p.refreshMetrics()

	return p, nil
}

// Run starts the pool. Starting the pool will start the scaling process.
func (p *Pool) Run() {
	p.l.Lock()
	if p.ctx.Err() != nil || p.runStarted {
		p.l.Unlock()
		return
	}
	p.runStarted = true
	p.l.Unlock()
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

		p.retryRemovals()
		desiredReplicas := p.GetReplicas()

		if !p.IsActive() {
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

// Stop cancels provisioning before destroying all owned machines. Failed cleanup
// remains in the pool registry rather than forgetting a potentially live VM.
func (p *Pool) Stop() {
	p.stopOnce.Do(func() {
		p.cancel()
		select {
		case p.stopCh <- struct{}{}:
		default:
		}

		// This barrier prevents any later WaitGroup Add from scale, cleanup
		// retry, or acquisition admission. It also prevents Run from starting
		// after Stop has decided whether to join it.
		p.l.Lock()
		p.cancelIdleProvisioningLocked()
		runStarted := p.runStarted
		p.l.Unlock()
		if runStarted {
			<-p.doneCh
		}

		p.workWg.Wait()
		p.acquireWg.Wait()
		machines, _ := p.ListMachines(context.Background())
		var destruction sync.WaitGroup
		for _, machine := range machines {
			destruction.Add(1)
			go func(m *Machine) {
				defer destruction.Done()
				if err := p.destroyMachine(context.Background(), m); err != nil {
					p.logger.Error().Err(err).Str("vm_id", m.Name).Msg("Failed to clean up VM during pool stop")
				}
			}(machine)
		}
		destruction.Wait()
		p.cleanupWg.Wait()
	})
}

// GetDir returns the directory where the pool sockets and logs are stored.
func (p *Pool) GetDir() string {
	return filepath.Join(p.stateDir, "pools", p.config.Name)
}

// Scale reconciles the number of ready clean-idle VMs to desiredReplicas.
func (p *Pool) Scale(ctx context.Context, desiredReplicas int) error {
	if desiredReplicas < 0 {
		return fmt.Errorf("replica count must not be negative")
	}

	p.l.Lock()
	defer p.l.Unlock()
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.setDesiredLocked(desiredReplicas)
	if !p.isActive {
		p.cancelIdleProvisioningLocked()
		return nil
	}
	p.reconcileScaleLocked(ctx, desiredReplicas)
	return nil
}

func (p *Pool) setDesiredLocked(desiredReplicas int) {
	if int(p.replicas.Load()) == desiredReplicas {
		return
	}
	p.replicas.Store(int32(desiredReplicas))
	p.scaleGeneration++
	for _, provision := range p.idleProvisioning {
		if !provision.cancelled {
			provision.generation = p.scaleGeneration
		}
	}
}

func (p *Pool) reconcileScaleLocked(ctx context.Context, desiredReplicas int) {
	idle, pending := p.pruneIdleProvisioningLocked(desiredReplicas)
	excess := idle + pending - desiredReplicas
	if excess > 0 {
		p.removeIdleLocked(excess, ctx)
		return
	}

	for range desiredReplicas - idle - pending {
		p.startIdleProvisionLocked(ctx)
	}
}

func (p *Pool) pruneIdleProvisioningLocked(desiredReplicas int) (idle, pending int) {
	if p.idleProvisioning == nil {
		p.idleProvisioning = make(map[uint64]*idleProvision)
	}
	idle = p.countIdleMachines()
	for _, provision := range p.idleProvisioning {
		if !provision.cancelled {
			pending++
		}
	}

	excess := idle + pending - desiredReplicas
	for _, provision := range p.idleProvisioning {
		if excess <= 0 {
			break
		}
		if provision.cancelled {
			continue
		}
		provision.cancelled = true
		provision.cancel()
		pending--
		excess--
	}
	return idle, pending
}

func (p *Pool) startIdleProvisionLocked(ctx context.Context) {
	p.nextIdleCreate++
	id := p.nextIdleCreate
	createCtx, cancel := context.WithCancel(ctx)
	provision := &idleProvision{cancel: cancel, generation: p.scaleGeneration}
	p.idleProvisioning[id] = provision
	p.workWg.Add(1)
	go func() {
		defer p.workWg.Done()
		defer p.finishIdleProvision(id, provision)
		if err := p.createIdleMachine(createCtx, id, provision); err != nil {
			if p.ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				p.logger.Error().Err(err).Msg("Failed to create idle machine")
			}
		}
	}()
}

func (p *Pool) finishIdleProvision(id uint64, provision *idleProvision) {
	p.l.Lock()
	if p.idleProvisioning[id] == provision {
		delete(p.idleProvisioning, id)
	}
	cancelled := provision.cancelled
	p.l.Unlock()
	provision.cancel()
	if cancelled {
		p.TriggerScale()
	}
}

func (p *Pool) cancelIdleProvisioningLocked() {
	for _, provision := range p.idleProvisioning {
		if provision.cancelled {
			continue
		}
		provision.cancelled = true
		provision.cancel()
	}
}

func (p *Pool) countIdleMachines() int {
	p.machinesMu.Lock()
	count := 0
	for _, machine := range p.machines {
		if machine.Metadata().State == "idle" {
			count++
		}
	}
	p.machinesMu.Unlock()
	return count
}

func (p *Pool) removeIdleLocked(count int, ctx context.Context) {
	p.machinesMu.Lock()
	removed := make([]*Machine, 0, count)
	for _, machine := range p.machines {
		if len(removed) == count {
			break
		}
		if machine.Metadata().State != "idle" {
			continue
		}
		machine.SetState("removing", "")
		if p.poolCleanup == nil {
			p.poolCleanup = make(map[*Machine]struct{})
		}
		p.poolCleanup[machine] = struct{}{}
		removed = append(removed, machine)
	}
	p.machinesMu.Unlock()
	if len(removed) > 0 {
		p.refreshMetrics()
	}

	for _, machine := range removed {
		p.workWg.Add(1)
		go func(m *Machine) {
			defer p.workWg.Done()
			if p.ctx.Err() != nil {
				return
			}
			if err := p.destroyMachine(ctx, m); err != nil {
				p.logger.Error().Err(err).Str("vm_id", m.Name).Msg("Failed to delete idle machine")
			}
		}(machine)
	}
}

// IsActive reads pause state under the same lock used by scaling.
func (p *Pool) IsActive() bool {
	p.l.Lock()
	defer p.l.Unlock()
	return p.isActive
}

func (p *Pool) Pause() {
	p.l.Lock()
	p.isActive = false
	p.scaleGeneration++
	p.cancelIdleProvisioningLocked()
	p.l.Unlock()
}

func (p *Pool) Resume() {
	p.l.Lock()
	p.isActive = true
	p.scaleGeneration++
	p.l.Unlock()
	p.TriggerScale()
}

// SetReplicas updates the desired replica count for the pool in a thread-safe manner.
func (p *Pool) SetReplicas(replicas int) {
	if replicas < 0 {
		return
	}
	p.l.Lock()
	p.setDesiredLocked(replicas)
	if p.isActive {
		p.pruneIdleProvisioningLocked(replicas)
	}
	p.l.Unlock()
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

// GetCurrentSize returns the number of ready, clean-idle VMs in the pool.
func (p *Pool) GetCurrentSize() int {
	return p.countIdleMachines()
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

func (p *Pool) acquire(ctx context.Context, expiry time.Time) (executor.VM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.l.Lock()
	if err := p.ctx.Err(); err != nil {
		p.l.Unlock()
		return nil, err
	}
	p.acquireWg.Add(1)
	p.l.Unlock()
	defer p.acquireWg.Done()

	acquireCtx, cancelAcquire := p.acquireContext(ctx, expiry)
	defer cancelAcquire()
	if err := acquireCtx.Err(); err != nil {
		return nil, err
	}

	if machine := p.claimIdleMachine(); machine != nil {
		p.TriggerScale()
		if err := acquireCtx.Err(); err != nil {
			p.markPoolCleanup(machine)
			return nil, errors.Join(err, p.destroyMachine(context.Background(), machine))
		}
		return machine, nil
	}

	p.l.Lock()
	active := p.isActive && p.ctx.Err() == nil
	p.l.Unlock()
	if err := acquireCtx.Err(); err != nil {
		return nil, err
	}
	if !active {
		return nil, executor.NewError(executor.Unavailable, fmt.Sprintf("image profile %q is paused and has no ready idle VM", p.config.Name), nil)
	}

	startupCtx, cancelStartup := p.startupContext(acquireCtx)
	defer cancelStartup()
	machine, err := p.provisionMachine(startupCtx)
	if err != nil {
		return nil, err
	}
	if _, err := machine.ConnectToGuestAgent(startupCtx); err != nil {
		cleanupErr := p.destroyUnpublishedMachine(machine)
		return nil, errors.Join(fmt.Errorf("guest readiness: %w", err), cleanupErr)
	}
	if err := startupCtx.Err(); err != nil {
		return nil, errors.Join(err, p.destroyUnpublishedMachine(machine))
	}

	p.machinesMu.Lock()
	if err := startupCtx.Err(); err != nil {
		p.machinesMu.Unlock()
		return nil, errors.Join(err, p.destroyUnpublishedMachine(machine))
	}
	machine.SetState("claimed", "")
	p.machines[machine.Name] = machine
	p.watchMachineLocked(machine)
	p.machinesMu.Unlock()
	p.refreshMetrics()
	p.TriggerScale()

	if err := startupCtx.Err(); err != nil {
		p.markPoolCleanup(machine)
		return nil, errors.Join(err, p.destroyMachine(context.Background(), machine))
	}
	return machine, nil
}

func (p *Pool) acquireContext(ctx context.Context, expiry time.Time) (context.Context, context.CancelFunc) {
	baseCtx, cancelBase := context.WithCancel(ctx)
	stopOnPoolShutdown := context.AfterFunc(p.ctx, cancelBase)
	acquireCtx := baseCtx
	var cancelDeadline context.CancelFunc
	if !expiry.IsZero() {
		acquireCtx, cancelDeadline = context.WithDeadline(acquireCtx, expiry)
	}
	return acquireCtx, func() {
		stopOnPoolShutdown()
		if cancelDeadline != nil {
			cancelDeadline()
		}
		cancelBase()
	}
}

func (p *Pool) startupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.startupTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.startupTimeout)
}

func (p *Pool) claimIdleMachine() *Machine {
	p.machinesMu.Lock()
	var target *Machine
	for _, machine := range p.machines {
		if machine.Metadata().State != "idle" {
			continue
		}
		machine.SetState("claimed", "")
		target = machine
		break
	}
	p.machinesMu.Unlock()
	if target != nil {
		p.refreshMetrics()
	}
	return target
}

func (p *Pool) watchMachineLocked(machine *Machine) {
	p.cleanupWg.Add(1)
	go func() {
		defer p.cleanupWg.Done()
		_ = machine.Wait(p.ctx)
		if err := p.destroyMachine(context.Background(), machine); err != nil {
			p.logger.Error().Err(err).Str("vm_id", machine.Name).Msg("Failed to clean up exited VM")
		}
	}()
}

func (p *Pool) createIdleMachine(ctx context.Context, id uint64, provision *idleProvision) error {
	creationCtx, cancelCreation := context.WithCancel(ctx)
	stopOnPoolShutdown := context.AfterFunc(p.ctx, cancelCreation)
	defer stopOnPoolShutdown()
	defer cancelCreation()
	startupCtx, cancelStartup := p.startupContext(creationCtx)
	defer cancelStartup()

	machine, err := p.provisionMachine(startupCtx)
	if err != nil {
		return err
	}
	if _, err := machine.ConnectToGuestAgent(startupCtx); err != nil {
		cleanupErr := p.destroyUnpublishedMachine(machine)
		return errors.Join(fmt.Errorf("guest readiness: %w", err), cleanupErr)
	}
	if err := startupCtx.Err(); err != nil {
		return errors.Join(err, p.destroyUnpublishedMachine(machine))
	}

	p.l.Lock()
	currentProvision := p.idleProvisioning[id]
	if p.ctx.Err() != nil || !p.isActive || startupCtx.Err() != nil ||
		provision.cancelled || currentProvision != provision ||
		provision.generation != p.scaleGeneration {
		p.l.Unlock()
		cause := startupCtx.Err()
		if cause == nil {
			cause = fmt.Errorf("idle provisioning is no longer desired")
		}
		return errors.Join(cause, p.destroyUnpublishedMachine(machine))
	}

	p.machinesMu.Lock()
	idle := 0
	for _, owned := range p.machines {
		if owned.Metadata().State == "idle" {
			idle++
		}
	}
	if idle >= p.GetReplicas() {
		p.machinesMu.Unlock()
		p.l.Unlock()
		return p.destroyUnpublishedMachine(machine)
	}
	delete(p.idleProvisioning, id)
	machine.SetState("idle", "")
	p.machines[machine.Name] = machine
	if err := startupCtx.Err(); err != nil || p.ctx.Err() != nil {
		delete(p.machines, machine.Name)
		machine.SetState("removing", "")
		p.machinesMu.Unlock()
		p.l.Unlock()
		if err == nil {
			err = p.ctx.Err()
		}
		return errors.Join(err, p.destroyUnpublishedMachine(machine))
	}
	p.watchMachineLocked(machine)
	p.machinesMu.Unlock()
	p.l.Unlock()

	p.refreshMetrics()
	p.logger.Info().Str("vm_id", machine.Name).Msg("Created ready idle Firecracker VM")
	return nil
}

func (p *Pool) destroyUnpublishedMachine(machine *Machine) error {
	if err := machine.Destroy(context.Background()); err != nil {
		p.markPoolCleanup(machine)
		p.machinesMu.Lock()
		p.machines[machine.Name] = machine
		p.machinesMu.Unlock()
		p.refreshMetrics()
		metricCleanupFailures.WithLabelValues(p.config.Name).Inc()
		return err
	}
	return nil
}

// markPoolCleanup records a teardown owned by the pool, rather than by an
// environment release through the executor.
func (p *Pool) markPoolCleanup(machine *Machine) {
	p.machinesMu.Lock()
	if p.poolCleanup == nil {
		p.poolCleanup = make(map[*Machine]struct{})
	}
	p.poolCleanup[machine] = struct{}{}
	p.machinesMu.Unlock()
}

func (p *Pool) destroyMachine(ctx context.Context, machine *Machine) error {
	p.machinesMu.Lock()
	if p.machines[machine.Name] == machine && machine.Metadata().State == "idle" {
		machine.SetState("removing", "")
		if p.poolCleanup == nil {
			p.poolCleanup = make(map[*Machine]struct{})
		}
		p.poolCleanup[machine] = struct{}{}
	}
	_, poolOwnedCleanup := p.poolCleanup[machine]
	p.machinesMu.Unlock()

	if err := machine.Destroy(ctx); err != nil {
		p.refreshMetrics()
		if poolOwnedCleanup {
			metricCleanupFailures.WithLabelValues(p.config.Name).Inc()
		}
		return err
	}
	p.machinesMu.Lock()
	if p.machines[machine.Name] == machine {
		delete(p.machines, machine.Name)
	}
	delete(p.poolCleanup, machine)
	p.machinesMu.Unlock()
	p.refreshMetrics()
	p.TriggerScale()
	return nil
}

func (p *Pool) deleteMachine(ctx context.Context) error {
	p.machinesMu.Lock()
	var target *Machine
	for _, machine := range p.machines {
		if machine.Metadata().State != "idle" {
			continue
		}
		target = machine
		machine.SetState("removing", "")
		if p.poolCleanup == nil {
			p.poolCleanup = make(map[*Machine]struct{})
		}
		p.poolCleanup[machine] = struct{}{}
		break
	}
	p.machinesMu.Unlock()
	if target == nil {
		return fmt.Errorf("no ready idle machines available to scale down")
	}
	p.refreshMetrics()
	return p.destroyMachine(ctx, target)
}

func (p *Pool) retryRemovals() {
	p.l.Lock()
	defer p.l.Unlock()
	if p.ctx.Err() != nil {
		return
	}
	machines, _ := p.ListMachines(p.ctx)
	for _, machine := range machines {
		if machine.Metadata().State != "removing" || !machine.cleanupQueued.CompareAndSwap(false, true) {
			continue
		}
		p.workWg.Add(1)
		go func(m *Machine) {
			defer p.workWg.Done()
			defer m.cleanupQueued.Store(false)
			if err := p.destroyMachine(context.Background(), m); err != nil {
				p.logger.Error().Err(err).Str("vm_id", m.Name).Msg("Retrying VM cleanup failed")
			}
		}(machine)
	}
}

func init() {
	_ = log.SetLevel("panic")
}
