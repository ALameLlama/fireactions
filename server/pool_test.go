package server

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
	"github.com/rs/zerolog"
)

func ownershipTestPool(t *testing.T, machines ...*Machine) *Pool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	pool := &Pool{
		config:           &PoolConfig{Name: "profile"},
		ctx:              ctx,
		cancel:           cancel,
		logger:           &logger,
		machinesMu:       &sync.Mutex{},
		machines:         make(map[string]*Machine),
		poolCleanup:      make(map[*Machine]struct{}),
		idleProvisioning: make(map[uint64]*idleProvision),
		scaleTrigger:     make(chan struct{}, 1),
		isActive:         true,
	}
	for _, machine := range machines {
		pool.machines[machine.Name] = machine
	}
	return pool
}

func TestScaleDownRetainsFailedCleanupAndReconcilesIt(t *testing.T) {
	calls := 0
	machine := &Machine{Name: "owned-vm", State: "idle", resources: &ownedResources{steps: []cleanupStep{
		{name: "stop", run: func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.New("stop failed")
			}
			return nil
		}},
	}}}
	pool := ownershipTestPool(t, machine)
	if err := pool.deleteMachine(context.Background()); err == nil {
		t.Fatal("failed stop was reported successful")
	}
	if got, err := pool.GetMachine(machine.Name); err != nil || got != machine {
		t.Fatalf("failed stop forgot live VM ownership: %v", err)
	}
	if machine.Metadata().State != "removing" {
		t.Fatal("failed cleanup VM remained available")
	}
	pool.retryRemovals()
	pool.workWg.Wait()
	if _, err := pool.GetMachine(machine.Name); err == nil || calls != 2 {
		t.Fatalf("reconciliation did not finish retained cleanup: calls=%d, err=%v", calls, err)
	}
}

func TestScaleDownNeverSelectsClaimedMachine(t *testing.T) {
	claimed := &Machine{Name: "claimed", State: "claimed", EnvironmentID: "environment", resources: &ownedResources{}}
	idle := &Machine{Name: "idle", State: "idle", resources: &ownedResources{}}
	pool := ownershipTestPool(t, claimed, idle)
	if err := pool.deleteMachine(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.GetMachine(idle.Name); err == nil {
		t.Fatal("idle VM was not removed")
	}
	if got, err := pool.GetMachine(claimed.Name); err != nil || got != claimed {
		t.Fatalf("scale-down terminated claimed environment: %v", err)
	}
	if err := pool.deleteMachine(context.Background()); err == nil {
		t.Fatal("scale-down claimed an already-owned machine")
	}
	if metadata := claimed.Metadata(); metadata.State != "claimed" || metadata.EnvironmentID != "environment" {
		t.Fatal("claimed VM lifecycle was changed by scale-down")
	}
}

func TestPoolRegistryIsNotLockedDuringDestruction(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	machine := &Machine{Name: "owned", State: "idle", resources: &ownedResources{steps: []cleanupStep{
		{name: "stop", run: func(context.Context) error {
			close(entered)
			<-release
			return nil
		}},
	}}}
	pool := ownershipTestPool(t, machine)
	finished := make(chan error, 1)
	go func() { finished <- pool.deleteMachine(context.Background()) }()
	<-entered
	read := make(chan error, 1)
	go func() {
		_, err := pool.GetMachine(machine.Name)
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("resource IO held the global pool registry lock")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestPoolPauseStateIsSynchronized(t *testing.T) {
	pool := ownershipTestPool(t)
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				pool.Pause()
				_ = pool.IsActive()
				pool.Resume()
			}
		}()
	}
	workers.Wait()
	pool.Pause()
	if pool.IsActive() {
		t.Fatal("pool did not remain paused")
	}
	if err := pool.Scale(context.Background(), -1); err == nil {
		t.Fatal("negative replica count accepted")
	}
}

func TestPoolStopRetainsIncompleteOwnership(t *testing.T) {
	machine := &Machine{Name: "failed-stop", State: "idle", resources: &ownedResources{steps: []cleanupStep{
		{name: "stop", run: func(context.Context) error { return errors.New("stop failed") }},
	}}}
	pool := ownershipTestPool(t, machine)
	pool.stopCh = make(chan struct{}, 1)
	pool.doneCh = make(chan struct{})
	close(pool.doneCh)
	pool.Stop()
	if got, err := pool.GetMachine(machine.Name); err != nil || got != machine {
		t.Fatalf("pool stop discarded incomplete resource ownership: %v", err)
	}
	if machine.Metadata().State != "removing" {
		t.Fatal("failed pool stop left a usable VM")
	}
}

