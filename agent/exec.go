package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ALameLlama/fireactions/internal/executor"
	"github.com/ALameLlama/fireactions/internal/guestfs"
	agentv1 "github.com/ALameLlama/fireactions/proto/agent/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	processChunkBytes = 32 * 1024
	processQueueDepth = 8
	processTermGrace  = 2 * time.Second
	processPipeDrain  = 2 * time.Second
)

type processOutput struct {
	stream agentv1.ExecStream
	data   []byte
	err    error
}

type processScope struct {
	id       string
	cmd      *exec.Cmd
	cgroup   string
	exited   chan struct{}
	mu       sync.Mutex
	finished bool
}
type execManager struct {
	cgroups          *cgroupScopes
	fs               *guestfs.RootFS
	workspace        string
	identity         func(string) (Identity, error)
	mu               sync.Mutex
	scopes           map[string]*processScope
	closing          bool
	selectedIdentity func() Identity
}

func newExecManager(a *Agent) *execManager {
	return &execManager{fs: a.fs, workspace: filepath.Clean(a.cfg.WorkspaceRoot), identity: resolveIdentity, selectedIdentity: a.currentIdentity, scopes: make(map[string]*processScope)}
}

// validateProcessReadinessLocked is for Ready handlers that already hold the
// process manager mutex while serializing identity/settings publication.
func (a *Agent) validateProcessReadinessLocked() error {
	if a.processes == nil {
		return errors.New("process manager is not initialized")
	}
	cg, err := newCgroupScopes()
	if err != nil {
		return err
	}
	if a.processes.cgroups != nil && a.processes.cgroups.root == cg.root {
		return nil
	}
	if len(a.processes.scopes) != 0 {
		return errors.New("cannot change process cgroup root while scopes exist")
	}
	a.processes.cgroups = cg
	return nil
}

