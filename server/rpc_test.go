package server

import (
	"context"
	serverv1 "github.com/hostinger/fireactions/proto/server/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sync"
	"testing"
)

func TestListPoolsReturnsSortedProfilesAndImages(t *testing.T) {
	s := &Server{
		l:     &sync.Mutex{},
		pools: make(map[string]*Pool),
	}
	for _, name := range []string{"z-large", "a-small"} {
		pool := &Pool{
			config:     &PoolConfig{Name: name, Image: "registry.example/" + name},
			machinesMu: &sync.Mutex{},
			machines:   make(map[string]*Machine),
			isActive:   true,
		}
		pool.replicas.Store(1)
		s.pools[name] = pool
	}

	response, err := s.ListPools(context.Background(), &serverv1.ListPoolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Pools) != 2 || response.Pools[0].Name != "a-small" || response.Pools[1].Name != "z-large" {
		t.Fatalf("profiles not sorted: %v", response.Pools)
	}
	for _, pool := range response.Pools {
		if pool.Image != "registry.example/"+pool.Name || pool.DesiredReplicas != 1 {
			t.Fatalf("profile diagnostics lost: %v", pool)
		}
	}
}

func TestScalePoolUpdatesRuntimeTarget(t *testing.T) {
	profile := "rpc-scale-profile"
	pool := &Pool{
		config:       &PoolConfig{Name: profile},
		scaleTrigger: make(chan struct{}, 1),
	}
	s := &Server{l: &sync.Mutex{}, pools: map[string]*Pool{profile: pool}}

	_, err := s.ScalePool(context.Background(), &serverv1.ScalePoolRequest{Name: profile, Replicas: 4})
	if err != nil {
		t.Fatal(err)
	}
	if pool.GetReplicas() != 4 {
		t.Fatalf("replica target was not updated: %d", pool.GetReplicas())
	}
}

func TestScalePoolRejectsNegativeReplicasWithoutChangingTarget(t *testing.T) {
	pool := &Pool{config: &PoolConfig{Name: "rpc-negative"}, scaleTrigger: make(chan struct{}, 1)}
	pool.replicas.Store(3)
	s := &Server{l: &sync.Mutex{}, pools: map[string]*Pool{"rpc-negative": pool}}

	_, err := s.ScalePool(context.Background(), &serverv1.ScalePoolRequest{Name: "rpc-negative", Replicas: -1})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if got := pool.GetReplicas(); got != 3 {
		t.Fatalf("negative request changed replica target to %d", got)
	}
}

func TestPoolMetricsReflectPublishedMachineStates(t *testing.T) {
	profile := "metrics-state-profile"
	pool := &Pool{
		config: &PoolConfig{Name: profile}, machinesMu: &sync.Mutex{},
		machines: map[string]*Machine{
			"idle":      {Name: "idle", State: "idle"},
			"claimed":   {Name: "claimed", State: "claimed"},
			"provision": {Name: "provision", State: "provisioning"},
			"removing":  {Name: "removing", State: "removing"},
		},
	}
	pool.refreshMetrics()
	if got := testutil.ToFloat64(metricCleanIdleVMs.WithLabelValues(profile)); got != 1 {
		t.Fatalf("clean idle VM gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metricClaimedVMs.WithLabelValues(profile)); got != 1 {
		t.Fatalf("claimed VM gauge = %v, want 1", got)
	}
}

func TestScalePoolUnknownProfileIsNotFound(t *testing.T) {
	s := &Server{l: &sync.Mutex{}, pools: make(map[string]*Pool)}
	_, err := s.ScalePool(context.Background(), &serverv1.ScalePoolRequest{Name: "missing", Replicas: 1})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}