func TestConcurrentAcquisitionClaimsIdleMachineOnce(t *testing.T) {
	machine := &Machine{Name: "idle", State: "idle", resources: &ownedResources{}}
	pool := ownershipTestPool(t, machine)
	pool.Pause()

	type result struct {
		machine executor.VM
		err     error
	}
	const callers = 24
	start := make(chan struct{})
	results := make(chan result, callers)
	for range callers {
		go func() {
			<-start
			vm, err := pool.acquire(context.Background(), time.Time{})
			results <- result{machine: vm, err: err}
		}()
	}
	close(start)

	claimed := 0
	for range callers {
		got := <-results
		if got.err == nil {
			if got.machine != machine {
				t.Fatalf("claimed unexpected VM: %v", got.machine)
			}
			claimed++
			continue
		}
		if executor.KindOf(got.err) != executor.Unavailable {
			t.Fatalf("empty paused pool returned %v, want Unavailable", got.err)
		}
	}
	if claimed != 1 {
		t.Fatalf("idle VM was claimed %d times, want exactly once", claimed)
	}
	if state := machine.Metadata().State; state != "claimed" {
		t.Fatalf("claimed VM state = %q, want claimed", state)
	}
	if got := pool.GetCurrentSize(); got != 0 {
		t.Fatalf("claimed VM counted as idle: size=%d", got)
	}
	pool.Stop()
}

func TestPausedPoolClaimsExistingIdleAndEmptyPoolIsUnavailable(t *testing.T) {
	machine := &Machine{Name: "idle", State: "idle", resources: &ownedResources{}}
	pool := ownershipTestPool(t, machine)
	pool.Pause()

	got, err := pool.acquire(context.Background(), time.Time{})
	if err != nil || got != machine {
		t.Fatalf("paused pool failed to claim its idle VM: vm=%v err=%v", got, err)
	}
	if _, err := pool.acquire(context.Background(), time.Time{}); executor.KindOf(err) != executor.Unavailable {
		t.Fatalf("empty paused pool error = %v, want Unavailable", err)
	}
	pool.Stop()
}

func TestCancelledIdleClaimDestroysSelectedMachine(t *testing.T) {
	machine := &Machine{Name: "idle", State: "idle", resources: &ownedResources{}}
	pool := ownershipTestPool(t, machine)
	pool.Pause()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hold metadata after admission so cancellation lands between selection
	// and the required post-claim check.
	machine.metadataMu.Lock()
	type result struct {
		vm  executor.VM
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		vm, err := pool.acquire(ctx, time.Time{})
		resultCh <- result{vm: vm, err: err}
	}()

	deadline := time.NewTimer(time.Second)
	enteredSelection := false
	for !enteredSelection {
		if pool.machinesMu.TryLock() {
			pool.machinesMu.Unlock()
		} else {
			enteredSelection = true
			break
		}
		select {
		case <-deadline.C:
			cancel()
			machine.metadataMu.Unlock()
			<-resultCh
			t.Fatal("acquisition did not reach idle selection")
		default:
			runtime.Gosched()
		}
	}
	deadline.Stop()
	cancel()
	machine.metadataMu.Unlock()

	got := <-resultCh
	if !errors.Is(got.err, context.Canceled) || got.vm != nil {
		t.Fatalf("canceled acquisition returned vm=%v err=%v", got.vm, got.err)
	}
	if _, err := pool.GetMachine(machine.Name); err == nil {
		t.Fatal("canceled claim returned the selected VM to the pool")
	}
	if state := machine.Metadata().State; state != "removing" {
		t.Fatalf("canceled VM state = %q, want removing", state)
	}
	pool.Stop()
}

