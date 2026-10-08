package v1alpha

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALameLlama/fireactions/internal/executor"
	pluginv1alpha "github.com/ALameLlama/fireactions/proto/forgejo/plugin/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type adapterTestBackend struct{ vm *adapterTestVM }

func (b *adapterTestBackend) Acquire(context.Context, string, time.Time) (executor.VM, error) {
	return b.vm, nil
}

type adapterTestVM struct {
	guest       *adapterTestGuest
	mu          sync.Mutex
	destroyed   int
	destroyedCh chan struct{}
}

func (v *adapterTestVM) ID() string { return "vm-test" }
func (v *adapterTestVM) Info() executor.VMInfo {
	layout, _ := executor.DefaultLayout("amd64")
	return executor.VMInfo{Layout: layout, ImageEnv: map[string]string{"IMAGE": "value"}}
}
func (v *adapterTestVM) Guest() executor.Guest { return v.guest }
func (v *adapterTestVM) Destroy(context.Context) error {
	v.mu.Lock()
	v.destroyed++
	if v.destroyedCh != nil {
		close(v.destroyedCh)
		v.destroyedCh = nil
	}
	v.mu.Unlock()
	return nil
}
func (v *adapterTestVM) destructionCount() int { v.mu.Lock(); defer v.mu.Unlock(); return v.destroyed }

type adapterTestGuest struct {
	readyStarted chan struct{}
	readyRelease <-chan struct{}
	readyOnce    sync.Once
	execStarted  chan struct{}
	execRelease  <-chan struct{}
	execOnce     sync.Once
	execResult   executor.ExecResult
	execErr      error
	execOutput   string
	execSpec     executor.ExecSpec
}

func (g *adapterTestGuest) Ready(ctx context.Context, _ executor.ReadySpec) (string, error) {
	if g.readyStarted != nil {
		g.readyOnce.Do(func() { close(g.readyStarted) })
		if g.readyRelease != nil {
			<-g.readyRelease
			return "", ctx.Err()
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "", ctx.Err()
}
func (*adapterTestGuest) CopyIn(context.Context, string, io.Reader) error { return nil }
func (g *adapterTestGuest) Exec(_ context.Context, spec executor.ExecSpec, stdout, _ io.Writer) (executor.ExecResult, error) {
	g.execSpec = spec
	if g.execStarted != nil {
		g.execOnce.Do(func() { close(g.execStarted) })
		<-g.execRelease
	}
	if g.execOutput != "" {
		if _, err := io.WriteString(stdout, g.execOutput); err != nil {
			return executor.ExecResult{}, err
		}
	}
	return g.execResult, g.execErr
}
func (*adapterTestGuest) CopyOut(context.Context, string, io.Writer) error { return nil }
func (*adapterTestGuest) Kill(context.Context, string) error               { return nil }
func (*adapterTestGuest) Close() error                                     { return nil }

type adapterStartStream struct {
	grpc.ServerStream
	ctx             context.Context
	cancel          context.CancelFunc
	outputs         []*pluginv1alpha.StartOutput
	sendErr         error
	terminalSendErr error
}

func (s *adapterStartStream) Context() context.Context { return s.ctx }
func (s *adapterStartStream) Send(out *pluginv1alpha.StartOutput) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.outputs = append(s.outputs, out)
	if out.GetStartComplete() != nil {
		if s.cancel != nil {
			s.cancel()
		}
		return s.terminalSendErr
	}
	return nil
}

type adapterExecStream struct {
	grpc.ServerStream
	ctx             context.Context
	cancel          context.CancelFunc
	outputs         []*pluginv1alpha.ExecOutput
	sendErr         error
	terminalSendErr error
}

func (s *adapterExecStream) Context() context.Context { return s.ctx }
func (s *adapterExecStream) Send(out *pluginv1alpha.ExecOutput) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.outputs = append(s.outputs, out)
	if out.GetExecComplete() != nil || out.GetExecFailed() != nil {
		if s.cancel != nil {
			s.cancel()
		}
		return s.terminalSendErr
	}
	return nil
}