func (m *execManager) exec(stream agentv1.AgentService_ExecServer, req *agentv1.ExecRequest) error {
	ctx := stream.Context()
	if len(req.Command) == 0 || req.Command[0] == "" {
		return status.Error(codes.InvalidArgument, "command is empty")
	}
	for _, arg := range req.Command {
		if strings.IndexByte(arg, 0) >= 0 {
			return status.Error(codes.InvalidArgument, "command contains NUL")
		}
	}
	for k, v := range req.Env {
		if !validEnvKey(k) || strings.IndexByte(v, 0) >= 0 {
			return status.Error(codes.InvalidArgument, "invalid environment")
		}
	}
	m.mu.Lock()
	managerLocked := true
	defer func() {
		if managerLocked {
			m.mu.Unlock()
		}
	}()
	identity := m.selectedIdentity()
	var err error
	if req.User != "" {
		identity, err = m.identity(req.User)
	}
	if err != nil {
		return sendFailed(stream, fmt.Errorf("resolve execution identity: %w", err))
	}
	cwdPath := m.workspace
	if req.Workdir != "" {
		rel, e := m.fs.ResolvePath(req.Workdir)
		if e != nil {
			return status.Error(codes.InvalidArgument, e.Error())
		}
		cwdPath = filepath.Join(m.workspace, rel)
	}
	cwdFD, err := m.workdir(req.Workdir)
	if err != nil {
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ELOOP) {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return sendFailed(stream, &executor.LaunchError{Message: err.Error()})
	}
	defer cwdFD.Close()
	pathValue, hasPath := req.Env["PATH"]
	if !hasPath {
		pathValue = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	argv := append([]string(nil), req.Command...)
	env := buildEnv(req.Env, identity)
	id := req.ProcessId
	if !validProcessID(id) {
		return status.Error(codes.InvalidArgument, "invalid process id")
	}
	// Go changes cwd before remapping ExtraFiles. Pin the original descriptor;
	// CLOEXEC prevents the workspace handle leaking into the workflow process.
	cmdDir := "/proc/self/fd/" + strconv.Itoa(int(cwdFD.Fd()))
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return sendFailed(stream, err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return sendFailed(stream, err)
	}
	stdout := &processPipe{File: stdoutR}
	stderr := &processPipe{File: stderrR}
	cg := m.cgroups
	if m.closing {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return status.Error(codes.FailedPrecondition, "agent is shutting down")
	}
	if cg == nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return status.Error(codes.FailedPrecondition, "guest process cgroups are not ready")
	}
	cgFD, cgPath, err := cg.create(id)
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return status.Errorf(codes.FailedPrecondition, "create process scope: %v", err)
	}
	credentials := &syscall.Credential{Uid: identity.UID, Gid: identity.GID, Groups: identity.Groups}
	var cmd *exec.Cmd
	_, err = resolveExecutable(req.Command[0], pathValue, cwdPath, func(path string) error {
		argv[0] = path
		// Start failures leave private state in exec.Cmd. Each attempt gets a
		// fresh command, but shares the still-open pipes and cgroup descriptor.
		cmd = &exec.Cmd{
			Path: path, Args: argv, Dir: cmdDir, Env: env,
			Stdout: stdoutW, Stderr: stderrW,
			SysProcAttr: &syscall.SysProcAttr{Setpgid: true, Credential: credentials, UseCgroupFD: true, CgroupFD: int(cgFD.Fd())},
		}
		return cmd.Start()
	})
	if err != nil {
		_ = cgFD.Close()
		_ = cg.remove(id)
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return sendFailed(stream, &executor.LaunchError{Message: "start process: " + err.Error()})
	}
	_ = cgFD.Close()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	scope := &processScope{id: id, cmd: cmd, cgroup: cgPath, exited: make(chan struct{})}
	m.scopes[id] = scope
	outputs := make(chan processOutput, processQueueDepth)
	readDone := make(chan struct{}, 2)
	readCtx, stopReaders := context.WithCancel(ctx)
	go readProcessPipe(readCtx, stdout, agentv1.ExecStream_STDOUT, outputs, readDone)
	go readProcessPipe(readCtx, stderr, agentv1.ExecStream_STDERR, outputs, readDone)
	wait := make(chan error, 1)
	go func() {
		e := cmd.Wait()
		stdout.beginDrain()
		stderr.beginDrain()
		close(scope.exited)
		wait <- e
	}()
	m.mu.Unlock()
	managerLocked = false
	defer func() { stopReaders(); _ = stdoutR.Close(); _ = stderrR.Close() }()
	defer m.forget(scope)
	readers := 2
	var waitErr, pipeErr error
	childDone := false
	for !childDone || readers > 0 || len(outputs) > 0 {
		select {
		case <-ctx.Done():
			reapProcess(scope, cg, wait, &waitErr, &childDone, stopReaders, stdoutR, stderrR, readDone, &readers)
			return status.FromContextError(ctx.Err()).Err()
		case e := <-wait:
			waitErr = e
			childDone = true
		case item := <-outputs:
			if item.err != nil {
				if pipeErr == nil {
					pipeErr = item.err
				}
				continue
			}
			if err := stream.Send(dataOutput(item.stream, item.data)); err != nil {
				reapProcess(scope, cg, wait, &waitErr, &childDone, stopReaders, stdoutR, stderrR, readDone, &readers)
				return err
			}
		case <-readDone:
			readers--
		}
	}
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if pipeErr != nil {
		return status.Errorf(codes.Internal, "read process output: %v", pipeErr)
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			code := exit.ExitCode()
			if code < 0 {
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					code = 128 + int(status.Signal())
				}
			}
			return stream.Send(completeOutput(int32(code)))
		}
		return status.Errorf(codes.Internal, "wait for process: %v", waitErr)
	}
	return stream.Send(completeOutput(0))
}

func reapProcess(scope *processScope, cg *cgroupScopes, wait <-chan error, waitErr *error, childDone *bool, stop context.CancelFunc, stdout, stderr io.ReadCloser, readDone <-chan struct{}, readers *int) {
	_ = terminateScope(scope, cg)
	if !*childDone {
		*waitErr = <-wait
		*childDone = true
	}
	stop()
	closePipe(stdout)
	closePipe(stderr)
	drainReaders(readDone, readers, processPipeDrain)
}

// processPipe bounds idle reads after the foreground process exits, not time
// spent delivering output. A blocked consumer must never discard queued bytes
// or prevent the remaining pipe contents from being read when it resumes.
type processPipe struct {
	*os.File
	mu       sync.Mutex
	draining bool
}

func (p *processPipe) beginDrain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.draining = true
	_ = p.SetReadDeadline(time.Now().Add(processPipeDrain))
}

func (p *processPipe) Read(buf []byte) (int, error) {
	p.mu.Lock()
	if p.draining {
		_ = p.SetReadDeadline(time.Now().Add(processPipeDrain))
	}
	p.mu.Unlock()
	n, err := p.File.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// Only an idle Read expires. Data already read is still delivered by
		// readProcessPipe, and its next Read receives a fresh idle deadline.
		err = io.EOF
	}
	return n, err
}

