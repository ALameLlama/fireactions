package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/hostinger/fireactions/internal/executor"
	"github.com/hostinger/fireactions/internal/guest"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
)

func journalRuntimeTestServer(t *testing.T, profile string) (*Server, *Pool, *Machine, *stateRecord, string) {
	t.Helper()
	store := newTestStateStore(t)
	record := testStateRecord(store, profile, profile+"-vm", "idle", nil)
	if err := store.create(record); err != nil {
		t.Fatal(err)
	}
	layout, err := executor.DefaultLayout("amd64")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("passthrough:test", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	// External destruction makes guest cooperation unavailable. A closed real
	// client exercises definitive host cleanup without any guest-server mock.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	machine := &Machine{
		Name: record.VMID, Pool: profile, State: "idle", info: executor.VMInfo{Layout: layout},
		guestConn: conn, guestClient: guest.New(conn),
		resources: &ownedResources{journal: store, record: record},
	}
	pool := ownershipTestPool(t, machine)
	pool.config.Name = profile
	pool.journal = store
	pool.Pause()
	logger := zerolog.Nop()
	s := &Server{journal: store, pools: map[string]*Pool{profile: pool}, l: &sync.Mutex{}, logger: &logger}
	manager, err := executor.NewManager(s, executor.Options{
		MaxLifetime: time.Hour, CleanupTimeout: time.Second, CleanupGrace: time.Second,
		Observer: &executor.Observer{ActiveEntries: func(profile string, delta int) {
			metricActiveEnvironments.WithLabelValues(profile).Add(float64(delta))
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.executor = manager
	environment, err := manager.Create(context.Background(), executor.CreateSpec{Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close(context.Background())
		pool.Stop()
		metricActiveEnvironments.DeleteLabelValues(profile)
		metricClaimedVMs.DeleteLabelValues(profile)
		metricCleanIdleVMs.DeleteLabelValues(profile)
	})
	return s, pool, machine, record, environment.ID
}

func TestExternalJournalCleanupRemovesCanonicalEnvironmentAndGauges(t *testing.T) {
	for _, exitWatcherAlreadyRemoved := range []bool{false, true} {
		name := "present-pool-entry"
		if exitWatcherAlreadyRemoved {
			name = "exit-watcher-removed-pool-entry"
		}
		t.Run(name, func(t *testing.T) {
			profile := "reconcile-" + name
			s, pool, machine, record, environmentID := journalRuntimeTestServer(t, profile)
			marker := filepath.Join(t.TempDir(), "owned-resource")
			if err := os.WriteFile(marker, []byte("owned"), 0600); err != nil {
				t.Fatal(err)
			}
			s.journal.cleanup = func(context.Context, *stateRecord, bool) error { return os.Remove(marker) }
			if exitWatcherAlreadyRemoved {
				if err := pool.destroyMachine(context.Background(), machine); err != nil {
					t.Fatal(err)
				}
			} else if err := s.journal.destroyRecord(context.Background(), record.VMID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("external cleanup left the owned resource: %v", err)
			}
			if got := testutil.ToFloat64(metricActiveEnvironments.WithLabelValues(profile)); got != 1 {
				t.Fatalf("external cleanup bypassed canonical environment ownership: active=%v", got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.reconcileOwnedVMs(ctx); err != nil {
				t.Fatal(err)
			}
			if id, _ := s.executor.EnvironmentForVM(record.VMID); id != "" {
				t.Fatalf("externally removed VM still belongs to environment %s (wanted removal of %s)", id, environmentID)
			}
			if _, err := pool.GetMachine(record.VMID); err == nil {
				t.Fatal("externally removed VM remained available in the pool")
			}
			for name, gauge := range map[string]float64{
				"active":  testutil.ToFloat64(metricActiveEnvironments.WithLabelValues(profile)),
				"claimed": testutil.ToFloat64(metricClaimedVMs.WithLabelValues(profile)),
				"idle":    testutil.ToFloat64(metricCleanIdleVMs.WithLabelValues(profile)),
			} {
				if gauge != 0 {
					t.Fatalf("external cleanup stranded %s gauge at %v", name, gauge)
				}
			}
			if err := s.reconcileOwnedVMs(ctx); err != nil {
				t.Fatal(err)
			}
			if got := testutil.ToFloat64(metricActiveEnvironments.WithLabelValues(profile)); got != 0 {
				t.Fatalf("repeated reconciliation double-decremented active gauge: %v", got)
			}
		})
	}
}

func TestExternalRemovingRecordRetriesCanonicalEnvironmentCleanup(t *testing.T) {
	profile := "reconcile-retained"
	s, pool, machine, record, environmentID := journalRuntimeTestServer(t, profile)
	if err := s.journal.update(record.VMID, func(r *stateRecord) error { r.State = "removing"; return nil }); err != nil {
		t.Fatal(err)
	}
	incomplete := errors.New("owned resource cleanup unavailable")
	cleanup := s.journal.cleanup
	s.journal.cleanup = func(context.Context, *stateRecord, bool) error { return incomplete }
	t.Cleanup(func() { s.journal.cleanup = cleanup })
	if err := s.reconcileOwnedVMs(context.Background()); !errors.Is(err, incomplete) {
		t.Fatalf("incomplete external cleanup was reported successful: %v", err)
	}
	if id, removing := s.executor.EnvironmentForVM(record.VMID); id != environmentID || !removing {
		t.Fatalf("incomplete cleanup forgot canonical ownership: id=%q removing=%v", id, removing)
	}
	if got, err := pool.GetMachine(record.VMID); err != nil || got != machine {
		t.Fatalf("incomplete cleanup forgot pool ownership: machine=%v err=%v", got, err)
	}
	if state, err := s.journal.State(record.VMID); err != nil || state != "removing" {
		t.Fatalf("incomplete cleanup lost retryable durable state: state=%q err=%v", state, err)
	}
	if got := testutil.ToFloat64(metricActiveEnvironments.WithLabelValues(profile)); got != 1 {
		t.Fatalf("incomplete owned cleanup changed active ownership to %v", got)
	}
	if got := testutil.ToFloat64(metricClaimedVMs.WithLabelValues(profile)); got != 0 {
		t.Fatalf("removing environment remained claimed: %v", got)
	}
	s.journal.cleanup = cleanup
	if err := s.reconcileOwnedVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.executor.EnvironmentForVM(record.VMID); id != "" {
		t.Fatalf("successful recovery retained environment %s", id)
	}
	if got := testutil.ToFloat64(metricActiveEnvironments.WithLabelValues(profile)); got != 0 {
		t.Fatalf("successful cleanup retry stranded active ownership at %v", got)
	}
}

func TestCorruptLiveRecordIsNotMistakenForExternalCleanup(t *testing.T) {
	profile := "reconcile-corrupt"
	s, pool, machine, record, environmentID := journalRuntimeTestServer(t, profile)
	cleanup := s.journal.cleanup
	cleanupCalled := false
	s.journal.cleanup = func(context.Context, *stateRecord, bool) error {
		cleanupCalled = true
		return errors.New("corrupt ownership must not authorize cleanup")
	}
	t.Cleanup(func() {
		s.journal.cleanup = cleanup
		if err := s.journal.writeRecord(record); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(s.journal.recordPath(record.VMID), []byte(`{"version":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileOwnedVMs(context.Background()); err == nil {
		t.Fatal("corrupt live ownership was silently treated as successful cleanup")
	}
	if cleanupCalled {
		t.Fatal("corrupt ownership triggered resource deletion")
	}
	if id, removing := s.executor.EnvironmentForVM(record.VMID); id != environmentID || removing {
		t.Fatalf("corrupt evidence removed the healthy live environment: id=%q removing=%v", id, removing)
	}
	if got, err := pool.GetMachine(record.VMID); err != nil || got != machine || machine.Metadata().State != "claimed" {
		t.Fatalf("corrupt evidence removed live pool ownership: machine=%v err=%v", got, err)
	}
	if got := testutil.ToFloat64(metricActiveEnvironments.WithLabelValues(profile)); got != 1 {
		t.Fatalf("corrupt evidence incorrectly decremented active ownership: %v", got)
	}
}

func TestExternalRemovingRecordRevokesIdleAvailability(t *testing.T) {
	store := newTestStateStore(t)
	profile := "reconcile-idle"
	record := testStateRecord(store, profile, profile+"-vm", "idle", nil)
	if err := store.create(record); err != nil {
		t.Fatal(err)
	}
	machine := &Machine{Name: record.VMID, State: "idle", resources: &ownedResources{journal: store, record: record}}
	pool := ownershipTestPool(t, machine)
	pool.config.Name, pool.journal = profile, store
	pool.Pause()
	pool.refreshMetrics()
	if err := store.update(record.VMID, func(r *stateRecord) error { r.State = "removing"; return nil }); err != nil {
		t.Fatal(err)
	}
	manager, err := executor.NewManager(&Server{}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{journal: store, pools: map[string]*Pool{profile: pool}, l: &sync.Mutex{}, executor: manager}
	if err := s.reconcileOwnedVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pool.GetCurrentSize() != 0 {
		t.Fatal("externally removing idle VM remained claimable")
	}
	if _, err := pool.acquire(context.Background(), time.Now().Add(time.Hour)); executor.KindOf(err) != executor.Unavailable {
		t.Fatalf("externally removed idle VM was acquired: %v", err)
	}
	if got := testutil.ToFloat64(metricCleanIdleVMs.WithLabelValues(profile)); got != 0 {
		t.Fatalf("external idle cleanup stranded idle gauge at %v", got)
	}
	pool.Stop()
	metricCleanIdleVMs.DeleteLabelValues(profile)
	metricClaimedVMs.DeleteLabelValues(profile)
}

func TestServerListenerFailureReturnsPromptlyAndPreservesError(t *testing.T) {
	for _, failure := range []string{"grpc only", "grpc with metrics", "metrics with grpc"} {
		t.Run(failure, func(t *testing.T) {
			listener, err := net.Listen("unix", filepath.Join(socketTestDir(t), "plugin.sock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			containerdPath := filepath.Join(socketTestDir(t), "containerd.sock")
			containerdListener, err := net.Listen("unix", containerdPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = containerdListener.Close() })
			// Client construction needs a real gRPC transport, but this test
			// only closes the client and never calls a containerd API.
			containerdServer := grpc.NewServer()
			containerdDone := make(chan error, 1)
			go func() { containerdDone <- containerdServer.Serve(containerdListener) }()
			t.Cleanup(func() {
				containerdServer.Stop()
				if err := <-containerdDone; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
					t.Error(err)
				}
			})
			client, err := containerd.New(containerdPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			config := DefaultConfig()
			config.Leases.ReapInterval = time.Hour
			logger := zerolog.Nop()
			s := &Server{
				config: config, grpcServer: grpc.NewServer(), containerd: client,
				health: health.NewServer(), logger: &logger,
			}
			t.Cleanup(s.grpcServer.Stop)
			var metricsListener net.Listener
			if failure != "grpc only" {
				metricsListener, err = net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = metricsListener.Close() })
				s.metricsServer = &http.Server{Handler: http.NewServeMux()}
				t.Cleanup(func() { _ = s.metricsServer.Close() })
			}
			failedListener := listener
			if failure == "metrics with grpc" {
				failedListener = metricsListener
			}
			if err := failedListener.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- s.serve(ctx, listener, metricsListener, nil) }()
			select {
			case err := <-result:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("listener failure was lost or replaced: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("listener failure did not shut down promptly")
			}
		})
	}
}
