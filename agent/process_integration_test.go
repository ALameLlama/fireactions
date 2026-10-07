package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
	"github.com/hostinger/fireactions/internal/guest"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func integrationGuest(t *testing.T) (*guest.Client, *Agent, string) {
	t.Helper()
	if os.Getenv("FIREACTIONS_TEST_CGROUP") != "1" {
		t.Skip("requires root in a delegated cgroup v2 service")
	}
	identity, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a, err := New(Config{Port: 9001, LogLevel: "info", WorkspaceRoot: root, DefaultUser: identity.Username}, func(a *Agent) { a.logFile = filepath.Join(root, "agent.log") })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	listener := bufconn.Listen(64 * 1024)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, a)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:execution", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	client := guest.New(conn)
	t.Cleanup(func() { client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = client.Ready(ctx, executor.ReadySpec{DefaultUser: identity.Username, Directories: []string{"/workspace/sub"}}); err != nil {
		t.Fatal(err)
	}
	return client, a, root
}

func TestGuestExecutionIntegration(t *testing.T) {
	client, _, root := integrationGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	script := []byte("#!/bin/sh\nprintf '%s|%s|%s|%s|%s|%s\\n' \"$1\" \"$SMOKE_VALUE\" \"${DAEMON_SECRET-unset}\" \"$HOME\" \"$USER\" \"$SHELL\"\npwd\nprintf 'stderr-marker\\n' >&2\nexit 42\n")
	if err := tw.WriteHeader(&tar.Header{Name: "tool", Mode: 0755, Size: int64(len(script))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(script); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.CopyIn(ctx, "/workspace/sub", &archive); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAEMON_SECRET", "must-not-be-inherited")
	var stdout, stderr bytes.Buffer
	spec := executor.ExecSpec{Command: []string{"tool", "$(touch injection); *"}, Env: map[string]string{"PATH": filepath.Join(root, "sub") + ":/usr/bin:/bin", "SMOKE_VALUE": "explicit"}, Workdir: "/workspace/sub"}
	result, err := client.Exec(ctx, spec, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolveIdentity(current.Username)
	if err != nil {
		t.Fatal(err)
	}
	expected := "$(touch injection); *|explicit|unset|" + selected.Home + "|" + selected.Username + "|" + selected.Shell + "\n" + filepath.Join(root, "sub") + "\n"
	if result.ExitCode != 42 || stdout.String() != expected || stderr.String() != "stderr-marker\n" {
		t.Fatalf("execution contract: exit=%d stdout=%q stderr=%q", result.ExitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "injection")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("argv was interpreted by a shell")
	}
	_, err = client.Exec(ctx, executor.ExecSpec{Command: []string{"definitely-missing"}, Env: map[string]string{"PATH": "/missing"}}, io.Discard, io.Discard)
	var launch *executor.LaunchError
	if !errors.As(err, &launch) {
		t.Fatalf("missing executable not a launch failure: %v", err)
	}
	_, err = client.Exec(ctx, executor.ExecSpec{Command: []string{"/bin/true"}, Workdir: "/workspace/missing"}, io.Discard, io.Discard)
	if !errors.As(err, &launch) {
		t.Fatalf("missing workdir not a launch failure: %v", err)
	}
	start := time.Now()
	result, err = client.Exec(ctx, executor.ExecSpec{Command: []string{"/bin/sh", "-c", "sleep 300 & exit 0"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}}, io.Discard, io.Discard)
	if err != nil || result.ExitCode != 0 || time.Since(start) > 4*time.Second {
		t.Fatalf("background inherited pipe hung or changed exit: %v %#v", err, result)
	}
	if err = client.Kill(ctx, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGuestExplicitUserIntegration(t *testing.T) {
	client, _, _ := integrationGuest(t)
	account, err := user.LookupId("1000")
	if err != nil {
		t.Skip("requires an existing unprivileged UID 1000")
	}
	group, err := user.LookupGroup("daemon")
	if err != nil {
		t.Skip("requires an alternate existing group")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := client.Exec(ctx, executor.ExecSpec{Command: []string{"/usr/bin/id", "-u"}, User: account.Uid}, &stdout, io.Discard)
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(stdout.String()) != account.Uid {
		t.Fatalf("numeric execution UID: result=%#v stdout=%q err=%v", result, stdout.String(), err)
	}
	stdout.Reset()
	result, err = client.Exec(ctx, executor.ExecSpec{Command: []string{"/usr/bin/id", "-g"}, User: account.Username + ":" + group.Gid}, &stdout, io.Discard)
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(stdout.String()) != group.Gid {
		t.Fatalf("named user/numeric group: result=%#v stdout=%q err=%v", result, stdout.String(), err)
	}
	_, err = client.Exec(ctx, executor.ExecSpec{Command: []string{"/bin/true"}, User: "fireactions-nonexistent-identity"}, io.Discard, io.Discard)
	var launch *executor.LaunchError
	if !errors.As(err, &launch) {
		t.Fatalf("unknown identity did not fail launch: %v", err)
	}
}

func TestGuestReadyRejectsChangesWhileScopeLives(t *testing.T) {
	client, a, root := integrationGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	spec := executor.ReadySpec{DefaultUser: currentUsername(t), Directories: []string{"/workspace/sub"}}
	if _, err := client.Ready(ctx, spec); err != nil {
		t.Fatalf("identical Ready failed: %v", err)
	}
	result, err := client.Exec(ctx, executor.ExecSpec{
		Command: []string{"/bin/sh", "-c", "sleep 300 & exit 0"},
		Env:     map[string]string{"PATH": "/usr/bin:/bin"},
	}, io.Discard, io.Discard)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("start background scope: result=%#v err=%v", result, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		clientAgentScopes := processScopesForRoot(t, a)
		if clientAgentScopes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background process scope was not retained")
		}
		time.Sleep(10 * time.Millisecond)
	}

	marker := filepath.Join(root, "sub", "ownership-marker")
	if err := os.Mkdir(marker, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(marker, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	changed := executor.ReadySpec{
		DefaultUser:       spec.DefaultUser,
		Directories:       []string{"/workspace/sub/ownership-marker", "/workspace/sub/new-directory"},
		MaxTransferBytes:  123456,
		MaxArchiveEntries: 17,
	}
	if _, err := client.Ready(ctx, changed); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("changed Ready while scope lives: got %v, want FailedPrecondition", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "new-directory")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected Ready created directory: %v", err)
	}
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 65534 || stat.Gid != 65534 {
		t.Fatalf("rejected Ready changed ownership: uid=%d gid=%d", stat.Uid, stat.Gid)
	}
	if err := client.Kill(ctx, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGuestDefaultIdentityUsesReadyGroup(t *testing.T) {
	client, _, _ := integrationGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	var alternate *user.Group
	for _, name := range []string{"daemon", "bin", "nobody", "nogroup", "users"} {
		candidate, lookupErr := user.LookupGroup(name)
		if lookupErr == nil && candidate.Gid != current.Gid {
			alternate = candidate
			break
		}
	}
	if alternate == nil {
		t.Skip("system has no alternate existing group")
	}
	groupSpec := current.Username + ":" + alternate.Name
	if _, err := client.Ready(ctx, executor.ReadySpec{DefaultUser: groupSpec, Directories: []string{"/workspace/sub"}}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	result, err := client.Exec(ctx, executor.ExecSpec{Command: []string{"id", "-G"}}, &stdout, io.Discard)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("id -G: result=%#v err=%v", result, err)
	}
	selected, err := resolveIdentity(groupSpec)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]bool{selected.GID: true}
	for _, gid := range selected.Groups {
		want[gid] = true
	}
	got := make(map[uint32]bool)
	for _, field := range strings.Fields(stdout.String()) {
		value, err := strconv.ParseUint(field, 10, 32)
		if err != nil {
			t.Fatalf("invalid id -G output %q: %v", field, err)
		}
		got[uint32(value)] = true
	}
	if len(got) != len(want) {
		t.Fatalf("supplementary groups mismatch: got %v want %v", got, want)
	}
	for gid := range want {
		if !got[gid] {
			t.Fatalf("supplementary groups mismatch: got %v want %v", got, want)
		}
	}
}

func currentUsername(t *testing.T) string {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return current.Username
}

func processScopesForRoot(t *testing.T, a *Agent) int {
	t.Helper()
	a.processes.mu.Lock()
	defer a.processes.mu.Unlock()
	return len(a.processes.scopes)
}

type cancellationWriter struct {
	cancel context.CancelFunc
	seen   chan struct{}
	once   bool
}

func (w *cancellationWriter) Write(p []byte) (int, error) {
	if !w.once && strings.Contains(string(p), "scope-ready") {
		w.once = true
		close(w.seen)
		w.cancel()
	}
	return len(p), nil
}

func TestGuestCancellationKillsSilentProcessIntegration(t *testing.T) {
	client, a, _ := integrationGuest(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, executor.ExecSpec{Command: []string{"/bin/sleep", "300"}}, io.Discard, io.Discard)
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for processScopesForRoot(t, a) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("silent process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("silent process cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("silent process cancellation blocked")
	}
	deadline = time.Now().Add(3 * time.Second)
	for processScopesForRoot(t, a) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("silent process scope survived cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGuestCancellationKillsSetsidIntegration(t *testing.T) {
	client, a, root := integrationGuest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &cancellationWriter{cancel: cancel, seen: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := client.Exec(ctx, executor.ExecSpec{Command: []string{"/bin/sh", "-c", "setsid sh -c 'echo $$ > child.pid; exec sleep 300' & while [ ! -s child.pid ]; do sleep 0.01; done; echo scope-ready; sleep 300"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}}, sink, io.Discard)
		result <- err
	}()
	select {
	case <-sink.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not start setsid descendant")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("setsid execution cancellation blocked")
	}
	cleanup, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCleanup()
	if err := client.Kill(cleanup, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err = os.Stat("/proc/" + pid)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setsid child %s survived cgroup kill: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if remaining := processScopesForRoot(t, a); remaining != 0 {
		t.Fatalf("%d scopes survived cancellation", remaining)
	}
}

type pausedExecStream struct {
	grpc.ServerStream
	ctx      context.Context
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	complete *agentv1.ExecComplete
}

func (s *pausedExecStream) Context() context.Context { return s.ctx }

func (s *pausedExecStream) Send(out *agentv1.ExecOutput) error {
	if data := out.GetData(); data != nil {
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.release:
			case <-s.ctx.Done():
			}
		})
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if data.Stream == agentv1.ExecStream_STDOUT {
			s.stdout.Write(data.Data)
		} else {
			s.stderr.Write(data.Data)
		}
	} else if complete := out.GetComplete(); complete != nil {
		s.complete = complete
	} else if failed := out.GetFailed(); failed != nil {
		return errors.New(failed.ErrorMessage)
	}
	return nil
}

func TestGuestFinalBurstSurvivesPausedSendIntegration(t *testing.T) {
	_, a, _ := integrationGuest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream := &pausedExecStream{ctx: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	const id = "12345678901234567890123456789012"
	const bursts = processQueueDepth + 3 // Send, queue, blocked reader, final pipe bytes.
	go func() {
		done <- a.processes.exec(stream, &agentv1.ExecRequest{
			ProcessId: id,
			Command: []string{"/bin/sh", "-c",
				"i=0; while [ \"$i\" -lt " + strconv.Itoa(bursts) + " ]; do dd if=/dev/zero bs=4096 count=1 2>/dev/null; i=$((i+1)); sleep 0.05; done"},
			Env: map[string]string{"PATH": "/usr/bin:/bin"},
		})
	}()
	select {
	case <-stream.entered:
	case <-ctx.Done():
		t.Fatal("execution did not reach blocked output Send")
	}
	a.processes.mu.Lock()
	scope := a.processes.scopes[id]
	a.processes.mu.Unlock()
	if scope == nil {
		t.Fatal("output-producing process did not publish its scope")
	}
	select {
	case <-scope.exited:
	case <-ctx.Done():
		t.Fatal("foreground could not finish its final burst with Send paused")
	}
	// The original post-Wait timer expired while Send was blocked and then
	// cancelled readers with one chunk pending and one still in the pipe.
	time.Sleep(processPipeDrain + 250*time.Millisecond)
	close(stream.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("execution did not finish after output Send resumed")
	}
	if stream.complete == nil || stream.complete.ExitCode != 0 {
		t.Fatalf("successful final burst did not complete: %#v", stream.complete)
	}
	if want := bursts * 4096; stream.stdout.Len() != want || stream.stderr.Len() != 0 {
		t.Fatalf("final burst output = %d stdout/%d stderr bytes, want %d/0", stream.stdout.Len(), stream.stderr.Len(), want)
	}
}
