package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ALameLlama/fireactions"
	"github.com/ALameLlama/fireactions/internal/executor"
	v1alpha "github.com/ALameLlama/fireactions/internal/forgejo/v1alpha"
	pluginv1alpha "github.com/ALameLlama/fireactions/proto/forgejo/plugin/v1alpha"
	serverv1 "github.com/ALameLlama/fireactions/proto/server/v1"
	"github.com/containerd/containerd"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	health "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

// Server represents the Fireactions server.
type Server struct {
	serverv1.UnimplementedServerServiceServer
	config        *Config
	pools         map[string]*Pool
	grpcServer    *grpc.Server
	metricsServer *http.Server
	containerd    *containerd.Client
	imageManager  *imageManager
	executor      *executor.Manager
	journal       *StateStore
	health        *health.Server
	l             *sync.Mutex
	logger        *zerolog.Logger
	nextCID       atomic.Uint32 // Global VSOCK CID counter (starts at 3)
	version       string        // Version info for GetVersion RPC
	commit        string
	date          string
}

// Opt is a functional option for Server.
type Opt func(s *Server)

// WithLogger sets the logger for the Server.
func WithLogger(logger *zerolog.Logger) Opt {
	f := func(s *Server) {
		s.logger = logger
	}

	return f
}

// New creates a new Server.
func New(config *Config, opts ...Opt) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	logger := zerolog.Nop()
	containerdClient, err := containerd.New(config.Containerd.Address,
		containerd.WithTimeout(5*time.Second), containerd.WithDefaultNamespace(config.Containerd.Namespace))
	if err != nil {
		return nil, fmt.Errorf("containerd: creating client: %w", err)
	}

	// Runner v13.2 pings active plugin streams every 30 seconds; allow arrival jitter.
	grpcServer := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime: 20 * time.Second,
	}))
	s := &Server{
		config: config, grpcServer: grpcServer, pools: make(map[string]*Pool),
		containerd: containerdClient, l: &sync.Mutex{}, logger: &logger,
		version: fireactions.Version, commit: fireactions.Commit, date: fireactions.Date,
	}
	s.nextCID.Store(2)
	for _, opt := range opts {
		opt(s)
	}
	s.imageManager = newImageManager(s.logger, containerdClient)

	profiles := make(map[string]struct{}, len(config.Pools))
	for _, pool := range config.Pools {
		profiles[pool.Name] = struct{}{}
	}
	manager, err := executor.NewManager(s, executor.Options{
		MaxLifetime:    config.Leases.MaxLifetime,
		CleanupGrace:   config.Leases.CleanupGrace,
		CleanupTimeout: min(config.Leases.CleanupGrace, 30*time.Second),
		ReadySpec: func(profile string) (executor.ReadySpec, error) {
			for _, pool := range config.Pools {
				if pool.Name == profile {
					return executor.ReadySpec{
						DefaultUser:       pool.DefaultUser,
						MaxTransferBytes:  config.Guest.MaxTransferBytes,
						MaxArchiveEntries: config.Guest.MaxArchiveEntries,
					}, nil
				}
			}
			return executor.ReadySpec{}, fmt.Errorf("unknown profile %q", profile)
		},
		Observer: &executor.Observer{
			Operation: func(profile string, operation executor.Operation, outcome executor.Outcome) {
				if _, ok := profiles[profile]; !ok {
					return
				}
				metricOperations.WithLabelValues(profile, string(operation), string(outcome)).Inc()
			},
			ActiveEntries: func(profile string, delta int) {
				if _, ok := profiles[profile]; !ok {
					return
				}
				metricActiveEnvironments.WithLabelValues(profile).Add(float64(delta))
			},
			CleanupFailure: func(profile string) {
				if _, ok := profiles[profile]; !ok {
					return
				}
				metricCleanupFailures.WithLabelValues(profile).Inc()
			},
			TTLExpiration: func(profile string) {
				if _, ok := profiles[profile]; !ok {
					return
				}
				s.logger.Warn().Str("profile", profile).Msg("Environment hard lease expired; forcing VM cleanup")
			},
		},
	})
	if err != nil {
		_ = containerdClient.Close()
		return nil, fmt.Errorf("create executor manager: %w", err)
	}
	s.executor = manager
	serverv1.RegisterServerServiceServer(grpcServer, s)
	pluginv1alpha.RegisterBackendPluginServer(grpcServer, v1alpha.New(manager, v1alpha.Options{
		Profiles: profiles, StartupTimeout: config.Guest.StartupTimeout,
	}))
	s.health = health.NewServer()
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	s.health.SetServingStatus("plugin.v1alpha.BackendPlugin", healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(grpcServer, s.health)

	if config.Metrics != nil && config.Metrics.Enabled {
		metricsHandler := http.NewServeMux()
		metricsHandler.Handle("/metrics", promhttp.Handler())
		s.metricsServer = &http.Server{
			Addr: config.Metrics.Address, Handler: metricsHandler,
			ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		}
	}
	return s, nil
}

