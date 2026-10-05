package server

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/log"
	"github.com/rs/zerolog"
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
	workWg         sync.WaitGroup
	stopOnce       sync.Once
	ctx            context.Context
	cancel         context.CancelFunc
	nextCID        *atomic.Uint32
	l              sync.Mutex
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

	if err := os.MkdirAll(p.GetDir(), 0700); err != nil {
		cancel()
		return nil, fmt.Errorf("creating pool directory: %w", err)
	}
	if err := os.Chmod(p.GetDir(), 0700); err != nil {
		cancel()
		return nil, fmt.Errorf("protecting pool directory: %w", err)
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
		p.retryRemovals()
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
		select {
		case <-p.doneCh:
		case <-time.After(5 * time.Second):
			p.logger.Warn().Msg("Timeout waiting for pool loop to exit")
		}
		// Synchronize with Scale's scheduling before waiting for work. No new
		// WaitGroup Add can occur after cancellation and this lock barrier.
		p.l.Lock()
		p.l.Unlock()
		p.workWg.Wait()
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
	return fmt.Sprintf("/var/lib/fireactions/pools/%s", p.config.Name)
}

// Scale scales the pool to the desired size.
func (p *Pool) Scale(ctx context.Context, desiredReplicas int) error {
	p.l.Lock()
	defer p.l.Unlock()
	if desiredReplicas < 0 {
		return fmt.Errorf("replica count must not be negative")
	}

	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	default:
	}
	if !p.isActive {
		return nil
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

	for range count {
		p.pendingCreates.Add(1)
		p.workWg.Add(1)

		go func() {
			defer p.pendingCreates.Add(-1)
			defer p.workWg.Done()

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
	p.machinesMu.Lock()
	available := 0
	for _, machine := range p.machines {
		state := machine.Metadata().State
		if state != "claimed" && state != "removing" {
			available++
		}
	}
	p.machinesMu.Unlock()
	if count > available {
		count = available
	}

	p.logger.Debug().Msgf("Scaling down by %d VMs (target: %d, current: %d, pending creates: %d, pending deletes: %d)",
		count, desiredReplicas, curSize, pendingCreates, pendingDeletes)

	for range count {
		p.pendingDeletes.Add(1)
		p.workWg.Add(1)

		go func() {
			defer p.pendingDeletes.Add(-1)
			defer p.workWg.Done()

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

// IsActive reads pause state under the same lock used by scaling.
func (p *Pool) IsActive() bool {
	p.l.Lock()
	defer p.l.Unlock()
	return p.isActive
}

func (p *Pool) Pause() {
	p.l.Lock()
	p.isActive = false
	p.l.Unlock()
}

func (p *Pool) Resume() {
	p.l.Lock()
	p.isActive = true
	p.l.Unlock()
	p.TriggerScale()
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
	creationCtx, cancel := context.WithCancel(ctx)
	stopOnPoolShutdown := context.AfterFunc(p.ctx, cancel)
	defer stopOnPoolShutdown()
	defer cancel()
	machine, err := p.provisionMachine(creationCtx)
	if err != nil {
		return err
	}
	if err := creationCtx.Err(); err != nil {
		if cleanupErr := machine.Destroy(context.Background()); cleanupErr != nil {
			p.machinesMu.Lock()
			p.machines[machine.Name] = machine
			p.machinesMu.Unlock()
			return fmt.Errorf("creation cancelled; cleaning up provisioned VM: %w", cleanupErr)
		}
		return err
	}
	p.machinesMu.Lock()
	p.machines[machine.Name] = machine
	p.machinesMu.Unlock()
	p.cleanupWg.Add(1)
	go func() {
		defer p.cleanupWg.Done()
		_ = machine.Wait(p.ctx)
		if err := p.destroyMachine(context.Background(), machine); err != nil {
			p.logger.Error().Err(err).Str("vm_id", machine.Name).Msg("Failed to clean up exited VM")
		}
	}()
	p.logger.Info().Str("vm_id", machine.Name).Msg("Created Firecracker VM")
	return nil
}

func (p *Pool) destroyMachine(ctx context.Context, machine *Machine) error {
	if err := machine.Destroy(ctx); err != nil {
		return err
	}
	p.machinesMu.Lock()
	if p.machines[machine.Name] == machine {
		delete(p.machines, machine.Name)
	}
	p.machinesMu.Unlock()
	p.TriggerScale()
	return nil
}

func (p *Pool) deleteMachine(ctx context.Context) error {
	p.machinesMu.Lock()
	var target *Machine
	for _, machine := range p.machines {
		state := machine.Metadata().State
		if state == "removing" || state == "claimed" {
			continue
		}
		target = machine
		machine.SetState("removing", "")
		break
	}
	p.machinesMu.Unlock()
	if target == nil {
		return fmt.Errorf("no unclaimed machines available to scale down")
	}
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
