package v1alpha

import (
	"context"
	"errors"
	"io"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
	pluginv1alpha "github.com/hostinger/fireactions/proto/forgejo/plugin/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	backendName      = "firecracker"
	copyInMaxChunk   = 1 << 20
	copyOutChunkSize = 32 << 10
)

type Options struct {
	Profiles       map[string]struct{}
	StartupTimeout time.Duration
}

type Server struct {
	pluginv1alpha.UnimplementedBackendPluginServer
	manager        *executor.Manager
	profiles       map[string]struct{}
	startupTimeout time.Duration
}

func New(manager *executor.Manager, options Options) *Server {
	profiles := make(map[string]struct{}, len(options.Profiles))
	for profile := range options.Profiles {
		profiles[profile] = struct{}{}
	}
	startup := options.StartupTimeout
	if startup <= 0 {
		startup = 2 * time.Minute
	}
	return &Server{manager: manager, profiles: profiles, startupTimeout: startup}
}

func (s *Server) Capabilities(context.Context, *pluginv1alpha.CapabilitiesRequest) (*pluginv1alpha.CapabilitiesResponse, error) {
	return &pluginv1alpha.CapabilitiesResponse{Name: backendName}, nil
}

func (s *Server) Create(ctx context.Context, req *pluginv1alpha.CreateRequest) (*pluginv1alpha.CreateResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "create request is required")
	}
	if len(req.Services) != 0 {
		return nil, status.Error(codes.Unimplemented, "service containers are not supported")
	}
	profile, err := s.selectProfile(req)
	if err != nil {
		return nil, err
	}
	lifetime, err := requestedLifetime(req.EnvironmentTimeout)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid environment timeout")
	}
	info, err := s.manager.Create(ctx, executor.CreateSpec{Profile: profile, Lifetime: lifetime})
	if err != nil {
		return nil, grpcError(err)
	}
	if err := ctx.Err(); err != nil {
		_ = s.manager.Remove(context.Background(), info.ID)
		return nil, grpcError(err)
	}
	layout := info.Layout
	return &pluginv1alpha.CreateResponse{
		EnvironmentId: info.ID, RootPath: layout.Root, ActPath: layout.Act,
		ToolCachePath: layout.ToolCache, TempPath: layout.Temp,
		PathVariableName: new("PATH"), DefaultPathVariable: new(layout.DefaultPath),
		PathSeparator: new(":"), Os: layout.OS, Arch: layout.Arch,
	}, nil
}

func (s *Server) selectProfile(req *pluginv1alpha.CreateRequest) (string, error) {
	for key := range req.BackendOptions {
		if key != "profile" {
			return "", status.Errorf(codes.InvalidArgument, "unknown backend option %q", key)
		}
	}
	profile := req.Image
	if profile == "" {
		profile = req.LabelArg
	}
	if profile == "" {
		profile = req.BackendOptions["profile"]
	}
	if profile == "" {
		return "", status.Error(codes.InvalidArgument, "a configured profile must be selected")
	}
	if _, ok := s.profiles[profile]; !ok {
		return "", status.Error(codes.InvalidArgument, "unknown execution profile")
	}
	return profile, nil
}

func requestedLifetime(value *durationpb.Duration) (time.Duration, error) {
	if value == nil {
		return 0, nil
	}
	if err := value.CheckValid(); err != nil {
		return 0, err
	}
	if value.Seconds < 0 || (value.Seconds == 0 && value.Nanos < 0) {
		return 0, errors.New("negative duration")
	}
	const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
	if value.Seconds > maxSeconds {
		return 0, errors.New("duration overflows time.Duration")
	}
	seconds := value.Seconds * int64(time.Second)
	if int64(value.Nanos) > int64(1<<63-1)-seconds {
		return 0, errors.New("duration overflows time.Duration")
	}
	return time.Duration(seconds + int64(value.Nanos)), nil
}

func (s *Server) Start(req *pluginv1alpha.StartRequest, stream pluginv1alpha.BackendPlugin_StartServer) error {
	if req == nil {
		return status.Error(codes.InvalidArgument, "start request is required")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), s.startupTimeout)
	operation := newStreamOperation(ctx, s.manager, req.EnvironmentId)
	defer func() {
		operation.stopWatching()
		cancel()
	}()
	if err := stream.Send(&pluginv1alpha.StartOutput{Output: &pluginv1alpha.StartOutput_Data{Data: &pluginv1alpha.DataChunk{
		Stream: pluginv1alpha.DataChunk_STDOUT,
		Data:   []byte("Starting environment..."),
	}}}); err != nil {
		operation.remove()
		return err
	}
	imageEnv, err := s.manager.Start(ctx, req.EnvironmentId)
	if err != nil {
		return grpcError(err)
	}
	if !operation.markCompleted() {
		return grpcError(ctx.Err())
	}
	if err := stream.Send(&pluginv1alpha.StartOutput{Output: &pluginv1alpha.StartOutput_StartComplete{StartComplete: &pluginv1alpha.StartComplete{ImageEnv: maps.Clone(imageEnv)}}}); err != nil {
		return err
	}
	return nil
}