// Run starts the server and blocks until the context is canceled.
func (s *Server) Run(ctx context.Context) error {
	var ownerRelease func()
	defer func() {
		if ownerRelease != nil {
			ownerRelease()
		}
	}()
	startedPools := make([]*Pool, 0, len(s.config.Pools))
	shutdownComplete := false
	defer func() {
		if !shutdownComplete {
			s.shutdown(startedPools)
		}
	}()
	owner, err := acquireOwnerLock(s.config.StateDir)
	if err != nil {
		return err
	}
	ownerRelease = func() { _ = owner.Close() }
	s.journal, err = NewStateStore(s.config, s.containerd)
	if err != nil {
		return fmt.Errorf("create durable VM journal: %w", err)
	}
	if err := s.journal.ReconcileStartup(ctx); err != nil {
		return fmt.Errorf("reconcile stale VM ownership before serving: %w", err)
	}
	listener, err := listenUnixSocket(s.config.SocketPath, s.config.SocketGroup)
	if err != nil {
		return err
	}
	defer listener.Close()
	for _, poolConfig := range s.config.Pools {
		pool, err := NewPool(s.logger, poolConfig, s.imageManager, s.containerd, &s.nextCID)
		if err != nil {
			return fmt.Errorf("creating pool: %w", err)
		}
		pool.configureRuntime(s.config.StateDir, s.config.Network.ResolverPath, s.config.Guest.StartupTimeout, executor.ReadySpec{
			DefaultUser: poolConfig.DefaultUser, MaxTransferBytes: s.config.Guest.MaxTransferBytes,
			MaxArchiveEntries: s.config.Guest.MaxArchiveEntries,
		}, s.journal)
		s.l.Lock()
		s.pools[poolConfig.Name] = pool
		s.l.Unlock()
		startedPools = append(startedPools, pool)
		go pool.Run()
	}

	var metricsListener net.Listener
	if s.metricsServer != nil {
		metricsListener, err = net.Listen("tcp", s.config.Metrics.Address)
		if err != nil {
			return fmt.Errorf("failed to start metrics server: %w", err)
		}
	}
	runErr := s.serve(ctx, listener, metricsListener, startedPools)
	shutdownComplete = true
	return runErr
}

func (s *Server) serve(ctx context.Context, listener, metricsListener net.Listener, startedPools []*Pool) error {
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	s.health.SetServingStatus("plugin.v1alpha.BackendPlugin", healthpb.HealthCheckResponse_SERVING)
	s.logger.Info().Str("socket_path", s.config.SocketPath).Msg("Serving Forgejo execution plugin")

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	serveErrors := make(chan error, 2)
	serveCount := 1
	go func() { serveErrors <- s.grpcServer.Serve(listener) }()
	if metricsListener != nil {
		serveCount++
		go func() { serveErrors <- s.metricsServer.Serve(metricsListener) }()
	}
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		ticker := time.NewTicker(s.config.Leases.ReapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				reconcileCtx, cancel := context.WithTimeout(runCtx, s.config.Leases.CleanupGrace)
				if err := s.reconcileOwnedVMs(reconcileCtx); err != nil && runCtx.Err() == nil {
					s.logger.Error().Err(err).Msg("Owned VM reconciliation failed; retaining incomplete cleanup")
				}
				cancel()
			}
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErrors:
		serveCount--
		if runErr == nil {
			runErr = fmt.Errorf("gRPC server stopped unexpectedly")
		}
	}
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	s.health.SetServingStatus("plugin.v1alpha.BackendPlugin", healthpb.HealthCheckResponse_NOT_SERVING)
	cancelRun()
	s.shutdown(startedPools)
	<-reconcileDone
	if s.metricsServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.metricsServer.Shutdown(shutdownCtx)
		shutdownCancel()
	}
	gracefulDone := make(chan struct{})
	go func() {
		s.grpcServer.GracefulStop()
		close(gracefulDone)
	}()
	select {
	case <-gracefulDone:
	case <-time.After(10 * time.Second):
		s.grpcServer.Stop()
		<-gracefulDone
	}
	for range serveCount {
		select {
		case serveErr := <-serveErrors:
			if runErr == nil && serveErr != nil && serveErr != http.ErrServerClosed && serveErr != grpc.ErrServerStopped {
				runErr = serveErr
			}
		case <-time.After(10 * time.Second):
			if runErr == nil {
				runErr = fmt.Errorf("server listener did not stop")
			}
			_ = listener.Close()
			if metricsListener != nil {
				_ = s.metricsServer.Close()
			}
			s.grpcServer.Stop()
		}
	}
	if runErr != nil && runErr != http.ErrServerClosed && runErr != grpc.ErrServerStopped {
		return runErr
	}
	return nil
}

