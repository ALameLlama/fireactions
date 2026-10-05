package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type backendFunc func(context.Context, string, time.Time) (VM, error)

func (f backendFunc) Acquire(ctx context.Context, profile string, expiry time.Time) (VM, error) {
	return f(ctx, profile, expiry)
}

type testGuest struct {
	ready   func(context.Context, ReadySpec) (string, error)
	copyIn  func(context.Context, string, io.Reader) error
	exec    func(context.Context, ExecSpec, io.Writer, io.Writer) (ExecResult, error)
	copyOut func(context.Context, string, io.Writer) error
	kill    func(context.Context, string) error
}

func (g *testGuest) Ready(ctx context.Context, spec ReadySpec) (string, error) {
	if g.ready != nil {
		return g.ready(ctx, spec)
	}
	return "test-agent", nil
}

func (g *testGuest) CopyIn(ctx context.Context, destination string, source io.Reader) error {
	if g.copyIn != nil {
		return g.copyIn(ctx, destination, source)
	}
	return nil
}

func (g *testGuest) Exec(ctx context.Context, spec ExecSpec, stdout, stderr io.Writer) (ExecResult, error) {
	if g.exec != nil {
		return g.exec(ctx, spec, stdout, stderr)
	}
	return ExecResult{}, nil
}

func (g *testGuest) CopyOut(ctx context.Context, source string, destination io.Writer) error {
	if g.copyOut != nil {
		return g.copyOut(ctx, source, destination)
	}
	return nil
}

func (g *testGuest) Kill(ctx context.Context, handle string) error {
	if g.kill != nil {
		return g.kill(ctx, handle)
	}
	return nil
}

func (g *testGuest) Close() error { return nil }

type testVM struct {
	id           string
	info         VMInfo
	guest        *testGuest
	destroy      func(context.Context) error
	destroyCalls atomic.Int32
	destroyed    chan struct{}
	destroyOnce  sync.Once
}

func newTestVM(id string) *testVM {
	return &testVM{id: id, guest: &testGuest{}, destroyed: make(chan struct{})}
}

func (v *testVM) ID() string   { return v.id }
func (v *testVM) Info() VMInfo { return v.info }
func (v *testVM) Guest() Guest { return v.guest }
func (v *testVM) Destroy(ctx context.Context) error {
	v.destroyCalls.Add(1)
	if v.destroy != nil {
		if err := v.destroy(ctx); err != nil {
			return err
		}
	}
	v.destroyOnce.Do(func() { close(v.destroyed) })
	return nil
}

func fixedBackend(vm VM) Backend {
	return backendFunc(func(context.Context, string, time.Time) (VM, error) { return vm, nil })
}

func testManager(t *testing.T, backend Backend, options Options) *Manager {
	t.Helper()
	manager, err := NewManager(backend, options)
	require.NoError(t, err)
	return manager
}

func createEnvironment(t *testing.T, manager *Manager, spec CreateSpec) EnvironmentInfo {
	t.Helper()
	if spec.Profile == "" {
		spec.Profile = "ubuntu-24.04"
	}
	info, err := manager.Create(context.Background(), spec)
	require.NoError(t, err)
	return info
}

func startEnvironment(t *testing.T, manager *Manager, id string) map[string]string {
	t.Helper()
	env, err := manager.Start(context.Background(), id)
	require.NoError(t, err)
	return env
}

func TestDefaultLayoutAndInvalidSettings(t *testing.T) {
	for _, test := range []struct{ architecture, expected string }{
		{"amd64", "X64"}, {"X64", "X64"}, {"arm64", "ARM64"}, {"ARM64", "ARM64"},
	} {
		layout, err := DefaultLayout(test.architecture)
		require.NoError(t, err)
		require.Equal(t, Layout{
			Root: "/workspace", Act: "/workspace/.fireactions/act",
			ToolCache: "/workspace/.fireactions/toolcache", Temp: "/workspace/.fireactions/tmp",
			OS: "Linux", Arch: test.expected, DefaultPath: DefaultPath,
		}, layout)
	}
	_, err := DefaultLayout("386")
	require.Equal(t, InvalidArgument, KindOf(err))
	_, err = NewManager(nil, Options{})
	require.Equal(t, InvalidArgument, KindOf(err))
	for _, options := range []Options{
		{MaxLifetime: -time.Second}, {CleanupTimeout: -time.Second}, {CleanupTimeout: time.Minute},
	} {
		_, err = NewManager(fixedBackend(newTestVM("vm")), options)
		require.Equal(t, InvalidArgument, KindOf(err))
	}
}