func readProcessPipe(ctx context.Context, r io.ReadCloser, which agentv1.ExecStream, out chan<- processOutput, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buf := make([]byte, processChunkBytes)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			select {
			case out <- processOutput{stream: which, data: data}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err != io.EOF {
				select {
				case out <- processOutput{err: err}:
				case <-ctx.Done():
				}
			}
			return
		}
	}
}
func drainReaders(ch <-chan struct{}, n *int, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for *n > 0 {
		select {
		case <-ch:
			*n--
		case <-timer.C:
			return
		}
	}
}
func closePipe(r io.ReadCloser) { _ = r.Close() }
func (m *execManager) workdir(request string) (*os.File, error) {
	rel := "."
	var err error
	if request != "" {
		rel, err = m.fs.ResolvePath(request)
		if err != nil {
			return nil, err
		}
	}
	root, err := m.fs.Root().Open(".")
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()
	fd, err := unix.Openat2(int(root.Fd()), rel, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, fmt.Errorf("open workdir: %w", err)
	}
	return os.NewFile(uintptr(fd), "guest-workdir"), nil
}
func validProcessID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// resolveExecutable starts candidates under the execution identity. Stat only
// filters obviously unusable PATH entries; the kernel decides access, including
// ACLs, supplementary groups, mount flags and search permission on directories.
func resolveExecutable(name, pathValue, dir string, start func(string) error) (string, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return "", errors.New("executable contains NUL")
	}
	if strings.ContainsRune(name, os.PathSeparator) {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return p, start(p)
	}
	var accessErr error
	for _, d := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if d == "" {
			d = "."
		}
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		candidate := filepath.Join(d, name)
		st, err := os.Stat(candidate)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
			continue
		}
		if err := start(candidate); err != nil {
			if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
				accessErr = err
				continue
			}
			return "", err
		}
		return candidate, nil
	}
	if accessErr != nil {
		return "", accessErr
	}
	return "", errors.New("executable not found")
}
func buildEnv(env map[string]string, id Identity) []string {
	values := make(map[string]string, len(env)+3)
	for k, v := range env {
		values[k] = v
	}
	if _, ok := values["HOME"]; !ok {
		values["HOME"] = id.Home
	}
	if _, ok := values["USER"]; !ok {
		values["USER"] = id.Username
	}
	if _, ok := values["SHELL"]; !ok {
		values["SHELL"] = id.Shell
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out
}
func validEnvKey(k string) bool { return k != "" && !strings.ContainsAny(k, "=\x00") }
func sendFailed(stream agentv1.AgentService_ExecServer, err error) error {
	return stream.Send(failedOutput(err.Error()))
}
func failedOutput(msg string) *agentv1.ExecOutput {
	return &agentv1.ExecOutput{Result: &agentv1.ExecOutput_Failed{Failed: &agentv1.ExecFailed{ErrorMessage: msg}}}
}
func dataOutput(stream agentv1.ExecStream, data []byte) *agentv1.ExecOutput {
	return &agentv1.ExecOutput{Result: &agentv1.ExecOutput_Data{Data: &agentv1.ExecData{Stream: stream, Data: data}}}
}
func completeOutput(code int32) *agentv1.ExecOutput {
	return &agentv1.ExecOutput{Result: &agentv1.ExecOutput_Complete{Complete: &agentv1.ExecComplete{ExitCode: code}}}
}
func terminateScope(scope *processScope, cgroups *cgroupScopes) error {
	scope.mu.Lock()
	finished := scope.finished
	scope.mu.Unlock()
	if !finished {
		select {
		case <-scope.exited:
		default:
			_ = signalProcessGroup(scope.cmd.Process.Pid, unix.SIGTERM)
			timer := time.NewTimer(processTermGrace)
			select {
			case <-scope.exited:
			case <-timer.C:
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
	if err := cgroups.kill(scope.id); err != nil {
		return err
	}
	return cgroups.waitEmpty(scope.id, processTermGrace)
}
func (m *execManager) forget(scope *processScope) {
	scope.mu.Lock()
	scope.finished = true
	scope.mu.Unlock()
	m.mu.Lock()
	cg := m.cgroups
	registered := m.scopes[scope.id] == scope
	m.mu.Unlock()
	if cg == nil || !registered {
		return
	}
	events, err := os.ReadFile(filepath.Join(scope.cgroup, "cgroup.events"))
	if err != nil || !strings.Contains(string(events), "populated 0") {
		return
	}
	if err = cg.remove(scope.id); err != nil {
		return
	}
	m.mu.Lock()
	if m.scopes[scope.id] == scope {
		delete(m.scopes, scope.id)
	}
	m.mu.Unlock()
}
func (m *execManager) shutdown() error {
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	return m.killContext(context.Background(), "")
}
func (m *execManager) killContext(ctx context.Context, id string) error {
	m.mu.Lock()
	targets := make([]*processScope, 0)
	if id == "" {
		for _, s := range m.scopes {
			targets = append(targets, s)
		}
	} else if s := m.scopes[id]; s != nil {
		targets = append(targets, s)
	}
	cg := m.cgroups
	m.mu.Unlock()
	if cg == nil {
		if len(targets) == 0 {
			return nil
		}
		return errors.New("process cgroup is unavailable")
	}
	if id != "" {
		if len(targets) == 0 {
			return nil
		}
		return m.killOne(ctx, targets[0], cg)
	}
	return m.killMany(ctx, targets, cg)
}
func (m *execManager) killOne(ctx context.Context, scope *processScope, cg *cgroupScopes) error {
	if !scopeExited(scope) {
		_ = signalProcessGroup(scope.cmd.Process.Pid, unix.SIGTERM)
		waitScopeExit(ctx, []*processScope{scope}, processTermGrace)
	}
	if err := cg.kill(scope.id); err != nil {
		return err
	}
	if ctx.Err() == nil {
		if err := waitCgroupsEmpty(ctx, cg, []*processScope{scope}, processTermGrace); err != nil {
			return err
		}
	}
	if err := cg.remove(scope.id); err != nil {
		return err
	}
	m.mu.Lock()
	if m.scopes[scope.id] == scope {
		delete(m.scopes, scope.id)
	}
	m.mu.Unlock()
	return ctx.Err()
}
func (m *execManager) killMany(ctx context.Context, scopes []*processScope, cg *cgroupScopes) error {
	if ctx.Err() == nil {
		for _, scope := range scopes {
			if !scopeExited(scope) {
				_ = signalProcessGroup(scope.cmd.Process.Pid, unix.SIGTERM)
			}
		}
		waitScopeExit(ctx, scopes, processTermGrace)
	}
	var errs []error
	for _, scope := range scopes {
		if err := cg.kill(scope.id); err != nil {
			errs = append(errs, err)
		}
	}
	if err := cg.killAll(); err != nil {
		errs = append(errs, err)
	}
	if ctx.Err() == nil {
		if err := waitCgroupsEmpty(ctx, cg, scopes, processTermGrace); err != nil {
			errs = append(errs, err)
		}
	}
	for _, scope := range scopes {
		if !scopeExited(scope) {
			continue
		}
		if empty, err := cgroupEmpty(scope.cgroup); err == nil && empty {
			if err := cg.remove(scope.id); err != nil {
				errs = append(errs, err)
				continue
			}
			m.mu.Lock()
			if m.scopes[scope.id] == scope {
				delete(m.scopes, scope.id)
			}
			m.mu.Unlock()
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
func scopeExited(scope *processScope) bool {
	select {
	case <-scope.exited:
		return true
	default:
		return false
	}
}
func waitScopeExit(ctx context.Context, scopes []*processScope, timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		done := true
		for _, scope := range scopes {
			if !scopeExited(scope) {
				done = false
				break
			}
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}
func cgroupEmpty(path string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return false, err
	}
	return strings.Contains(string(data), "populated 0"), nil
}
func waitCgroupsEmpty(ctx context.Context, cg *cgroupScopes, scopes []*processScope, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		empty := true
		for _, scope := range scopes {
			if !scopeExited(scope) {
				empty = false
				continue
			}
			if _, tracked := cg.lookup(scope.id); !tracked {
				continue
			}
			scopeEmpty, err := cgroupEmpty(scope.cgroup)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !scopeEmpty {
				empty = false
			}
		}
		if empty {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("process cgroups remained populated after kill")
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-tick.C:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			return errors.New("process cgroups remained populated after kill")
		}
	}
}

func (a *Agent) Kill(ctx context.Context, req *agentv1.KillRequest) (*agentv1.KillResponse, error) {
	if req.ProcessId != "" && !validProcessID(req.ProcessId) {
		return nil, status.Error(codes.InvalidArgument, "invalid process id")
	}
	if a.processes == nil {
		return nil, status.Error(codes.FailedPrecondition, "process manager unavailable")
	}
	if err := a.processes.killContext(ctx, req.ProcessId); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, status.FromContextError(err).Err()
		}
		return nil, status.Errorf(codes.Internal, "kill process scope: %v", err)
	}
	return &agentv1.KillResponse{}, nil
}
