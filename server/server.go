package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd"
	"github.com/hostinger/fireactions"
	"github.com/hostinger/fireactions/internal/executor"
	v1alpha "github.com/hostinger/fireactions/internal/forgejo/v1alpha"
	pluginv1alpha "github.com/hostinger/fireactions/proto/forgejo/plugin/v1alpha"
	serverv1 "github.com/hostinger/fireactions/proto/server/v1"
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

	// Runner v13.2 pings active plugin streams every 30 seconds.
	grpcServer := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime: 30 * time.Second,
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

	if config.Metrics.Enabled {
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
		})
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
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	s.health.SetServingStatus("plugin.v1alpha.BackendPlugin", healthpb.HealthCheckResponse_SERVING)
	metricUp.Set(1)
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
				_ = s.executor.RetryRemovals(reconcileCtx)
				cancel()
			}
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErrors:
		if runErr == nil {
			runErr = fmt.Errorf("gRPC server stopped unexpectedly")
		}
	}
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	s.health.SetServingStatus("plugin.v1alpha.BackendPlugin", healthpb.HealthCheckResponse_NOT_SERVING)
	cancelRun()
	s.shutdown(startedPools)
	shutdownComplete = true
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
			runErr = fmt.Errorf("server listener did not stop")
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