func TestExecDistinguishesNonzeroExitFromLaunchFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		result       executor.ExecResult
		err          error
		wantComplete bool
		wantCode     int32
		wantFailure  bool
	}{
		{name: "nonzero exit is a completed command", result: executor.ExecResult{ExitCode: 42}, wantComplete: true, wantCode: 42},
		{name: "launch error is an execution failure", err: &executor.LaunchError{Message: "executable not found"}, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guest := &adapterTestGuest{execResult: tc.result, execErr: tc.err}
			vm := &adapterTestVM{guest: guest, destroyedCh: make(chan struct{})}
			manager, err := executor.NewManager(&adapterTestBackend{vm: vm}, executor.Options{})
			if err != nil {
				t.Fatal(err)
			}
			s := New(manager, Options{Profiles: map[string]struct{}{"image": {}}})
			created, err := manager.Create(context.Background(), executor.CreateSpec{Profile: "image"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Start(context.Background(), created.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			stream := &adapterExecStream{ctx: ctx, cancel: cancel, terminalSendErr: context.Canceled}
			err = s.Exec(&pluginv1alpha.ExecRequest{
				EnvironmentId: created.ID,
				Command:       []string{"test-command", "--token=argv-secret"},
				Env:           map[string]string{"PRIVATE_VALUE": "env-secret", "FORGEJO_WORKSPACE": "/workspace/repo"},
			}, stream)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Exec terminal send error = %v, want cancellation after receipt", err)
			}
			terminals := 0
			for _, out := range stream.outputs {
				if got := out.GetExecComplete(); got != nil {
					terminals++
					if !tc.wantComplete || got.ExitCode != tc.wantCode {
						t.Fatalf("unexpected completion: %v", got)
					}
				}
				if got := out.GetExecFailed(); got != nil {
					terminals++
					if !tc.wantFailure {
						t.Fatalf("unexpected launch failure: %v", got)
					}
					if strings.Contains(got.ErrorMessage, "argv-secret") || strings.Contains(got.ErrorMessage, "env-secret") {
						t.Fatalf("launch failure leaked request data: %q", got.ErrorMessage)
					}
				}
				if data := out.GetData(); data != nil && (strings.Contains(string(data.Data), "argv-secret") || strings.Contains(string(data.Data), "env-secret")) {
					t.Fatalf("output leaked request data: %q", data.Data)
				}
			}
			if terminals != 1 {
				t.Fatalf("got %d terminal frames, want exactly one", terminals)
			}
			if got := vm.destructionCount(); got != 0 {
				t.Fatalf("terminal-frame cancellation destroyed the VM %d times", got)
			}

			guest.execErr = nil
			later := &adapterExecStream{ctx: context.Background()}
			if err := s.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: created.ID, Command: []string{"later"}}, later); err != nil {
				t.Fatalf("later Exec after terminal cancellation: %v", err)
			}
			if len(later.outputs) != 1 || later.outputs[0].GetExecComplete() == nil {
				t.Fatalf("later operation did not complete: %v", later.outputs)
			}
			if err := manager.Remove(context.Background(), created.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStartSendsSanitizedProgressBeforeSingleTerminal(t *testing.T) {
	guest := &adapterTestGuest{}
	vm := &adapterTestVM{guest: guest, destroyedCh: make(chan struct{})}
	manager, err := executor.NewManager(&adapterTestBackend{vm: vm}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := New(manager, Options{Profiles: map[string]struct{}{"image": {}}, StartupTimeout: time.Second})
	created, err := manager.Create(context.Background(), executor.CreateSpec{Profile: "image"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &adapterStartStream{ctx: ctx, cancel: cancel, terminalSendErr: context.Canceled}
	if err := s.Start(&pluginv1alpha.StartRequest{EnvironmentId: created.ID}, stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start terminal send error = %v, want cancellation after receipt", err)
	}
	if len(stream.outputs) < 2 || stream.outputs[0].GetData() == nil {
		t.Fatalf("Start did not send progress before completion: %v", stream.outputs)
	}
	progress := string(stream.outputs[0].GetData().Data)
	if progress != "Starting environment..." || strings.Contains(progress, "IMAGE") || strings.Contains(progress, "value") {
		t.Fatalf("Start progress was not sanitized: %q", progress)
	}
	terminals := 0
	for _, out := range stream.outputs {
		if complete := out.GetStartComplete(); complete != nil {
			terminals++
			if complete.ImageEnv["IMAGE"] != "value" {
				t.Fatalf("terminal image environment = %v", complete.ImageEnv)
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("got %d terminal frames, want exactly one", terminals)
	}
	if got := vm.destructionCount(); got != 0 {
		t.Fatalf("terminal-frame cancellation destroyed the ready VM %d times", got)
	}

	invalid := &adapterExecStream{ctx: context.Background()}
	err = s.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: created.ID}, invalid)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid Exec status = %s, want InvalidArgument", status.Code(err))
	}
	if got := vm.destructionCount(); got != 0 {
		t.Fatalf("validation error destroyed the ready VM %d times", got)
	}
	later := &adapterExecStream{ctx: context.Background()}
	if err := s.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: created.ID, Command: []string{"later"}}, later); err != nil {
		t.Fatalf("later Exec after Start cancellation: %v", err)
	}
	if len(later.outputs) != 1 || later.outputs[0].GetExecComplete() == nil {
		t.Fatalf("later operation did not complete: %v", later.outputs)
	}
	if err := manager.Remove(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStartCancellationDuringReadinessRemovesEnvironment(t *testing.T) {
	readyStarted := make(chan struct{})
	destroyed := make(chan struct{})
	vm := &adapterTestVM{guest: &adapterTestGuest{readyStarted: readyStarted, readyRelease: destroyed}, destroyedCh: destroyed}
	manager, err := executor.NewManager(&adapterTestBackend{vm: vm}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := New(manager, Options{Profiles: map[string]struct{}{"image": {}}, StartupTimeout: time.Second})
	created, err := manager.Create(context.Background(), executor.CreateSpec{Profile: "image"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &adapterStartStream{ctx: ctx, cancel: cancel}
	result := make(chan error, 1)
	go func() { result <- s.Start(&pluginv1alpha.StartRequest{EnvironmentId: created.ID}, stream) }()
	<-readyStarted
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Start succeeded after cancellation during readiness")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Start remained blocked after cancellation until VM cleanup")
	}
	select {
	case <-destroyed:
	case <-time.After(time.Second):
		t.Fatal("cancelled startup did not finish VM cleanup")
	}
	if got := vm.destructionCount(); got != 1 {
		t.Fatalf("cancelled startup destroyed the VM %d times, want 1", got)
	}
	for _, out := range stream.outputs {
		if out.GetStartComplete() != nil {
			t.Fatal("cancelled Start emitted a terminal completion")
		}
	}
}

func TestExecCancellationWhileGuestIgnoresContextRemovesEnvironment(t *testing.T) {
	execStarted := make(chan struct{})
	destroyed := make(chan struct{})
	guest := &adapterTestGuest{execStarted: execStarted, execRelease: destroyed}
	vm := &adapterTestVM{guest: guest, destroyedCh: destroyed}
	manager, err := executor.NewManager(&adapterTestBackend{vm: vm}, executor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := New(manager, Options{Profiles: map[string]struct{}{"image": {}}})
	created, err := manager.Create(context.Background(), executor.CreateSpec{Profile: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &adapterExecStream{ctx: ctx}
	result := make(chan error, 1)
	go func() {
		result <- s.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: created.ID, Command: []string{"blocked", "argv-secret"}, Env: map[string]string{"SECRET": "env-secret"}}, stream)
	}()
	<-execStarted
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Exec succeeded after cancellation")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Exec remained blocked after cancellation until VM cleanup")
	}
	select {
	case <-destroyed:
	case <-time.After(time.Second):
		t.Fatal("cancelled Exec did not finish VM cleanup")
	}
	if got := vm.destructionCount(); got != 1 {
		t.Fatalf("cancelled Exec destroyed the VM %d times, want 1", got)
	}
	for _, out := range stream.outputs {
		if data := out.GetData(); data != nil && (strings.Contains(string(data.Data), "argv-secret") || strings.Contains(string(data.Data), "env-secret")) {
			t.Fatalf("Exec output leaked request data: %q", data.Data)
		}
	}
}

func TestPreCompletionSendFailuresRemoveEnvironment(t *testing.T) {
	t.Run("start progress", func(t *testing.T) {
		vm := &adapterTestVM{guest: &adapterTestGuest{}, destroyedCh: make(chan struct{})}
		manager, err := executor.NewManager(&adapterTestBackend{vm: vm}, executor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		s := New(manager, Options{Profiles: map[string]struct{}{"image": {}}})
		created, err := manager.Create(context.Background(), executor.CreateSpec{Profile: "image"})
		if err != nil {
			t.Fatal(err)
		}
		sendErr := errors.New("stream failed")
		stream := &adapterStartStream{ctx: context.Background(), sendErr: sendErr}
		if err := s.Start(&pluginv1alpha.StartRequest{EnvironmentId: created.ID}, stream); !errors.Is(err, sendErr) {
			t.Fatalf("Start error = %v, want stream failure", err)
		}
		if got := vm.destructionCount(); got != 1 {
			t.Fatalf("failed Start send destroyed the VM %d times, want 1", got)
		}
	})

	t.Run("exec output", func(t *testing.T) {
		guest := &adapterTestGuest{execOutput: "guest output"}
		vm := &adapterTestVM{guest: guest, destroyedCh: make(chan struct{})}
		manager, err := executor.NewManager(&adapterTestBackend{vm: vm}, executor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		s := New(manager, Options{Profiles: map[string]struct{}{"image": {}}})
		created, err := manager.Create(context.Background(), executor.CreateSpec{Profile: "image"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Start(context.Background(), created.ID); err != nil {
			t.Fatal(err)
		}
		sendErr := errors.New("stream failed")
		stream := &adapterExecStream{ctx: context.Background(), sendErr: sendErr}
		if err := s.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: created.ID, Command: []string{"test"}}, stream); !errors.Is(err, sendErr) {
			t.Fatalf("Exec error = %v, want stream failure", err)
		}
		if got := vm.destructionCount(); got != 1 {
			t.Fatalf("failed Exec send destroyed the VM %d times, want 1", got)
		}
	})
}

func TestExecClassifiesPrivateTransportFailure(t *testing.T) {
	for _, test := range []struct {
		kind    executor.Kind
		code    codes.Code
		message string
	}{
		{executor.Unavailable, codes.Unavailable, "execution backend unavailable"},
		{executor.Internal, codes.Internal, "execution backend failure"},
	} {
		t.Run(test.code.String(), func(t *testing.T) {
			guest := &adapterTestGuest{
				execErr: executor.NewError(test.kind, "private transport message", status.Error(codes.DataLoss, "private cause")),
			}
			vm := &adapterTestVM{guest: guest}
			manager, err := executor.NewManager(&adapterTestBackend{vm: vm}, executor.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close(context.Background())
			created, err := manager.Create(context.Background(), executor.CreateSpec{Profile: "image"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Start(context.Background(), created.ID); err != nil {
				t.Fatal(err)
			}
			s := New(manager, Options{})
			err = s.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: created.ID, Command: []string{"true"}}, &adapterExecStream{ctx: context.Background()})
			if status.Code(err) != test.code || status.Convert(err).Message() != test.message {
				t.Fatalf("Exec error = %v, want %s: %s", err, test.code, test.message)
			}
		})
	}
}
