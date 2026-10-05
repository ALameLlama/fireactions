package executor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const guestCooperationTimeout = 2 * time.Second

type environmentState uint8

const (
	stateAcquiring environmentState = iota
	stateCreated
	stateStarted
	stateRemoving
)

type cleanupAttempt struct {
	done chan struct{}
	err  error
}

type environment struct {
	id, profile         string
	createdAt, deadline time.Time
	ctx                 context.Context
	cancel              context.CancelFunc
	gate                chan struct{}
	acquired            chan struct{}

	mu        sync.Mutex
	state     environmentState
	vm        VM
	vmID      string
	guest     Guest
	info      VMInfo
	ready     ReadySpec
	workspace string
	cleanup   *cleanupAttempt
	destroyed bool
}

// Manager owns each acquired VM until destruction succeeds. The registry mutex
// only protects map membership; each environment serializes its own operations.
// Cancellation and removal never need to acquire the operation gate first.
type Manager struct {
	backend      Backend
	options      Options
	random       io.Reader
	mu           sync.Mutex
	environments map[string]*environment
	vms          map[string]*environment
	closed       bool
}

func NewManager(backend Backend, options Options) (*Manager, error) {
	if backend == nil {
		return nil, NewError(InvalidArgument, "an execution backend is required", nil)
	}
	if options.MaxLifetime == 0 {
		options.MaxLifetime = 3*time.Hour + 2*time.Minute
	}
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = 30 * time.Second
	}
	if options.MaxLifetime < 0 || options.CleanupTimeout < 0 {
		return nil, NewError(InvalidArgument, "environment lifetime and cleanup timeout must be positive", nil)
	}
	if options.CleanupTimeout > 30*time.Second {
		return nil, NewError(InvalidArgument, "cleanup attempts cannot exceed 30 seconds", nil)
	}
	return &Manager{
		backend:      backend,
		options:      options,
		random:       rand.Reader,
		environments: make(map[string]*environment),
		vms:          make(map[string]*environment),
	}, nil
}

// EnvironmentForVM provides cached administrative ownership without guest IO.
// Registry membership and mutable environment state use separate locks.
func (m *Manager) EnvironmentForVM(vmID string) (id string, removing bool) {
	m.mu.Lock()
	e := m.vms[vmID]
	m.mu.Unlock()
	if e == nil {
		return "", false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.destroyed {
		return "", false
	}
	return e.id, e.state == stateRemoving
}

func (m *Manager) Create(ctx context.Context, spec CreateSpec) (result EnvironmentInfo, err error) {
	profile := spec.Profile
	defer m.observeOperation(OperationCreate, &err, &profile)
	createdAt := time.Now()
	if spec.Profile == "" || spec.Lifetime < 0 {
		return EnvironmentInfo{}, NewError(InvalidArgument, "a valid profile and nonnegative lifetime are required", nil)
	}
	if err := ctx.Err(); err != nil {
		return EnvironmentInfo{}, err
	}
	lifetime := spec.Lifetime
	if lifetime == 0 || lifetime > m.options.MaxLifetime {
		lifetime = m.options.MaxLifetime
	}
	deadline := createdAt.Add(lifetime)
	ready := ReadySpec{}
	if m.options.ReadySpec != nil {
		var err error
		ready, err = m.options.ReadySpec(spec.Profile)
		if err != nil {
			return EnvironmentInfo{}, err
		}
	}
	if err := validateReadySpec(ready); err != nil {
		return EnvironmentInfo{}, err
	}
	// Reserve the opaque ID before acquisition. Even a cancelled acquisition
	// that returns a VM late remains owned and retryable by this registry.
	e, err := m.reserve(spec.Profile, createdAt, deadline)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	go func() {
		<-e.ctx.Done()
		if errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
			m.beginRemoval(e)
		}
	}()
	acquireCtx, finishAcquire := operationContext(ctx, e.ctx)
	vm, acquireErr := m.backend.Acquire(acquireCtx, spec.Profile, deadline)
	if acquireErr == nil {
		acquireErr = context.Cause(acquireCtx)
	}
	var info VMInfo
	var guest Guest
	var vmID string
	if vm != nil {
		vmID = vm.ID()
		guest = vm.Guest()
		if acquireErr == nil {
			if vmID == "" || guest == nil {
				acquireErr = NewError(Internal, "backend returned an incomplete VM", nil)
			} else {
				info, acquireErr = normalizeInfo(vm.Info())
				if acquireErr == nil {
					ready = completeReadySpec(ready, info)
					info.DefaultUser = ready.DefaultUser
				}
			}
		}
	} else if acquireErr == nil {
		acquireErr = NewError(Internal, "backend did not return a VM", nil)
	}
	if vm != nil && vmID != "" {
		m.mu.Lock()
		if m.vms[vmID] != nil {
			// Never tear down a VM already owned by another environment, even
			// if the backend mistakenly offers it a second time.
			vm, guest, vmID = nil, nil, ""
			acquireErr = NewError(Internal, "backend returned an already-owned VM", acquireErr)
		} else {
			m.vms[vmID] = e
		}
		m.mu.Unlock()
	}
	if acquireErr == nil {
		acquireErr = context.Cause(acquireCtx)
	}
	finishAcquire()
	e.mu.Lock()
	e.vm, e.guest, e.info, e.ready = vm, guest, info, ready
	e.vmID = vmID
	if acquireErr == nil && e.state != stateRemoving {
		e.state = stateCreated
	} else if acquireErr == nil {
		acquireErr = context.Cause(e.ctx)
		if acquireErr == nil {
			acquireErr = NewError(FailedPrecondition, "environment is being removed", nil)
		}
	}
	close(e.acquired)
	e.mu.Unlock()
	<-e.gate
	if acquireErr == nil {
		acquireErr = context.Cause(e.ctx)
		if acquireErr == nil {
			acquireErr = ctx.Err()
		}
	}
	if acquireErr != nil {
		return EnvironmentInfo{}, errors.Join(acquireErr, m.removeEntry(e))
	}
	return EnvironmentInfo{ID: e.id, Layout: info.Layout}, nil
}

