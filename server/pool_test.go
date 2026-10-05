package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func ownershipTestPool(t *testing.T, machines ...*Machine) *Pool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	pool := &Pool{
		config: &PoolConfig{Name: "profile"},
		ctx:    ctx, cancel: cancel, logger: &logger,
		machinesMu: &sync.Mutex{}, machines: make(map[string]*Machine),
		scaleTrigger: make(chan struct{}, 1), isActive: true,
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