// reconcileOwnedVMs converges daemon ownership with external journal cleanup.
// Registry VM IDs remain observable even if an exit watcher already removed the
// corresponding pool entry, so external cleanup cannot strand active gauges.
func (s *Server) reconcileOwnedVMs(ctx context.Context) error {
	reaped, result := s.journal.Reap(ctx)
	type ownedMachine struct {
		pool    *Pool
		machine *Machine
	}
	owned := make(map[string]ownedMachine)
	for _, pool := range s.poolSnapshot() {
		machines, _ := pool.ListMachines(ctx)
		for _, machine := range machines {
			owned[machine.Name] = ownedMachine{pool: pool, machine: machine}
		}
	}
	for _, vmID := range s.executor.VMIDs() {
		if _, ok := owned[vmID]; !ok {
			owned[vmID] = ownedMachine{}
		}
	}
	removed := make(map[string]struct{}, len(reaped))
	for _, vmID := range reaped {
		removed[vmID] = struct{}{}
	}
	var cleanup sync.WaitGroup
	var errorsMu sync.Mutex
	for vmID, entry := range owned {
		if err := ctx.Err(); err != nil {
			errorsMu.Lock()
			result = errors.Join(result, err)
			errorsMu.Unlock()
			break
		}
		_, needsCleanup := removed[vmID]
		if !needsCleanup {
			state, err := s.journal.State(vmID)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				errorsMu.Lock()
				result = errors.Join(result, fmt.Errorf("read live VM journal %s: %w", vmID, err))
				errorsMu.Unlock()
				continue
			}
			needsCleanup = errors.Is(err, os.ErrNotExist) || state == "removing"
		}
		if !needsCleanup {
			continue
		}
		cleanup.Add(1)
		go func(id string, entry ownedMachine) {
			defer cleanup.Done()
			err := ctx.Err()
			environmentID, _ := s.executor.EnvironmentForVM(id)
			if err == nil && environmentID != "" {
				err = s.executor.Remove(ctx, environmentID)
			}
			if entry.machine != nil {
				if environmentID == "" {
					entry.pool.markPoolCleanup(entry.machine)
				}
				if err == nil {
					if err = ctx.Err(); err == nil {
						err = entry.pool.destroyMachine(ctx, entry.machine)
					}
				}
				entry.pool.refreshMetrics()
			}
			if err != nil {
				errorsMu.Lock()
				result = errors.Join(result, fmt.Errorf("reconcile live VM %s: %w", id, err))
				errorsMu.Unlock()
			}
		}(vmID, entry)
	}
	cleanup.Wait()
	return errors.Join(result, s.executor.RetryRemovals(ctx))
}

func (s *Server) shutdown(pools []*Pool) {
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	s.health.SetServingStatus("plugin.v1alpha.BackendPlugin", healthpb.HealthCheckResponse_NOT_SERVING)
	closeCtx, cancel := context.WithTimeout(context.Background(), s.config.Leases.CleanupGrace)
	if s.executor != nil {
		_ = s.executor.Close(closeCtx)
	}
	cancel()
	for _, pool := range pools {
		pool.Stop()
	}
	_ = s.containerd.Close()
}

func (s *Server) poolSnapshot() map[string]*Pool {
	s.l.Lock()
	defer s.l.Unlock()
	pools := make(map[string]*Pool, len(s.pools))
	for name, pool := range s.pools {
		pools[name] = pool
	}
	return pools
}

func (s *Server) findPool(id string) (*Pool, error) {
	s.l.Lock()
	defer s.l.Unlock()

	pool, ok := s.pools[id]
	if !ok {
		return nil, fmt.Errorf("pool not found: %s", id)
	}

	return pool, nil
}

func (s *Server) findMachine(id string) (*Machine, error) {
	for _, pool := range s.poolSnapshot() {
		machine, err := pool.GetMachine(id)
		if err == nil {
			return machine, nil
		}
	}
	return nil, fmt.Errorf("machine not found: %s", id)
}