func TestIndependentOpaqueOwnershipAndAcquisitionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var vms []*testVM
		var expiries []time.Time
		backend := backendFunc(func(_ context.Context, profile string, expiry time.Time) (VM, error) {
			mu.Lock()
			defer mu.Unlock()
			if profile != "ubuntu-24.04" {
				return nil, NewError(InvalidArgument, "unknown profile", nil)
			}
			vm := newTestVM(fmt.Sprintf("internal-vm-%d", len(vms)))
			vms = append(vms, vm)
			expiries = append(expiries, expiry)
			return vm, nil
		})
		manager := testManager(t, backend, Options{MaxLifetime: time.Hour})
		defer manager.Close(context.Background())
		createdAt := time.Now()
		results := make(chan EnvironmentInfo, 2)
		errors := make(chan error, 2)
		for range 2 {
			go func() {
				info, err := manager.Create(context.Background(), CreateSpec{Profile: "ubuntu-24.04", Lifetime: 2 * time.Hour})
				results <- info
				errors <- err
			}()
		}
		first, second := <-results, <-results
		require.NoError(t, <-errors)
		require.NoError(t, <-errors)
		require.NotEqual(t, first.ID, second.ID)
		for _, id := range []string{first.ID, second.ID} {
			require.Len(t, id, 43)
			decoded, err := base64.RawURLEncoding.Strict().DecodeString(id)
			require.NoError(t, err)
			require.Len(t, decoded, 32)
			require.NotContains(t, id, "internal-vm")
		}
		mu.Lock()
		require.Len(t, vms, 2)
		require.Equal(t, []time.Time{createdAt.Add(time.Hour), createdAt.Add(time.Hour)}, expiries)
		mu.Unlock()
		require.NoError(t, manager.Remove(context.Background(), first.ID))
		startEnvironment(t, manager, second.ID)
		_, err := manager.Start(context.Background(), first.ID)
		require.Equal(t, NotFound, KindOf(err))
	})
}

func TestStartPreconditionsAndEnvironmentIsolation(t *testing.T) {
	t.Setenv("FIREACTIONS_EXECUTOR_HOST_SECRET", "must-not-reach-guest")
	vm := newTestVM("vm")
	vm.info = VMInfo{ImageEnv: map[string]string{"IMAGE_ONLY": "base", "SHARED": "image"}, DefaultUser: "build"}
	var readyCount int
	var readySpec ReadySpec
	vm.guest.ready = func(_ context.Context, spec ReadySpec) (string, error) {
		readyCount++
		readySpec = spec
		return "test-agent", nil
	}
	var executed ExecSpec
	vm.guest.exec = func(_ context.Context, spec ExecSpec, stdout, stderr io.Writer) (ExecResult, error) {
		executed = spec
		_, err := io.WriteString(stdout, "stdout\n")
		if err != nil {
			return ExecResult{}, err
		}
		_, err = io.WriteString(stderr, "stderr\n")
		return ExecResult{ExitCode: 42}, err
	}
	manager := testManager(t, fixedBackend(vm), Options{})
	defer manager.Close(context.Background())
	info := createEnvironment(t, manager, CreateSpec{})
	vm.info.ImageEnv["SHARED"] = "changed-after-acquisition"
	_, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"echo"}}, nil, nil)
	require.Equal(t, FailedPrecondition, KindOf(err))
	err = manager.CopyIn(context.Background(), info.ID, "/workspace", strings.NewReader("archive"))
	require.Equal(t, FailedPrecondition, KindOf(err))
	err = manager.CopyOut(context.Background(), info.ID, "/workspace", io.Discard)
	require.Equal(t, FailedPrecondition, KindOf(err))
	imageEnv := startEnvironment(t, manager, info.ID)
	require.Equal(t, map[string]string{"IMAGE_ONLY": "base", "SHARED": "image", "PATH": DefaultPath}, imageEnv)
	imageEnv["SHARED"] = "modified-returned-map"
	require.Equal(t, "image", startEnvironment(t, manager, info.ID)["SHARED"])
	require.Equal(t, 1, readyCount)
	require.Equal(t, []string{info.Layout.Root, info.Layout.Act, info.Layout.ToolCache, info.Layout.Temp}, readySpec.Directories)
	require.Equal(t, "build", readySpec.DefaultUser)
	require.Equal(t, DefaultMaxTransferBytes, readySpec.MaxTransferBytes)
	require.Equal(t, DefaultMaxArchiveEntries, readySpec.MaxArchiveEntries)
	requestEnv := map[string]string{"SHARED": "request", "REQUEST_ONLY": "request"}
	var stdout, stderr bytes.Buffer
	result, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"echo", "literal;argument"}, Env: requestEnv}, &stdout, &stderr)
	require.NoError(t, err)
	require.Equal(t, int32(42), result.ExitCode)
	require.Equal(t, "stdout\n", stdout.String())
	require.Equal(t, "stderr\n", stderr.String())
	require.Equal(t, map[string]string{"IMAGE_ONLY": "base", "SHARED": "request", "REQUEST_ONLY": "request", "PATH": DefaultPath}, executed.Env)
	require.Equal(t, "build", executed.User)
	require.Equal(t, "/workspace", executed.Workdir)
	require.Equal(t, map[string]string{"SHARED": "request", "REQUEST_ONLY": "request"}, requestEnv)
	_, err = manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"echo"}, Env: map[string]string{"PATH": "/custom"}, User: "root", Workdir: "/workspace/project"}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "/custom", executed.Env["PATH"])
	require.Equal(t, "root", executed.User)
	require.Equal(t, "/workspace/project", executed.Workdir)
}