func TestClaimAndScaleDownSelectIdleAtomically(t *testing.T) {
	machine := &Machine{Name: "idle", State: "idle", resources: &ownedResources{}}
	pool := ownershipTestPool(t, machine)
	pool.Pause()
	start := make(chan struct{})
	type acquireResult struct {
		vm  executor.VM
		err error
	}
	acquired := make(chan acquireResult, 1)
	removed := make(chan error, 1)
	go func() {
		<-start
		vm, err := pool.acquire(context.Background(), time.Time{})
		acquired <- acquireResult{vm: vm, err: err}
	}()
	go func() {
		<-start
		removed <- pool.deleteMachine(context.Background())
	}()
	close(start)

	got := <-acquired
	deleteErr := <-removed
	if got.err == nil {
		if got.vm != machine || deleteErr == nil {
			t.Fatalf("claimed VM was removed: acquire=%v delete=%v", got.err, deleteErr)
		}
		if current, err := pool.GetMachine(machine.Name); err != nil || current != machine {
			t.Fatalf("claimed VM ownership was lost: %v", err)
		}
		if state := machine.Metadata().State; state != "claimed" {
			t.Fatalf("VM state after claim/downscale race = %q, want claimed", state)
		}
	} else {
		if executor.KindOf(got.err) != executor.Unavailable || deleteErr != nil {
			t.Fatalf("downscale/claim race had unexpected outcome: acquire=%v delete=%v", got.err, deleteErr)
		}
		if _, err := pool.GetMachine(machine.Name); err == nil {
			t.Fatal("downscaled VM remained owned")
		}
	}
	pool.Stop()
}

func TestCurrentSizeCountsReadyIdleOnly(t *testing.T) {
	pool := ownershipTestPool(t,
		&Machine{Name: "idle", State: "idle"},
		&Machine{Name: "claimed", State: "claimed"},
		&Machine{Name: "provisioning", State: "provisioning"},
		&Machine{Name: "removing", State: "removing"},
	)
	if got := pool.GetCurrentSize(); got != 1 {
		t.Fatalf("current size = %d, want only the one idle VM", got)
	}
	machines, err := pool.ListMachines(context.Background())
	if err != nil || len(machines) != 4 {
		t.Fatalf("ListMachines lost owned records: count=%d err=%v", len(machines), err)
	}
}

func TestStopSynchronizesAcquireAdmission(t *testing.T) {
	machine := &Machine{Name: "idle", State: "idle", resources: &ownedResources{}}
	pool := ownershipTestPool(t, machine)
	pool.Pause()
	pool.l.Lock()
	started := make(chan struct{})
	acquired := make(chan error, 1)
	go func() {
		close(started)
		_, err := pool.acquire(context.Background(), time.Time{})
		acquired <- err
	}()
	<-started

	stopped := make(chan struct{})
	go func() {
		pool.Stop()
		close(stopped)
	}()
	<-pool.ctx.Done()
	pool.l.Unlock()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not settle pool work")
	}
	if err := <-acquired; !errors.Is(err, context.Canceled) {
		t.Fatalf("acquisition admitted across Stop barrier: %v", err)
	}
	if _, err := pool.GetMachine(machine.Name); err == nil {
		t.Fatal("Stop left the idle VM registered")
	}
}

func TestStopWaitsForAdmittedAcquisitionCleanup(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	machine := &Machine{Name: "idle", State: "idle", resources: &ownedResources{steps: []cleanupStep{
		{name: "stop", run: func(context.Context) error {
			close(entered)
			<-release
			return nil
		}},
	}}}
	pool := ownershipTestPool(t, machine)
	pool.Pause()
	acquireCtx, cancelAcquire := context.WithCancel(context.Background())
	defer cancelAcquire()
	machine.metadataMu.Lock()

	type result struct {
		vm  executor.VM
		err error
	}
	acquired := make(chan result, 1)
	go func() {
		vm, err := pool.acquire(acquireCtx, time.Time{})
		acquired <- result{vm: vm, err: err}
	}()
	deadline := time.NewTimer(time.Second)
	for pool.machinesMu.TryLock() {
		pool.machinesMu.Unlock()
		select {
		case <-deadline.C:
			machine.metadataMu.Unlock()
			close(release)
			<-acquired
			pool.Stop()
			t.Fatal("acquisition did not reach idle selection")
		default:
			runtime.Gosched()
		}
	}
	deadline.Stop()

	stopped := make(chan struct{})
	go func() {
		pool.Stop()
		close(stopped)
	}()
	<-pool.ctx.Done()
	cancelAcquire()
	machine.metadataMu.Unlock()

	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		<-acquired
		<-stopped
		t.Fatal("shutdown did not roll back the admitted claim")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned before acquisition cleanup settled")
	default:
	}
	close(release)
	got := <-acquired
	if got.vm != nil || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("shutdown acquisition returned vm=%v err=%v", got.vm, got.err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish after acquisition cleanup settled")
	}
}