func (m *Manager) reserve(profile string, createdAt, deadline time.Time) (*environment, error) {
	var entropy [32]byte
	for range 10 {
		if _, err := io.ReadFull(m.random, entropy[:]); err != nil {
			return nil, NewError(Internal, "could not generate an environment ID", err)
		}
		id := base64.RawURLEncoding.EncodeToString(entropy[:])
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, NewError(Unavailable, "execution manager is closed", nil)
		}
		if _, exists := m.environments[id]; exists {
			m.mu.Unlock()
			continue
		}
		lifetimeCtx, cancel := context.WithDeadline(context.Background(), deadline)
		e := &environment{
			id: id, profile: profile, createdAt: createdAt, deadline: deadline,
			ctx: lifetimeCtx, cancel: cancel,
			gate: make(chan struct{}, 1), acquired: make(chan struct{}),
		}
		e.gate <- struct{}{}
		m.environments[id] = e
		m.mu.Unlock()
		m.observeActiveEntries(e.profile, 1)
		return e, nil
	}
	return nil, NewError(Internal, "could not generate a unique environment ID", nil)
}

func (m *Manager) Start(ctx context.Context, id string) (result map[string]string, err error) {
	profile := ""
	defer m.observeOperation(OperationStart, &err, &profile)
	e, opCtx, finish, err := m.operation(ctx, id, false)
	if e != nil {
		profile = e.profile
	}
	if err != nil {
		if e != nil && (KindOf(err) == Cancelled || KindOf(err) == DeadlineExceeded) {
			m.beginRemoval(e)
		}
		return nil, err
	}
	defer finish()
	e.mu.Lock()
	started := e.state == stateStarted
	e.mu.Unlock()
	if !started {
		if _, err := e.guest.Ready(opCtx, e.ready); err != nil {
			m.beginRemoval(e)
			return nil, err
		}
		e.mu.Lock()
		if e.state == stateRemoving {
			e.mu.Unlock()
			if err := context.Cause(e.ctx); err != nil {
				return nil, err
			}
			return nil, NewError(FailedPrecondition, "environment is being removed", nil)
		}
		e.state = stateStarted
		e.mu.Unlock()
	}
	result = maps.Clone(e.info.ImageEnv)
	if _, exists := result["PATH"]; !exists {
		result["PATH"] = e.info.Layout.DefaultPath
	}
	return result, nil
}