func TestExecCreatesDeclaredWorkspaceButNotWorkdir(t *testing.T) {
	vm := newTestVM("vm")
	hostRoot, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer hostRoot.Close()
	mapPath := func(guestPath string) string {
		rel := strings.TrimPrefix(guestPath, WorkspaceRoot)
		if rel == "" {
			return "."
		}
		if rel != "" && !strings.HasPrefix(rel, "/") {
			t.Fatalf("guest path %q is outside %s", guestPath, WorkspaceRoot)
		}
		return strings.TrimPrefix(rel, "/")
	}
	mkdirAll := func(guestPath string) error {
		return hostRoot.MkdirAll(mapPath(guestPath), 0o755)
	}
	vm.guest.ready = func(_ context.Context, spec ReadySpec) (string, error) {
		for _, directory := range spec.Directories {
			if err := mkdirAll(directory); err != nil {
				return "", err
			}
		}
		return "test-agent", nil
	}
	vm.guest.exec = func(_ context.Context, spec ExecSpec, stdout, _ io.Writer) (ExecResult, error) {
		file, err := hostRoot.Open(mapPath(spec.Workdir + "/marker"))
		if err != nil {
			return ExecResult{}, &LaunchError{Message: "working directory is missing"}
		}
		defer file.Close()
		_, err = io.Copy(stdout, file)
		return ExecResult{}, err
	}
	manager := testManager(t, fixedBackend(vm), Options{})
	defer manager.Close(context.Background())
	info := createEnvironment(t, manager, CreateSpec{})
	startEnvironment(t, manager, info.ID)

	workspace := WorkspaceRoot + "/owner/repo"
	workdir := workspace + "/not-created"
	_, err = hostRoot.Stat(mapPath(workspace))
	require.ErrorIs(t, err, os.ErrNotExist)
	spec := ExecSpec{Command: []string{"run"}, Workspace: workspace, Workdir: workdir}
	var launch *LaunchError
	if _, err := manager.Exec(context.Background(), info.ID, spec, nil, nil); !errors.As(err, &launch) {
		t.Fatalf("Exec with missing Workdir error = %v, want launch failure", err)
	}
	workspaceInfo, err := hostRoot.Stat(mapPath(workspace))
	require.NoError(t, err)
	require.True(t, workspaceInfo.IsDir())
	_, err = hostRoot.Stat(mapPath(workdir))
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, mkdirAll(workdir))
	file, err := hostRoot.Create(mapPath(workdir + "/marker"))
	require.NoError(t, err)
	_, err = file.Write([]byte("persistent"))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	var output bytes.Buffer
	_, err = manager.Exec(context.Background(), info.ID, spec, &output, nil)
	require.NoError(t, err)
	require.Equal(t, "persistent", output.String())
	_, err = manager.Exec(context.Background(), info.ID, spec, nil, nil)
	require.NoError(t, err)

	missing := WorkspaceRoot + "/generic-missing"
	_, err = manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"run"}, Workdir: missing}, nil, nil)
	if !errors.As(err, &launch) {
		t.Fatalf("generic Exec with missing Workdir error = %v, want launch failure", err)
	}
	_, err = hostRoot.Stat(mapPath(missing))
	require.ErrorIs(t, err, os.ErrNotExist)
	other := WorkspaceRoot + "/other-repo"
	_, err = manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"run"}, Workspace: other}, nil, nil)
	require.Equal(t, InvalidArgument, KindOf(err))
	_, err = hostRoot.Stat(mapPath(other))
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, escaped := range []string{WorkspaceRoot + "/../outside", WorkspaceRoot + "/owner/../outside", WorkspaceRoot + "/owner/\x00repo"} {
		_, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"run"}, Workspace: escaped}, nil, nil)
		require.Equal(t, InvalidArgument, KindOf(err))
	}
}

