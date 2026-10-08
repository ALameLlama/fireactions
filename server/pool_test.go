package server

import (
	"context"
	"errors"
	"math"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ALameLlama/fireactions/internal/executor"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/rs/zerolog"
	"golang.org/x/sys/unix"
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

func TestNewPoolRejectsReplicaOverflowBeforeConversion(t *testing.T) {
	replicas := int64(math.MaxInt32) + 1
	if int64(int(replicas)) != replicas {
		t.Skip("int32 overflow is already outside the host int range")
	}
	logger := zerolog.Nop()
	pool, err := NewPool(&logger, &PoolConfig{Name: "overflow", Replicas: int(replicas)}, nil, nil, nil)
	if err == nil || pool != nil {
		t.Fatalf("initial replica overflow allocated a pool: pool=%v err=%v", pool, err)
	}
}

func TestFailedDurableWarmClaimNeverPublishesSelectedVM(t *testing.T) {
	for _, failure := range []string{"record fsync", "directory fsync"} {
		t.Run(failure, func(t *testing.T) {
			store := newTestStateStore(t)
			profile := "persist-claim"
			record := testStateRecord(store, profile, profile+"-vm", "idle", nil)
			cmd := startJournalProcess(t, record)
			pidfd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(pidfd)
			if err := store.create(record); err != nil {
				t.Fatal(err)
			}
			machine := &Machine{Name: record.VMID, State: "idle", resources: &ownedResources{journal: store, record: record}}
			pool := ownershipTestPool(t, machine)
			pool.config.Name, pool.journal = profile, store
			pool.Pause()
			persistErr := errors.New("persistent disk sync failure")
			syncFile, syncDir := store.syncFile, store.syncDir
			t.Cleanup(func() { store.syncFile, store.syncDir = syncFile, syncDir; pool.Stop() })
			if failure == "record fsync" {
				store.syncFile = func(*os.File) error { return persistErr }
			} else {
				store.syncDir = func(*os.File) error { return persistErr }
			}
			vm, err := pool.acquire(context.Background(), time.Now().Add(time.Hour))
			if vm != nil || !errors.Is(err, persistErr) {
				t.Fatalf("failed durable claim published vm=%v err=%v", vm, err)
			}
			if state := machine.Metadata().State; state != "removing" {
				t.Fatalf("failed durable claim left selected VM available: state=%q", state)
			}
			if pool.GetCurrentSize() != 0 {
				t.Fatal("failed durable claim returned the selected VM to idle inventory")
			}
			if exited, err := pidfdExited(pidfd, 0); err != nil || !exited {
				t.Fatalf("selected VMM survived failed durable publication: exited=%v err=%v", exited, err)
			}
			store.syncFile, store.syncDir = syncFile, syncDir
			pool.retryRemovals()
			pool.workWg.Wait()
			if _, err := pool.GetMachine(record.VMID); err == nil {
				t.Fatal("failed durable claim ownership remained after successful cleanup retry")
			}
			if _, err := store.State(record.VMID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup retry retained durable selected VM ownership: %v", err)
			}
		})
	}
}

func TestDurablyRemovingIdleVMCannotBeClaimed(t *testing.T) {
	store := newTestStateStore(t)
	profile := "revoked-claim"
	record := testStateRecord(store, profile, profile+"-vm", "removing", nil)
	if err := store.create(record); err != nil {
		t.Fatal(err)
	}
	machine := &Machine{Name: record.VMID, State: "idle", resources: &ownedResources{journal: store, record: record}}
	pool := ownershipTestPool(t, machine)
	pool.config.Name, pool.journal = profile, store
	pool.Pause()
	vm, err := pool.acquire(context.Background(), time.Now().Add(time.Hour))
	if vm != nil || err == nil {
		t.Fatalf("durably revoked idle VM was claimed: vm=%v err=%v", vm, err)
	}
	if _, err := pool.GetMachine(record.VMID); err == nil || pool.GetCurrentSize() != 0 {
		t.Fatal("durably revoked VM remained claimable after failed acquisition")
	}
	pool.Stop()
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

func TestScheduledReconciliationPreservesConcurrentDownscale(t *testing.T) {
	machine := &Machine{Name: "idle", State: "idle", resources: &ownedResources{}}
	pool := ownershipTestPool(t, machine)
	pool.SetReplicas(1)
	scheduled := make(chan int, 1)
	reconcile := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		// A reconciliation scheduled for the old inventory must not restore
		// that target after a control-plane update.
		scheduled <- pool.GetReplicas()
		<-reconcile
		finished <- pool.reconcileScale(context.Background())
	}()
	if got := <-scheduled; got != 1 {
		t.Fatalf("initial target = %d, want 1", got)
	}
	pool.SetReplicas(0)
	close(reconcile)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	pool.workWg.Wait()
	if got := pool.GetReplicas(); got != 0 {
		t.Fatalf("reconciliation overwrote downscale: target=%d", got)
	}
	if _, err := pool.GetMachine(machine.Name); err == nil {
		t.Fatal("reconciliation did not remove the superseded idle VM")
	}
}

func TestScaleChangesDesiredTargetWhilePaused(t *testing.T) {
	pool := ownershipTestPool(t)
	pool.Pause()
	if err := pool.Scale(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if got := pool.GetReplicas(); got != 2 {
		t.Fatalf("explicit scale did not update paused target: %d", got)
	}
	if pool.IsActive() || len(pool.idleProvisioning) != 0 {
		t.Fatal("explicit scale resumed provisioning in a paused pool")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pool.Scale(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled explicit scale returned %v", err)
	}
	if got := pool.GetReplicas(); got != 2 {
		t.Fatalf("canceled explicit scale changed target: %d", got)
	}
}

func TestPendingIdlePublicationHonorsPoolControls(t *testing.T) {
	for _, test := range []struct {
		name    string
		control func(*testing.T, *Pool)
		publish bool
	}{
		{name: "repeated active resume", control: func(_ *testing.T, p *Pool) { p.Resume(); p.Resume() }, publish: true},
		{name: "larger target", control: func(_ *testing.T, p *Pool) { p.SetReplicas(2) }, publish: true},
		{name: "pause", control: func(_ *testing.T, p *Pool) { p.Pause() }},
		{name: "pause then resume", control: func(_ *testing.T, p *Pool) { p.Pause(); p.Resume() }},
		{name: "downscale", control: func(_ *testing.T, p *Pool) { p.SetReplicas(0) }},
		{name: "explicit downscale", control: func(t *testing.T, p *Pool) {
			if err := p.Scale(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stop", control: func(_ *testing.T, p *Pool) { p.Stop() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := ownershipTestPool(t)
			t.Cleanup(pool.Stop)
			pool.Pause()
			pool.Resume()
			pool.SetReplicas(1)
			ctx, cancel := context.WithCancel(pool.ctx)
			defer cancel()
			provision := &idleProvision{cancel: cancel, generation: pool.scaleGeneration}
			pool.idleProvisioning[1] = provision

			// Readiness has completed, but publication has not. The SDK
			// watcher needs no running VMM and waits for pool cancellation.
			vmm, err := firecracker.NewMachine(pool.ctx, firecracker.Config{VMID: "pending-idle"})
			if err != nil {
				t.Fatal(err)
			}
			machine := &Machine{Machine: vmm, Name: "pending-idle", State: "provisioning", resources: &ownedResources{}}
			test.control(t, pool)
			err = pool.publishIdleMachine(ctx, machine, 1, provision)
			pool.finishIdleProvision(1, provision)
			if test.publish {
				if err != nil {
					t.Fatalf("desired ready VM was discarded: %v", err)
				}
				if pool.GetCurrentSize() != 1 {
					t.Fatal("desired ready VM was not added to idle inventory")
				}
				vm, err := pool.acquire(context.Background(), time.Now().Add(time.Hour))
				if err != nil || vm != machine {
					t.Fatalf("published VM was not claimable: vm=%v err=%v", vm, err)
				}
			} else {
				if err == nil {
					t.Fatal("revoked provisioning was admitted")
				}
				if _, err := pool.GetMachine(machine.Name); err == nil || pool.GetCurrentSize() != 0 {
					t.Fatal("revoked VM remained available in the pool")
				}
				if state := machine.Metadata().State; state != "removing" {
					t.Fatalf("revoked ready VM was not destroyed: state=%q", state)
				}
			}
		})
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
