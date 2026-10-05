package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/hostinger/fireactions/internal/guestfs"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"github.com/rs/zerolog"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

const (
	logFilePath = "/var/log/fireactions-agent.log"
)

type readySettings struct {
	identity          Identity
	directories       []string
	maxTransferBytes  int64
	maxArchiveEntries int
	ready             bool
}

type Agent struct {
	agentv1.UnimplementedAgentServiceServer
	cfg           Config
	fs            *guestfs.RootFS
	processes     *execManager
	readyMu       sync.RWMutex
	readySettings readySettings
	closeOnce     sync.Once
	closeErr      error
	logFile       string
	logFileWriter *os.File
	logger        *zerolog.Logger
}

type Opt func(a *Agent)

func New(cfg Config, opts ...Opt) (*Agent, error) {
	cfg = cfg.withDefaults()
	if cfgErr := cfg.Validate(); cfgErr != nil {
		return nil, fmt.Errorf("validate config: %w", cfgErr)
	}

	root, err := guestfs.OpenRoot(cfg.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	a := &Agent{
		cfg:     cfg,
		fs:      root,
		logFile: logFilePath,
	}

	for _, opt := range opts {
		opt(a)
	}

	a.processes = newExecManager(a)

	if err := a.setupLogger(); err != nil {
		_ = root.Close()
		return nil, err
	}

	return a, nil
}

func (a *Agent) setupLogger() error {
	logDir := filepath.Dir(a.logFile)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	logFileWriter, err := os.OpenFile(a.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	a.logFileWriter = logFileWriter

	logLevel, err := zerolog.ParseLevel(a.cfg.LogLevel)
	if err != nil {
		logFileWriter.Close()
		a.logFileWriter = nil
		return fmt.Errorf("parse log level: %w", err)
	}

	multiWriter := io.MultiWriter(os.Stdout, logFileWriter)
	logger := zerolog.New(zerolog.ConsoleWriter{Out: multiWriter, TimeFormat: time.RFC3339}).With().
		Timestamp().
		Logger().Level(logLevel)

	a.logger = &logger
	return nil
}

// Close closes the agent resources, including the log file and workspace root.
func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		if a.processes != nil {
			if err := a.processes.shutdown(); err != nil {
				a.closeErr = errors.Join(a.closeErr, err)
			}
		}
		if a.logFileWriter != nil {
			if err := a.logFileWriter.Close(); err != nil {
				a.closeErr = errors.Join(a.closeErr, err)
			}
			a.logFileWriter = nil
		}
		if err := a.fs.Close(); err != nil {
			a.closeErr = errors.Join(a.closeErr, err)
		}
	})
	return a.closeErr
}

func (a *Agent) Run(ctx context.Context) error {
	defer a.Close()
	return a.runGRPCServer(ctx)
}

func (a *Agent) runGRPCServer(ctx context.Context) error {
	logrusLogger := logrus.New()
	logrusLogger.SetLevel(logrus.InfoLevel)
	logrusEntry := logrus.NewEntry(logrusLogger)

	listener, err := vsock.Listener(ctx, logrusEntry, a.cfg.Port)
	if err != nil {
		return fmt.Errorf("vsock listen: %w", err)
	}
	defer listener.Close()

	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcServer, a)

	errCh := make(chan error, 1)
	go func() {
		a.logger.Info().Msgf("Agent GRPC server listening on VSOCK port %d", a.cfg.Port)
		errCh <- grpcServer.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-stopped:
		case <-timer.C:
			grpcServer.Stop()
			<-stopped
		}
		return nil
	case err := <-errCh:
		grpcServer.Stop()
		if err != nil {
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	}
}