func TestImagePathAndProfileReadySettings(t *testing.T) {
	vm := newTestVM("vm")
	vm.info.ImageEnv = map[string]string{"PATH": "/image/bin"}
	var readySpec ReadySpec
	vm.guest.ready = func(_ context.Context, spec ReadySpec) (string, error) {
		readySpec = spec
		return "test-agent", nil
	}
	manager := testManager(t, fixedBackend(vm), Options{ReadySpec: func(profile string) (ReadySpec, error) {
		return ReadySpec{DefaultUser: "1001:1001", MaxTransferBytes: 1024, MaxArchiveEntries: 10, Directories: []string{"/workspace/project"}}, nil
	}})
	defer manager.Close(context.Background())
	info := createEnvironment(t, manager, CreateSpec{})
	require.Equal(t, "/image/bin", startEnvironment(t, manager, info.ID)["PATH"])
	require.Equal(t, "1001:1001", readySpec.DefaultUser)
	require.Equal(t, int64(1024), readySpec.MaxTransferBytes)
	require.Equal(t, 10, readySpec.MaxArchiveEntries)
	require.Contains(t, readySpec.Directories, "/workspace/.fireactions/act")
	require.Contains(t, readySpec.Directories, "/workspace/project")
	vm.guest.exec = func(_ context.Context, spec ExecSpec, _, _ io.Writer) (ExecResult, error) {
		require.Equal(t, "1001:1001", spec.User)
		require.Equal(t, "/image/bin", spec.Env["PATH"])
		return ExecResult{}, nil
	}
	_, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"true"}}, nil, nil)
	require.NoError(t, err)
}

func TestUnknownAndMalformedEnvironmentIDs(t *testing.T) {
	manager := testManager(t, fixedBackend(newTestVM("vm")), Options{})
	defer manager.Close(context.Background())
	absent := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for _, id := range []string{"", "runner-name", strings.Repeat("/", 43), strings.Repeat("A", 42) + "B", absent} {
		expected := InvalidArgument
		if id == absent {
			expected = NotFound
		}
		_, err := manager.Start(context.Background(), id)
		require.Equal(t, expected, KindOf(err))
		err = manager.CopyIn(context.Background(), id, "/workspace", strings.NewReader("archive"))
		require.Equal(t, expected, KindOf(err))
		_, err = manager.Exec(context.Background(), id, ExecSpec{Command: []string{"true"}}, nil, nil)
		require.Equal(t, expected, KindOf(err))
		err = manager.CopyOut(context.Background(), id, "/workspace", io.Discard)
		require.Equal(t, expected, KindOf(err))
		err = manager.Remove(context.Background(), id)
		if id == absent {
			require.NoError(t, err)
		} else {
			require.Equal(t, InvalidArgument, KindOf(err))
		}
	}
}

func TestRemoveCancelsSilentExecAndSharesCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("vm")
		execStarted := make(chan struct{})
		execEnded := make(chan error, 1)
		vm.guest.exec = func(ctx context.Context, _ ExecSpec, _, _ io.Writer) (ExecResult, error) {
			close(execStarted)
			<-ctx.Done()
			return ExecResult{}, context.Cause(ctx)
		}
		var kills atomic.Int32
		vm.guest.kill = func(ctx context.Context, handle string) error {
			kills.Add(1)
			if ctx.Err() != nil || handle != "" {
				return errors.New("cleanup did not get an independent kill-all context")
			}
			return nil
		}
		allowDestroy := make(chan struct{})
		vm.destroy = func(ctx context.Context) error {
			if ctx.Err() != nil {
				return errors.New("cleanup used cancelled caller context")
			}
			<-allowDestroy
			return nil
		}
		manager := testManager(t, fixedBackend(vm), Options{})
		defer manager.Close(context.Background())
		info := createEnvironment(t, manager, CreateSpec{})
		startEnvironment(t, manager, info.ID)
		go func() {
			_, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"sleep", "300"}}, nil, nil)
			execEnded <- err
		}()
		<-execStarted
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		results := make(chan error, 8)
		for range 8 {
			go func() { results <- manager.Remove(cancelledCtx, info.ID) }()
		}
		synctest.Wait()
		require.ErrorIs(t, <-execEnded, context.Canceled)
		_, err := manager.Start(context.Background(), info.ID)
		require.Equal(t, FailedPrecondition, KindOf(err))
		close(allowDestroy)
		for range 8 {
			require.NoError(t, <-results)
		}
		require.Equal(t, int32(1), kills.Load())
		require.Equal(t, int32(1), vm.destroyCalls.Load())
		require.NoError(t, manager.Remove(context.Background(), info.ID))
		_, err = manager.Start(context.Background(), info.ID)
		require.Equal(t, NotFound, KindOf(err))
	})
}