func (m *Manager) CopyIn(ctx context.Context, id, destination string, source io.Reader) (err error) {
	profile := ""
	if destination == "" || source == nil {
		profile = m.profileForID(id)
	}
	defer m.observeOperation(OperationCopyIn, &err, &profile)
	if destination == "" || source == nil {
		return NewError(InvalidArgument, "a destination and archive reader are required", nil)
	}
	e, opCtx, finish, err := m.operation(ctx, id, true)
	if e != nil {
		profile = e.profile
	}
	if err != nil {
		return err
	}
	defer finish()
	err = e.guest.CopyIn(opCtx, destination, source)
	if KindOf(err) == Unavailable {
		m.beginRemoval(e)
	}
	return err
}

func (m *Manager) Exec(ctx context.Context, id string, spec ExecSpec, stdout, stderr io.Writer) (result ExecResult, err error) {
	profile := m.profileForID(id)
	defer m.observeExecOperation(&err, &result, &profile)
	if err := validateExecSpec(spec); err != nil {
		return ExecResult{}, err
	}
	e, opCtx, finish, err := m.operation(ctx, id, true)
	if e != nil {
		profile = e.profile
	}
	if err != nil {
		if e != nil && (KindOf(err) == Cancelled || KindOf(err) == DeadlineExceeded) {
			m.beginRemoval(e)
		}
		return ExecResult{}, err
	}
	defer finish()
	if spec.Workspace != "" {
		workspace, err := canonicalWorkspace(spec.Workspace, e.info.Layout.Root)
		if err != nil {
			return ExecResult{}, err
		}
		if e.workspace != "" {
			if e.workspace != workspace {
				return ExecResult{}, NewError(InvalidArgument, "environment workspace cannot be changed", nil)
			}
		} else {
			ready := e.ready
			ready.Directories = append(slices.Clone(ready.Directories), workspace)
			if _, err := e.guest.Ready(opCtx, ready); err != nil {
				m.beginRemoval(e)
				return ExecResult{}, err
			}
			e.mu.Lock()
			if e.state == stateRemoving {
				e.mu.Unlock()
				if err := context.Cause(e.ctx); err != nil {
					return ExecResult{}, err
				}
				return ExecResult{}, NewError(FailedPrecondition, "environment is being removed", nil)
			}
			e.workspace = workspace
			e.mu.Unlock()
		}
	}
	merged := maps.Clone(e.info.ImageEnv)
	if _, exists := merged["PATH"]; !exists {
		merged["PATH"] = e.info.Layout.DefaultPath
	}
	maps.Copy(merged, spec.Env)
	spec.Env = merged
	if spec.User == "" {
		spec.User = e.info.DefaultUser
	}
	if spec.Workdir == "" {
		spec.Workdir = e.info.Layout.Root
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	result, err = e.guest.Exec(opCtx, spec, stdout, stderr)
	// A terminal result (including LaunchError) wins over the caller cancelling
	// its completed stream. Only interrupted execution invalidates ownership.
	m.invalidateOnFailure(e, err)
	return result, err
}

func (m *Manager) CopyOut(ctx context.Context, id, source string, destination io.Writer) (err error) {
	profile := ""
	if source == "" || destination == nil {
		profile = m.profileForID(id)
	}
	defer m.observeOperation(OperationCopyOut, &err, &profile)
	if source == "" || destination == nil {
		return NewError(InvalidArgument, "a source and archive writer are required", nil)
	}
	e, opCtx, finish, err := m.operation(ctx, id, true)
	if e != nil {
		profile = e.profile
	}
	if err != nil {
		return err
	}
	defer finish()
	err = e.guest.CopyOut(opCtx, source, destination)
	if KindOf(err) == Unavailable {
		m.beginRemoval(e)
	}
	return err
}

func (m *Manager) observeOperation(operation Operation, err *error, profile *string) {
	observer := m.options.Observer
	if observer == nil || observer.Operation == nil {
		return
	}
	observer.Operation(*profile, operation, operationOutcome(*err))
}

func (m *Manager) observeExecOperation(err *error, result *ExecResult, profile *string) {
	observer := m.options.Observer
	if observer == nil || observer.Operation == nil {
		return
	}
	outcome := operationOutcome(*err)
	if *err == nil && result.ExitCode != 0 {
		outcome = OutcomeFailure
	}
	observer.Operation(*profile, OperationExec, outcome)
}

func operationOutcome(err error) Outcome {
	if err == nil {
		return OutcomeSuccess
	}
	if KindOf(err) == Cancelled || KindOf(err) == DeadlineExceeded {
		return OutcomeCancelled
	}
	return OutcomeFailure
}

func (m *Manager) profileForID(id string) string {
	observer := m.options.Observer
	if observer == nil || observer.Operation == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.environments[id]; e != nil {
		return e.profile
	}
	return ""
}

func (m *Manager) observeActiveEntries(profile string, delta int) {
	if observer := m.options.Observer; observer != nil && observer.ActiveEntries != nil {
		observer.ActiveEntries(profile, delta)
	}
}

func (m *Manager) observeCleanupFailure(profile string) {
	if observer := m.options.Observer; observer != nil && observer.CleanupFailure != nil {
		observer.CleanupFailure(profile)
	}
}

func (m *Manager) invalidateOnFailure(e *environment, err error) {
	if err == nil {
		return
	}
	var launch *LaunchError
	if errors.As(err, &launch) {
		return
	}
	switch KindOf(err) {
	case Cancelled, DeadlineExceeded, Unavailable:
		m.beginRemoval(e)
	}
}

func (m *Manager) operation(ctx context.Context, id string, requireStarted bool) (*environment, context.Context, func(), error) {
	e, err := m.lookup(id)
	if err != nil {
		return nil, nil, nil, err
	}
	e.mu.Lock()
	err = checkState(e.state, requireStarted)
	e.mu.Unlock()
	if err != nil {
		return e, nil, nil, err
	}
	opCtx, cancel := operationContext(ctx, e.ctx)
	select {
	case e.gate <- struct{}{}:
	case <-opCtx.Done():
		cancel()
		return e, nil, nil, context.Cause(opCtx)
	}
	finish := func() {
		<-e.gate
		cancel()
	}
	if err := context.Cause(opCtx); err != nil {
		finish()
		return e, nil, nil, err
	}
	e.mu.Lock()
	err = checkState(e.state, requireStarted)
	e.mu.Unlock()
	if err != nil {
		finish()
		return e, nil, nil, err
	}
	return e, opCtx, finish, nil
}

func checkState(state environmentState, requireStarted bool) error {
	if state == stateRemoving {
		return NewError(FailedPrecondition, "environment is being removed", nil)
	}
	if state == stateAcquiring || (requireStarted && state != stateStarted) {
		return NewError(FailedPrecondition, "environment has not been started", nil)
	}
	return nil
}

func operationContext(caller, lifetime context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(lifetime)
	stop := context.AfterFunc(caller, func() { cancel(context.Cause(caller)) })
	if err := context.Cause(caller); err != nil {
		cancel(err)
	}
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

func (m *Manager) lookup(id string) (*environment, error) {
	if !validID(id) {
		return nil, NewError(InvalidArgument, "invalid environment ID", nil)
	}
	m.mu.Lock()
	e := m.environments[id]
	m.mu.Unlock()
	if e == nil {
		return nil, NewError(NotFound, "environment not found", nil)
	}
	return e, nil
}

// Remove signals cancellation before waiting for any active operation. Cleanup
// has its own bounded context, even when the caller's context is already done.
// A syntactically valid ID that is no longer present is an idempotent success.
func (m *Manager) Remove(_ context.Context, id string) (err error) {
	profile := m.profileForID(id)
	defer m.observeOperation(OperationRemove, &err, &profile)
	e, err := m.lookup(id)
	if e != nil {
		profile = e.profile
	}
	if err != nil {
		if KindOf(err) == NotFound {
			return nil
		}
		return err
	}
	return m.removeEntry(e)
}

func (m *Manager) removeEntry(e *environment) error {
	attempt := m.beginRemoval(e)
	<-attempt.done
	return attempt.err
}

func (m *Manager) beginRemoval(e *environment) *cleanupAttempt {
	e.mu.Lock()
	e.state = stateRemoving
	e.cancel()
	if e.cleanup != nil {
		select {
		case <-e.cleanup.done:
			if e.destroyed {
				attempt := e.cleanup
				e.mu.Unlock()
				return attempt
			}
		default:
			attempt := e.cleanup
			e.mu.Unlock()
			return attempt
		}
	}
	attempt := &cleanupAttempt{done: make(chan struct{})}
	e.cleanup = attempt
	e.mu.Unlock()
	go m.cleanup(e, attempt)
	return attempt
}

func (m *Manager) cleanup(e *environment, attempt *cleanupAttempt) {
	ctx, cancel := context.WithTimeout(context.Background(), m.options.CleanupTimeout)
	defer cancel()
	var err error
	select {
	case <-e.acquired:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil {
		e.mu.Lock()
		vm, guest := e.vm, e.guest
		e.mu.Unlock()
		if vm != nil {
			cooperationCtx, stopCooperation := context.WithTimeout(ctx, guestCooperationTimeout)
			if guest != nil {
				// A failed Kill does not prevent definitive host-side destruction.
				_ = guest.Kill(cooperationCtx, "")
			}
			gateHeld := false
			select {
			case e.gate <- struct{}{}:
				gateHeld = true
			case <-cooperationCtx.Done():
			}
			stopCooperation()
			// An uncooperative operation cannot hold a live VM indefinitely.
			err = vm.Destroy(ctx)
			if gateHeld {
				<-e.gate
			}
		}
	}
	e.mu.Lock()
	activeDelta := false
	if err == nil {
		e.destroyed = true
		m.mu.Lock()
		if m.environments[e.id] == e {
			delete(m.environments, e.id)
			activeDelta = true
		}
		if m.vms[e.vmID] == e {
			delete(m.vms, e.vmID)
		}
		m.mu.Unlock()
	} else {
		attempt.err = NewError(KindOf(err), "environment cleanup is incomplete", err)
	}
	failed := err != nil
	e.mu.Unlock()
	if activeDelta {
		m.observeActiveEntries(e.profile, -1)
	}
	if failed {
		m.observeCleanupFailure(e.profile)
	}
	close(attempt.done)
}

// RetryRemovals retries retained, incomplete cleanup without touching healthy
// environments. It is suitable for the host's periodic reconciliation loop.
func (m *Manager) RetryRemovals(ctx context.Context) error {
	m.mu.Lock()
	entries := make([]*environment, 0, len(m.environments))
	for _, e := range m.environments {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	attempts := make([]*cleanupAttempt, 0, len(entries))
	for _, e := range entries {
		e.mu.Lock()
		removing := e.state == stateRemoving
		e.mu.Unlock()
		if removing {
			attempts = append(attempts, m.beginRemoval(e))
		}
	}
	var result error
	for _, attempt := range attempts {
		select {
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		case <-attempt.done:
		}
		result = errors.Join(result, attempt.err)
	}
	return result
}

// Close prevents acquisition, cancels every environment first, and waits for
// independent bounded cleanup. Calling it again retries incomplete destruction.
func (m *Manager) Close(_ context.Context) error {
	m.mu.Lock()
	m.closed = true
	entries := make([]*environment, 0, len(m.environments))
	for _, e := range m.environments {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	attempts := make([]*cleanupAttempt, len(entries))
	for i, e := range entries {
		attempts[i] = m.beginRemoval(e)
	}
	var result error
	for _, attempt := range attempts {
		<-attempt.done
		result = errors.Join(result, attempt.err)
	}
	return result
}

func validID(id string) bool {
	if len(id) != 43 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	// A 32-byte unpadded base64 encoding has two zero padding bits.
	return strings.IndexByte("AEIMQUYcgkosw048", id[42]) >= 0
}

func normalizeInfo(info VMInfo) (VMInfo, error) {
	architecture := info.Layout.Arch
	if architecture == "" {
		architecture = runtime.GOARCH
	}
	layout, err := DefaultLayout(architecture)
	if err != nil {
		return VMInfo{}, err
	}
	provided := info.Layout
	if (provided.Root != "" && provided.Root != layout.Root) ||
		(provided.Act != "" && provided.Act != layout.Act) ||
		(provided.ToolCache != "" && provided.ToolCache != layout.ToolCache) ||
		(provided.Temp != "" && provided.Temp != layout.Temp) ||
		(provided.OS != "" && provided.OS != layout.OS) ||
		(provided.DefaultPath != "" && provided.DefaultPath != layout.DefaultPath) {
		return VMInfo{}, NewError(InvalidArgument, "backend supplied a noncanonical guest layout", nil)
	}
	info.Layout = layout
	info.ImageEnv = maps.Clone(info.ImageEnv)
	if info.ImageEnv == nil {
		info.ImageEnv = make(map[string]string)
	}
	if info.DefaultUser == "" {
		info.DefaultUser = DefaultUser
	}
	return info, nil
}

func validateReadySpec(spec ReadySpec) error {
	if spec.MaxTransferBytes < 0 || spec.MaxArchiveEntries < 0 || strings.ContainsRune(spec.DefaultUser, 0) {
		return NewError(InvalidArgument, "invalid guest readiness settings", nil)
	}
	for _, directory := range spec.Directories {
		if directory == "" || strings.ContainsRune(directory, 0) {
			return NewError(InvalidArgument, "invalid guest readiness directory", nil)
		}
		for component := range strings.SplitSeq(directory, "/") {
			if component == ".." {
				return NewError(InvalidArgument, "invalid guest readiness directory", nil)
			}
		}
		clean := path.Clean(directory)
		if clean != WorkspaceRoot && !strings.HasPrefix(clean, WorkspaceRoot+"/") {
			return NewError(InvalidArgument, "readiness directories must be inside the workspace", nil)
		}
	}
	return nil
}

func completeReadySpec(spec ReadySpec, info VMInfo) ReadySpec {
	directories := []string{info.Layout.Root, info.Layout.Act, info.Layout.ToolCache, info.Layout.Temp}
	for _, directory := range spec.Directories {
		directory = path.Clean(directory)
		if !slices.Contains(directories, directory) {
			directories = append(directories, directory)
		}
	}
	spec.Directories = directories
	if spec.DefaultUser == "" {
		spec.DefaultUser = info.DefaultUser
	}
	if spec.MaxTransferBytes == 0 {
		spec.MaxTransferBytes = DefaultMaxTransferBytes
	}
	if spec.MaxArchiveEntries == 0 {
		spec.MaxArchiveEntries = DefaultMaxArchiveEntries
	}
	return spec
}

func validateExecSpec(spec ExecSpec) error {
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return NewError(InvalidArgument, "execution requires a command", nil)
	}
	for _, argument := range spec.Command {
		if strings.ContainsRune(argument, 0) {
			return NewError(InvalidArgument, "command arguments cannot contain NUL", nil)
		}
	}
	for key, value := range spec.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return NewError(InvalidArgument, "invalid execution environment entry", nil)
		}
	}
	if strings.ContainsRune(spec.User, 0) || strings.ContainsRune(spec.Workdir, 0) {
		return NewError(InvalidArgument, "invalid execution user or working directory", nil)
	}
	return nil
}
func canonicalWorkspace(workspace, root string) (string, error) {
	if strings.ContainsRune(workspace, 0) {
		return "", NewError(InvalidArgument, "workspace path contains NUL", nil)
	}
	for component := range strings.SplitSeq(workspace, "/") {
		if component == ".." {
			return "", NewError(InvalidArgument, "workspace path cannot traverse its root", nil)
		}
	}
	clean := path.Clean(workspace)
	if !path.IsAbs(workspace) || (clean != root && !strings.HasPrefix(clean, root+"/")) {
		return "", NewError(InvalidArgument, "workspace must be inside the guest workspace root", nil)
	}
	return clean, nil
}