func (s *Server) Exec(req *pluginv1alpha.ExecRequest, stream pluginv1alpha.BackendPlugin_ExecServer) error {
	if req == nil || len(req.Command) == 0 || req.Command[0] == "" {
		return status.Error(codes.InvalidArgument, "command must include an executable")
	}
	ctx := stream.Context()
	operation := newStreamOperation(ctx, s.manager, req.EnvironmentId)
	defer operation.stopWatching()
	var sendMu sync.Mutex
	var sendErr error
	send := func(out *pluginv1alpha.ExecOutput) error {
		err := stream.Send(out)
		if err != nil {
			sendMu.Lock()
			if sendErr == nil {
				sendErr = err
			}
			sendMu.Unlock()
			operation.remove()
		}
		return err
	}
	stdout, stderr := &streamWriter{send: func(data []byte) error {
		return send(&pluginv1alpha.ExecOutput{Output: &pluginv1alpha.ExecOutput_Data{Data: &pluginv1alpha.DataChunk{Stream: pluginv1alpha.DataChunk_STDOUT, Data: data}}})
	}}, &streamWriter{send: func(data []byte) error {
		return send(&pluginv1alpha.ExecOutput{Output: &pluginv1alpha.ExecOutput_Data{Data: &pluginv1alpha.DataChunk{Stream: pluginv1alpha.DataChunk_STDERR, Data: data}}})
	}}
	user := ""
	if req.User != nil {
		user = *req.User
	}
	result, err := s.manager.Exec(ctx, req.EnvironmentId, executor.ExecSpec{Command: req.Command, Env: req.Env, User: user, Workdir: req.Workdir, Workspace: req.Env["FORGEJO_WORKSPACE"]}, stdout, stderr)
	sendMu.Lock()
	streamSendErr := sendErr
	sendMu.Unlock()
	if streamSendErr != nil {
		return streamSendErr
	}
	if err != nil {
		var launch *executor.LaunchError
		if errors.As(err, &launch) {
			if !operation.markCompleted() {
				return grpcError(ctx.Err())
			}
			if err := stream.Send(&pluginv1alpha.ExecOutput{Output: &pluginv1alpha.ExecOutput_ExecFailed{ExecFailed: &pluginv1alpha.ExecFailed{ErrorMessage: launch.Message}}}); err != nil {
				return err
			}
			return nil
		}
		return grpcError(err)
	}
	if !operation.markCompleted() {
		return grpcError(ctx.Err())
	}
	if err := stream.Send(&pluginv1alpha.ExecOutput{Output: &pluginv1alpha.ExecOutput_ExecComplete{ExecComplete: &pluginv1alpha.ExecComplete{ExitCode: result.ExitCode}}}); err != nil {
		return err
	}
	return nil
}

type streamOperation struct {
	manager *executor.Manager
	id      string
	ctx     context.Context
	state   atomic.Uint32
	stop    func() bool
	done    chan struct{}
}

const (
	streamOperationPending uint32 = iota
	streamOperationComplete
	streamOperationAbandoned
)

func newStreamOperation(ctx context.Context, manager *executor.Manager, id string) *streamOperation {
	operation := &streamOperation{manager: manager, id: id, ctx: ctx, done: make(chan struct{})}
	operation.stop = context.AfterFunc(ctx, func() {
		defer close(operation.done)
		operation.abandon()
	})
	return operation
}

// Completion must win before the terminal Send so runner cancellation after
// receipt cannot invalidate the environment. Err also catches a cancellation
// whose AfterFunc callback has not run yet.
func (operation *streamOperation) markCompleted() bool {
	if operation.ctx.Err() != nil {
		operation.abandon()
		return false
	}
	return operation.state.CompareAndSwap(streamOperationPending, streamOperationComplete)
}

func (operation *streamOperation) abandon() {
	if operation.state.CompareAndSwap(streamOperationPending, streamOperationAbandoned) {
		_ = operation.manager.Remove(context.Background(), operation.id)
	}
}

func (operation *streamOperation) remove() {
	operation.state.Store(streamOperationAbandoned)
	_ = operation.manager.Remove(context.Background(), operation.id)
}

func (operation *streamOperation) stopWatching() {
	if operation.ctx.Err() != nil {
		operation.abandon()
	}
	if !operation.stop() {
		<-operation.done
		return
	}
	if operation.ctx.Err() != nil {
		operation.abandon()
	}
}

func (s *Server) CopyIn(stream pluginv1alpha.BackendPlugin_CopyInServer) error {
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return status.Error(codes.InvalidArgument, "copy-in stream requires metadata")
		}
		return grpcError(err)
	}
	if first.EnvironmentId == nil || first.DestPath == nil || *first.EnvironmentId == "" || *first.DestPath == "" {
		return status.Error(codes.InvalidArgument, "first copy-in chunk requires environment_id and dest_path")
	}
	reader := newCopyInReader(stream, first)
	defer reader.Close()
	if err := s.manager.CopyIn(stream.Context(), *first.EnvironmentId, *first.DestPath, reader); err != nil {
		return grpcError(err)
	}
	return stream.SendAndClose(&pluginv1alpha.CopyInResponse{})
}

func (s *Server) CopyOut(req *pluginv1alpha.CopyOutRequest, stream pluginv1alpha.BackendPlugin_CopyOutServer) error {
	if req == nil {
		return status.Error(codes.InvalidArgument, "copy-out request is required")
	}
	writer := &streamWriter{send: func(data []byte) error { return stream.Send(&pluginv1alpha.CopyOutChunk{Data: data}) }}
	if err := s.manager.CopyOut(stream.Context(), req.EnvironmentId, req.SrcPath, writer); err != nil {
		return grpcError(err)
	}
	return nil
}

func (s *Server) Remove(ctx context.Context, req *pluginv1alpha.RemoveRequest) (*pluginv1alpha.RemoveResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "remove request is required")
	}
	if err := s.manager.Remove(ctx, req.EnvironmentId); err != nil {
		return nil, grpcError(err)
	}
	return &pluginv1alpha.RemoveResponse{}, nil
}

var _ pluginv1alpha.BackendPluginServer = (*Server)(nil)