func TestIncompleteCleanupRetainsOwnershipAndRetries(t *testing.T) {
	vm := newTestVM("vm")
	var fail atomic.Bool
	fail.Store(true)
	failure := errors.New("snapshot removal failed")
	vm.destroy = func(context.Context) error {
		if fail.Load() {
			return failure
		}
		return nil
	}
	manager := testManager(t, fixedBackend(vm), Options{})
	defer manager.Close(context.Background())
	info := createEnvironment(t, manager, CreateSpec{})
	startEnvironment(t, manager, info.ID)
	err := manager.Remove(context.Background(), info.ID)
	require.ErrorIs(t, err, failure)
	e, err := manager.lookup(info.ID)
	require.NoError(t, err)
	require.Same(t, vm, e.vm)
	_, err = manager.Start(context.Background(), info.ID)
	require.Equal(t, FailedPrecondition, KindOf(err))
	fail.Store(false)
	require.NoError(t, manager.RetryRemovals(context.Background()))
	require.Equal(t, int32(2), vm.destroyCalls.Load())
	_, err = manager.lookup(info.ID)
	require.Equal(t, NotFound, KindOf(err))
	require.NoError(t, manager.Remove(context.Background(), info.ID))
}

func TestHardDeadlineCancelsAndDestroysActiveEnvironment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("vm")
		vm.guest.exec = func(ctx context.Context, _ ExecSpec, _, _ io.Writer) (ExecResult, error) {
			<-ctx.Done()
			return ExecResult{}, context.Cause(ctx)
		}
		manager := testManager(t, fixedBackend(vm), Options{MaxLifetime: 2 * time.Minute})
		defer manager.Close(context.Background())
		info := createEnvironment(t, manager, CreateSpec{Lifetime: time.Minute})
		startEnvironment(t, manager, info.ID)
		execDone := make(chan error, 1)
		go func() {
			_, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"sleep", "300"}}, nil, nil)
			execDone <- err
		}()
		synctest.Wait()
		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		require.Zero(t, vm.destroyCalls.Load())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		require.ErrorIs(t, <-execDone, context.DeadlineExceeded)
		require.Equal(t, int32(1), vm.destroyCalls.Load())
		_, err := manager.Start(context.Background(), info.ID)
		require.Equal(t, NotFound, KindOf(err))
	})
}

func TestAcquisitionCancellationOwnsLateVM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("late-vm")
		acquiring := make(chan struct{})
		backend := backendFunc(func(ctx context.Context, _ string, _ time.Time) (VM, error) {
			close(acquiring)
			<-ctx.Done()
			return vm, ctx.Err()
		})
		manager := testManager(t, backend, Options{})
		defer manager.Close(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			info, err := manager.Create(ctx, CreateSpec{Profile: "ubuntu-24.04"})
			if info.ID != "" {
				result <- errors.New("cancelled creation exposed an environment")
			} else {
				result <- err
			}
		}()
		<-acquiring
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		require.Equal(t, int32(1), vm.destroyCalls.Load())
		require.Empty(t, manager.environments)
	})
}

func TestProvisioningTimeCountsTowardHardDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("vm")
		createdAt := time.Now()
		backend := backendFunc(func(ctx context.Context, _ string, expiry time.Time) (VM, error) {
			if !expiry.Equal(createdAt.Add(time.Minute)) {
				return nil, errors.New("deadline was not calculated before acquisition")
			}
			<-ctx.Done()
			return vm, nil
		})
		manager := testManager(t, backend, Options{})
		defer manager.Close(context.Background())
		info, err := manager.Create(context.Background(), CreateSpec{Profile: "ubuntu-24.04", Lifetime: time.Minute})
		require.Empty(t, info.ID)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, time.Minute, time.Since(createdAt))
		require.Equal(t, int32(1), vm.destroyCalls.Load())
		require.Empty(t, manager.environments)
	})
}

