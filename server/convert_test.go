package server

import (
	"context"
	"sync"
	"testing"
	"time"

	serverv1 "github.com/ALameLlama/fireactions/proto/server/v1"
	"github.com/firecracker-microvm/firecracker-go-sdk"
)

func TestConvertPoolUsesProfileImageAndCurrentTarget(t *testing.T) {
	pool := &Pool{
		config:       &PoolConfig{Name: "ubuntu-large", Image: "registry.example/guest@sha256:abc", Replicas: 9},
		machinesMu:   &sync.Mutex{},
		machines:     map[string]*Machine{"vm-1": {State: "idle"}, "vm-2": {State: "claimed"}},
		scaleTrigger: make(chan struct{}, 1),
		isActive:     true,
	}
	pool.SetReplicas(3)

	got := convertPoolToProto(context.Background(), pool)
	if got.Name != "ubuntu-large" || got.Image != pool.config.Image {
		t.Fatalf("profile identity/image lost: %v", got)
	}
	if got.CurrentReplicas != 1 || got.DesiredReplicas != 3 || got.Replicas != 3 {
		t.Fatalf("expected one clean idle replica and runtime target 3: %v", got)
	}
	if got.State != serverv1.PoolState_POOL_STATE_ACTIVE {
		t.Fatalf("expected active profile: %v", got.State)
	}
	pool.isActive = false
	if got := convertPoolToProto(context.Background(), pool); got.State != serverv1.PoolState_POOL_STATE_PAUSED {
		t.Fatalf("expected paused profile: %v", got.State)
	}
}

func TestConvertPoolKeepsReplicaFieldsConsistentDuringScale(t *testing.T) {
	pool := ownershipTestPool(t)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range 1000 {
				pool.SetReplicas(i % 2)
				got := convertPoolToProto(context.Background(), pool)
				if got.Replicas != got.DesiredReplicas {
					t.Errorf("pool target changed within one response: %v", got)
					return
				}
			}
		}()
	}
	workers.Wait()
}

func TestConvertMachineKeepsHostStateWhenAgentUnavailable(t *testing.T) {
	createdAt := time.Unix(1720000000, 0)
	for _, state := range []string{"provisioning", "idle", "claimed", "removing"} {
		t.Run(state, func(t *testing.T) {
			machine := &Machine{
				Machine:      &firecracker.Machine{},
				Name:         "ubuntu-vm-1",
				Pool:         "ubuntu",
				CreatedAt:    createdAt,
				State:        state,
				AgentVersion: "1.2.3",
			}
			if state == "claimed" || state == "removing" {
				machine.EnvironmentID = "owned-environment"
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			got := convertMachineToProto(ctx, machine)
			if got.State != state || got.EnvironmentId != machine.EnvironmentID {
				t.Fatalf("agent failure changed host-owned lifecycle: %v", got)
			}
			if got.AgentVersion != machine.AgentVersion {
				t.Fatalf("agent failure discarded known version: %v", got)
			}
			if got.ID != machine.Name || got.Pool != machine.Pool || !got.CreatedAt.AsTime().Equal(createdAt) {
				t.Fatalf("machine diagnostics lost: %v", got)
			}
		})
	}
}