func TestTerminalExecResultsDoNotInvalidateEnvironment(t *testing.T) {
	for _, launchFailed := range []bool{false, true} {
		t.Run(fmt.Sprintf("launchFailed=%v", launchFailed), func(t *testing.T) {
			vm := newTestVM("vm")
			manager := testManager(t, fixedBackend(vm), Options{})
			defer manager.Close(context.Background())
			info := createEnvironment(t, manager, CreateSpec{})
			startEnvironment(t, manager, info.ID)
			ctx, cancel := context.WithCancel(context.Background())
			vm.guest.exec = func(context.Context, ExecSpec, io.Writer, io.Writer) (ExecResult, error) {
				cancel()
				if launchFailed {
					return ExecResult{}, &LaunchError{Message: "executable not found"}
				}
				return ExecResult{ExitCode: 42}, nil
			}
			result, err := manager.Exec(ctx, info.ID, ExecSpec{Command: []string{"step"}}, nil, nil)
			if launchFailed {
				var launch *LaunchError
				require.ErrorAs(t, err, &launch)
			} else {
				require.NoError(t, err)
				require.Equal(t, int32(42), result.ExitCode)
			}
			require.Zero(t, vm.destroyCalls.Load())
			startEnvironment(t, manager, info.ID)
		})
	}
}

func TestInterruptedExecAndConnectionLossInvalidateEnvironment(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable=%v", unavailable), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				vm := newTestVM("vm")
				started := make(chan struct{})
				vm.guest.exec = func(ctx context.Context, _ ExecSpec, _, _ io.Writer) (ExecResult, error) {
					close(started)
					if unavailable {
						return ExecResult{}, NewError(Unavailable, "guest control connection lost", nil)
					}
					<-ctx.Done()
					return ExecResult{}, context.Cause(ctx)
				}
				manager := testManager(t, fixedBackend(vm), Options{})
				defer manager.Close(context.Background())
				info := createEnvironment(t, manager, CreateSpec{})
				startEnvironment(t, manager, info.ID)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					_, err := manager.Exec(ctx, info.ID, ExecSpec{Command: []string{"sleep", "300"}}, nil, nil)
					result <- err
				}()
				<-started
				cancel()
				err := <-result
				if unavailable {
					require.Equal(t, Unavailable, KindOf(err))
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
				synctest.Wait()
				require.Equal(t, int32(1), vm.destroyCalls.Load())
				_, err = manager.Start(context.Background(), info.ID)
				require.Equal(t, NotFound, KindOf(err))
			})
		})
	}
}

func TestUncooperativeExecCannotBlockHostDestruction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("vm")
		started, stopped := make(chan struct{}), make(chan struct{})
		vm.guest.exec = func(ctx context.Context, _ ExecSpec, _, _ io.Writer) (ExecResult, error) {
			close(started)
			<-stopped
			return ExecResult{}, context.Cause(ctx)
		}
		vm.guest.kill = func(context.Context, string) error { return NewError(Unavailable, "guest did not cooperate", nil) }
		vm.destroy = func(context.Context) error { close(stopped); return nil }
		manager := testManager(t, fixedBackend(vm), Options{})
		defer manager.Close(context.Background())
		info := createEnvironment(t, manager, CreateSpec{})
		startEnvironment(t, manager, info.ID)
		result := make(chan error, 1)
		go func() {
			_, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"sleep"}}, nil, nil)
			result <- err
		}()
		<-started
		before := time.Now()
		require.NoError(t, manager.Remove(context.Background(), info.ID))
		require.Equal(t, guestCooperationTimeout, time.Since(before))
		require.ErrorIs(t, <-result, context.Canceled)
		require.Equal(t, int32(1), vm.destroyCalls.Load())
	})
}

func TestOperationGatesArePerEnvironment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newTestVM("first-vm"), newTestVM("second-vm")
		started := make(chan struct{})
		first.guest.exec = func(ctx context.Context, _ ExecSpec, _, _ io.Writer) (ExecResult, error) {
			close(started)
			<-ctx.Done()
			return ExecResult{}, context.Cause(ctx)
		}
		calls := 0
		backend := backendFunc(func(context.Context, string, time.Time) (VM, error) {
			calls++
			if calls == 1 {
				return first, nil
			}
			return second, nil
		})
		manager := testManager(t, backend, Options{})
		defer manager.Close(context.Background())
		firstInfo := createEnvironment(t, manager, CreateSpec{})
		startEnvironment(t, manager, firstInfo.ID)
		result := make(chan error, 1)
		go func() {
			_, err := manager.Exec(context.Background(), firstInfo.ID, ExecSpec{Command: []string{"sleep"}}, nil, nil)
			result <- err
		}()
		<-started
		secondInfo := createEnvironment(t, manager, CreateSpec{})
		startEnvironment(t, manager, secondInfo.ID)
		_, err := manager.Exec(context.Background(), secondInfo.ID, ExecSpec{Command: []string{"true"}}, nil, nil)
		require.NoError(t, err)
		require.NoError(t, manager.Remove(context.Background(), firstInfo.ID))
		require.ErrorIs(t, <-result, context.Canceled)
	})
}

func TestCloseRetainsCancelledAcquisitionUntilVMIsKnown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("late-vm")
		started, release := make(chan struct{}), make(chan struct{})
		backend := backendFunc(func(context.Context, string, time.Time) (VM, error) {
			close(started)
			<-release
			return vm, nil
		})
		manager := testManager(t, backend, Options{CleanupTimeout: time.Second})
		defer manager.Close(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := manager.Create(context.Background(), CreateSpec{Profile: "ubuntu-24.04"})
			result <- err
		}()
		<-started
		require.ErrorIs(t, manager.Close(context.Background()), context.DeadlineExceeded)
		require.Len(t, manager.environments, 1)
		require.Zero(t, vm.destroyCalls.Load())
		close(release)
		require.ErrorIs(t, <-result, context.Canceled)
		require.Equal(t, int32(1), vm.destroyCalls.Load())
		require.Empty(t, manager.environments)
		_, err := manager.Create(context.Background(), CreateSpec{Profile: "ubuntu-24.04"})
		require.Equal(t, Unavailable, KindOf(err))
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random source unavailable") }

func TestInvalidCreationAndEntropyFailureAllocateNothing(t *testing.T) {
	var calls atomic.Int32
	backend := backendFunc(func(context.Context, string, time.Time) (VM, error) {
		calls.Add(1)
		return newTestVM("vm"), nil
	})
	manager := testManager(t, backend, Options{})
	defer manager.Close(context.Background())
	for _, spec := range []CreateSpec{{}, {Profile: "ubuntu", Lifetime: -time.Second}} {
		_, err := manager.Create(context.Background(), spec)
		require.Equal(t, InvalidArgument, KindOf(err))
	}
	manager.random = failingReader{}
	_, err := manager.Create(context.Background(), CreateSpec{Profile: "ubuntu"})
	require.Equal(t, Internal, KindOf(err))
	require.Zero(t, calls.Load())
	require.Empty(t, manager.environments)
	for _, spec := range []ReadySpec{{MaxTransferBytes: -1}, {MaxArchiveEntries: -1}, {Directories: []string{"/etc"}}, {Directories: []string{"/workspace/a/../b"}}} {
		manager := testManager(t, backend, Options{ReadySpec: func(string) (ReadySpec, error) { return spec, nil }})
		_, err := manager.Create(context.Background(), CreateSpec{Profile: "ubuntu"})
		require.Equal(t, InvalidArgument, KindOf(err))
		require.NoError(t, manager.Close(context.Background()))
	}
	require.Zero(t, calls.Load())
}

func TestInvalidExecDoesNotCallGuest(t *testing.T) {
	vm := newTestVM("vm")
	var executions atomic.Int32
	vm.guest.exec = func(context.Context, ExecSpec, io.Writer, io.Writer) (ExecResult, error) {
		executions.Add(1)
		return ExecResult{}, nil
	}
	manager := testManager(t, fixedBackend(vm), Options{})
	defer manager.Close(context.Background())
	info := createEnvironment(t, manager, CreateSpec{})
	startEnvironment(t, manager, info.ID)
	for _, spec := range []ExecSpec{
		{}, {Command: []string{""}}, {Command: []string{"true", "\x00"}},
		{Command: []string{"true"}, Env: map[string]string{"BAD=KEY": "value"}},
		{Command: []string{"true"}, Env: map[string]string{"OK": "\x00"}},
	} {
		_, err := manager.Exec(context.Background(), info.ID, spec, nil, nil)
		require.Equal(t, InvalidArgument, KindOf(err))
	}
	require.Zero(t, executions.Load())
	require.Equal(t, map[string]string{"PATH": DefaultPath}, startEnvironment(t, manager, info.ID))
}

func TestErrorClassificationPreservesCausesWithoutLeakingMessages(t *testing.T) {
	cause := errors.New("private command and credentials")
	err := NewError(Unavailable, "guest connection lost", cause)
	require.Equal(t, "guest connection lost", err.Error())
	require.ErrorIs(t, err, cause)
	require.Equal(t, Unavailable, KindOf(err))
	require.Equal(t, Cancelled, KindOf(NewError(Internal, "interrupted", context.Canceled)))
	require.Equal(t, DeadlineExceeded, KindOf(context.DeadlineExceeded))
	require.Equal(t, Internal, KindOf(errors.New("unexpected failure")))
	require.Equal(t, Unknown, KindOf(nil))
}

func TestBackendCannotOfferOneVMToTwoEnvironments(t *testing.T) {
	vm := newTestVM("shared-vm")
	manager := testManager(t, fixedBackend(vm), Options{})
	defer manager.Close(context.Background())
	first := createEnvironment(t, manager, CreateSpec{})
	startEnvironment(t, manager, first.ID)
	second, err := manager.Create(context.Background(), CreateSpec{Profile: "ubuntu-24.04"})
	require.Equal(t, Internal, KindOf(err))
	require.Empty(t, second.ID)
	require.Zero(t, vm.destroyCalls.Load())
	startEnvironment(t, manager, first.ID)
	require.NoError(t, manager.Remove(context.Background(), first.ID))
	require.Equal(t, int32(1), vm.destroyCalls.Load())
}

func TestStartCancellationDestroysUnreadyEnvironment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("vm")
		started := make(chan struct{})
		vm.guest.ready = func(ctx context.Context, _ ReadySpec) (string, error) {
			close(started)
			<-ctx.Done()
			return "", context.Cause(ctx)
		}
		manager := testManager(t, fixedBackend(vm), Options{})
		defer manager.Close(context.Background())
		info := createEnvironment(t, manager, CreateSpec{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := manager.Start(ctx, info.ID)
			result <- err
		}()
		<-started
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		synctest.Wait()
		require.Equal(t, int32(1), vm.destroyCalls.Load())
		_, err := manager.Start(context.Background(), info.ID)
		require.Equal(t, NotFound, KindOf(err))
	})
}

func TestCompletedReadinessAndDefaultIdentity(t *testing.T) {
	vm := newTestVM("vm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	vm.guest.ready = func(_ context.Context, spec ReadySpec) (string, error) {
		require.Equal(t, DefaultUser, spec.DefaultUser)
		cancel()
		return "test-agent", nil
	}
	manager := testManager(t, fixedBackend(vm), Options{})
	defer manager.Close(context.Background())
	info := createEnvironment(t, manager, CreateSpec{})
	env, err := manager.Start(ctx, info.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"PATH": DefaultPath}, env)
	startEnvironment(t, manager, info.ID)
	vm.guest.exec = func(_ context.Context, spec ExecSpec, _, _ io.Writer) (ExecResult, error) {
		require.Equal(t, DefaultUser, spec.User)
		require.Equal(t, "/workspace", spec.Workdir)
		require.Equal(t, map[string]string{"PATH": DefaultPath}, spec.Env)
		return ExecResult{}, nil
	}
	_, err = manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"true"}}, nil, nil)
	require.NoError(t, err)
	require.Zero(t, vm.destroyCalls.Load())
}

func TestCancelledTransferDoesNotMasqueradeAsConnectionLoss(t *testing.T) {
	vm := newTestVM("vm")
	vm.guest.copyIn = func(context.Context, string, io.Reader) error { return context.Canceled }
	vm.guest.copyOut = func(context.Context, string, io.Writer) error { return context.Canceled }
	manager := testManager(t, fixedBackend(vm), Options{})
	defer manager.Close(context.Background())
	info := createEnvironment(t, manager, CreateSpec{})
	startEnvironment(t, manager, info.ID)
	require.ErrorIs(t, manager.CopyIn(context.Background(), info.ID, "/workspace", strings.NewReader("archive")), context.Canceled)
	require.ErrorIs(t, manager.CopyOut(context.Background(), info.ID, "/workspace", io.Discard), context.Canceled)
	startEnvironment(t, manager, info.ID)
	require.Zero(t, vm.destroyCalls.Load())
}

func TestOperationsSerializeWithinOneEnvironment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		vm := newTestVM("vm")
		started, release := make(chan struct{}), make(chan struct{})
		vm.guest.exec = func(context.Context, ExecSpec, io.Writer, io.Writer) (ExecResult, error) {
			close(started)
			<-release
			return ExecResult{}, nil
		}
		var copies atomic.Int32
		vm.guest.copyIn = func(context.Context, string, io.Reader) error {
			copies.Add(1)
			return nil
		}
		manager := testManager(t, fixedBackend(vm), Options{})
		defer manager.Close(context.Background())
		info := createEnvironment(t, manager, CreateSpec{})
		startEnvironment(t, manager, info.ID)
		execDone, copyDone := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := manager.Exec(context.Background(), info.ID, ExecSpec{Command: []string{"step"}}, nil, nil)
			execDone <- err
		}()
		<-started
		go func() {
			copyDone <- manager.CopyIn(context.Background(), info.ID, "/workspace", strings.NewReader("archive"))
		}()
		synctest.Wait()
		require.Zero(t, copies.Load())
		close(release)
		require.NoError(t, <-execDone)
		require.NoError(t, <-copyDone)
		require.Equal(t, int32(1), copies.Load())
	})
}
