//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/errdefs"
	sdkvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/hostinger/fireactions/internal/executor"
	"github.com/hostinger/fireactions/internal/guest"
	pluginv1alpha "github.com/hostinger/fireactions/proto/forgejo/plugin/v1alpha"
	serverv1 "github.com/hostinger/fireactions/proto/server/v1"
	"github.com/hostinger/fireactions/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"gopkg.in/yaml.v3"
)

type pluginHarness struct {
	t              *testing.T
	root           string
	config         *server.Config
	configPath     string
	binary         string
	process        *exec.Cmd
	exited         chan error
	conn           *grpc.ClientConn
	plugin         pluginv1alpha.BackendPluginClient
	admin          serverv1.ServerServiceClient
	containerd     *containerd.Client
	namespaceOwned bool
	ctx            context.Context
	baseSnapshots  map[string]bool
	baseLeases     map[string]bool
	stopOnce       sync.Once
}

func newPluginHarness(t *testing.T, configure ...func(*server.Config)) *pluginHarness {
	t.Helper()
	configPath, binary, archive := os.Getenv("FIREACTIONS_CONFIG"), os.Getenv("FIREACTIONS_BIN"), os.Getenv("FIREACTIONS_GUEST_ARCHIVE")
	if configPath == "" || binary == "" || archive == "" {
		t.Skip("requires FIREACTIONS_CONFIG, FIREACTIONS_BIN and FIREACTIONS_GUEST_ARCHIVE on a real Firecracker host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("integration harness requires host root privileges")
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(archive) {
		t.Fatal("binary and guest archive must be absolute paths")
	}
	config, err := server.NewConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "fa-it-")
	if err != nil {
		t.Fatal(err)
	}
	h := &pluginHarness{t: t, root: root, config: config, binary: binary, exited: make(chan error, 1)}
	t.Cleanup(h.close)
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	config.Containerd.Namespace = "fireactions-it-" + hex.EncodeToString(random[:])
	config.StateDir = filepath.Join(root, "s")
	config.SocketPath = filepath.Join(root, "run", "plugin.sock")
	config.Metrics.Enabled = false
	for _, pool := range config.Pools {
		pool.Replicas = 0
		pool.ImagePullPolicy = "Never"
	}
	if len(configure) > 1 {
		t.Fatal("newPluginHarness accepts at most one config callback")
	}
	if len(configure) == 1 && configure[0] != nil {
		configure[0](config)
	}
	h.ctx = namespaces.WithNamespace(context.Background(), config.Containerd.Namespace)
	h.containerd, err = containerd.New(config.Containerd.Address, containerd.WithDefaultNamespace(config.Containerd.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.containerd.NamespaceService().Create(h.ctx, config.Containerd.Namespace, map[string]string{"fireactions.integration": "true"}); err != nil {
		t.Fatal(err)
	}
	h.namespaceOwned = true
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	importCtx, cancel := context.WithTimeout(h.ctx, 5*time.Minute)
	imported, err := h.containerd.Import(importCtx, file)
	_ = file.Close()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if len(imported) == 0 {
		cancel()
		t.Fatal("archive contained no image")
	}
	imageName := imported[0].Name
	for _, image := range imported {
		if image.Name == config.Pools[0].Image {
			imageName = image.Name
			break
		}
	}
	image, err := h.containerd.GetImage(importCtx, imageName)
	if err == nil {
		err = image.Unpack(importCtx, "devmapper")
	}
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range config.Pools {
		pool.Image = imageName
	}
	h.baseSnapshots = h.snapshots()
	h.baseLeases = h.leases()
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	h.configPath = filepath.Join(root, "config.yaml")
	if err = os.WriteFile(h.configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	h.launch()
	return h
}

func (h *pluginHarness) launch(beforeServing ...func()) {
	h.t.Helper()
	log, err := os.OpenFile(filepath.Join(h.root, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		h.t.Fatal(err)
	}
	h.process = exec.Command(h.binary, "server", "--config", h.configPath)
	h.process.Stdout = log
	h.process.Stderr = log
	if err = h.process.Start(); err != nil {
		_ = log.Close()
		h.t.Fatal(err)
	}
	process, exited := h.process, make(chan error, 1)
	h.exited = exited
	go func() { err := process.Wait(); _ = log.Close(); exited <- err; close(exited) }()
	h.conn, err = grpc.NewClient("unix://"+h.config.SocketPath, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: false}))
	if err != nil {
		h.t.Fatal(err)
	}
	h.plugin = pluginv1alpha.NewBackendPluginClient(h.conn)
	h.admin = serverv1.NewServerServiceClient(h.conn)
	health := healthv1.NewHealthClient(h.conn)
	deadline := time.Now().Add(h.config.Guest.StartupTimeout + time.Minute)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		response, callErr := health.Check(ctx, &healthv1.HealthCheckRequest{Service: "plugin.v1alpha.BackendPlugin"})
		cancel()
		if callErr == nil && response.Status == healthv1.HealthCheckResponse_SERVING {
			for _, check := range beforeServing {
				check()
			}
			break
		}
		select {
		case err := <-h.exited:
			h.t.Fatalf("daemon exited before serving: %v; %s", err, h.log())
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("plugin did not become SERVING: %v; %s", callErr, h.log())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *pluginHarness) log() string {
	data, _ := os.ReadFile(filepath.Join(h.root, "daemon.log"))
	return string(data)
}
func (h *pluginHarness) close() {
	h.stopOnce.Do(func() {
		if h.conn != nil {
			_ = h.conn.Close()
		}
		if h.process != nil && h.process.Process != nil {
			_ = h.process.Process.Signal(syscall.SIGTERM)
			select {
			case <-h.exited:
			case <-time.After(35 * time.Second):
				_ = h.process.Process.Kill()
				<-h.exited
				h.t.Errorf("daemon needed forced shutdown; %s", h.log())
			}
		}
		// Crash/restart cases must use the same independent cleanup path as the
		// deployed timer before namespace teardown can remove image/base layers.
		if h.configPath != "" {
			if err := h.reap(); err != nil {
				h.t.Errorf("harness recovery failed; retaining namespace and journal at %s: %v", h.root, err)
				if h.containerd != nil {
					_ = h.containerd.Close()
				}
				return
			}
			if entries, err := os.ReadDir(filepath.Join(h.config.StateDir, "journal")); err == nil {
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".json") {
						h.t.Errorf("retaining unrecovered journal and namespace: %s", h.root)
						if h.containerd != nil {
							_ = h.containerd.Close()
						}
						return
					}
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				h.t.Errorf("cannot inspect recovery journal; retaining namespace at %s: %v", h.root, err)
				if h.containerd != nil {
					_ = h.containerd.Close()
				}
				return
			}
		}
		if alive := ownedVMMs(h.config.StateDir); len(alive) != 0 {
			h.t.Errorf("retaining live harness VMM resources rather than removing mounted snapshots: %v; diagnostics %s", alive, h.root)
			if h.containerd != nil {
				_ = h.containerd.Close()
			}
			return
		}
		if h.containerd != nil && h.namespaceOwned {
			ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
			defer cancel()
			list, err := h.containerd.LeasesService().List(ctx)
			if err == nil {
				for _, lease := range list {
					_ = h.containerd.LeasesService().Delete(ctx, lease, leases.SynchronousDelete)
				}
			}
			images, err := h.containerd.ListImages(ctx)
			if err == nil {
				for _, image := range images {
					_ = h.containerd.ImageService().Delete(ctx, image.Name())
				}
			}
			// This namespace was created exclusively by this harness. Remove its
			// imported base layers child-first, never another namespace's images.
			for range 100 {
				var names []string
				_ = h.containerd.SnapshotService("devmapper").Walk(ctx, func(_ context.Context, info snapshots.Info) error { names = append(names, info.Name); return nil })
				if len(names) == 0 {
					break
				}
				removed := false
				for _, name := range names {
					if h.containerd.SnapshotService("devmapper").Remove(ctx, name) == nil {
						removed = true
					}
				}
				if !removed {
					h.t.Errorf("harness-owned snapshots remained: %v", names)
					break
				}
			}
			store := h.containerd.ContentStore()
			var blobs []content.Info
			if err := store.Walk(ctx, func(info content.Info) error {
				blobs = append(blobs, info)
				return nil
			}); err != nil {
				h.t.Errorf("list harness namespace content: %v", err)
			}
			for _, blob := range blobs {
				if err := store.Delete(ctx, blob.Digest); err != nil && !errdefs.IsNotFound(err) {
					h.t.Errorf("remove harness namespace content %s: %v", blob.Digest, err)
				}
			}
			if err = h.containerd.NamespaceService().Delete(ctx, h.config.Containerd.Namespace); err != nil {
				h.t.Errorf("remove harness namespace: %v", err)
			}
		}
		if h.containerd != nil {
			_ = h.containerd.Close()
		}
		if !h.t.Failed() {
			_ = os.RemoveAll(h.root)
		} else {
			h.t.Logf("retained diagnostics: %s", h.root)
		}
	})
}

func (h *pluginHarness) snapshots() map[string]bool {
	h.t.Helper()
	result := make(map[string]bool)
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if err := h.containerd.SnapshotService("devmapper").Walk(ctx, func(_ context.Context, info snapshots.Info) error { result[info.Name] = true; return nil }); err != nil {
		h.t.Fatal(err)
	}
	return result
}
func (h *pluginHarness) leases() map[string]bool {
	h.t.Helper()
	result := make(map[string]bool)
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	list, err := h.containerd.LeasesService().List(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, lease := range list {
		result[lease.ID] = true
	}
	return result
}
func (h *pluginHarness) machines() []*serverv1.Machine {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := h.admin.ListMachines(ctx, &serverv1.ListMachinesRequest{})
	if err != nil {
		h.t.Fatal(err)
	}
	return response.Machines
}
func (h *pluginHarness) waitForMachines(description string, timeout time.Duration, ready func([]*serverv1.Machine) bool) []*serverv1.Machine {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var machines []*serverv1.Machine
	for {
		machines = h.machines()
		if ready(machines) {
			return machines
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s: machines=%+v; %s", description, machines, h.log())
		}
		time.Sleep(100 * time.Millisecond)
	}
}
func (h *pluginHarness) create(profile string) *pluginv1alpha.CreateResponse {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), h.config.Guest.StartupTimeout+20*time.Second)
	defer cancel()
	response, err := h.plugin.Create(ctx, &pluginv1alpha.CreateRequest{Image: profile, Name: "../../runner-name-is-not-an-owned-path"})
	if err != nil {
		h.t.Fatalf("Create: %v; %s", err, h.log())
	}
	if len(response.EnvironmentId) != 43 {
		h.t.Fatalf("non-opaque environment ID: %q", response.EnvironmentId)
	}
	return response
}
func (h *pluginHarness) start(id string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), h.config.Guest.StartupTimeout+10*time.Second)
	defer cancel()
	stream, err := h.plugin.Start(ctx, &pluginv1alpha.StartRequest{EnvironmentId: id})
	if err != nil {
		h.t.Fatal(err)
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			h.t.Fatalf("Start before completion: %v; %s", err, h.log())
		}
		if complete := message.GetStartComplete(); complete != nil {
			if complete.ImageEnv["PATH"] == "" {
				h.t.Fatal("Start omitted usable image PATH")
			}
			cancel()
			return
		}
	}
}
func (h *pluginHarness) exec(ctx context.Context, id string, command []string) (string, string, int32, error) {
	stream, err := h.plugin.Exec(ctx, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: command})
	if err != nil {
		return "", "", 0, err
	}
	var stdout, stderr strings.Builder
	for {
		message, err := stream.Recv()
		if err != nil {
			return stdout.String(), stderr.String(), 0, err
		}
		switch output := message.Output.(type) {
		case *pluginv1alpha.ExecOutput_Data:
			if output.Data.Stream == pluginv1alpha.DataChunk_STDOUT {
				stdout.Write(output.Data.Data)
			} else {
				stderr.Write(output.Data.Data)
			}
		case *pluginv1alpha.ExecOutput_ExecComplete:
			return stdout.String(), stderr.String(), output.ExecComplete.ExitCode, nil
		case *pluginv1alpha.ExecOutput_ExecFailed:
			return stdout.String(), stderr.String(), 0, fmt.Errorf("launch failed: %s", output.ExecFailed.ErrorMessage)
		}
	}
}
func (h *pluginHarness) copyIn(ctx context.Context, id, destination string, source io.Reader) error {
	stream, err := h.plugin.CopyIn(ctx)
	if err != nil {
		return err
	}
	if err = stream.Send(&pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &destination}); err != nil {
		return err
	}
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if err = stream.Send(&pluginv1alpha.CopyInChunk{Data: buffer[:n]}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

type pluginArchiveReader struct {
	stream  pluginv1alpha.BackendPlugin_CopyOutClient
	pending []byte
}

func (r *pluginArchiveReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		message, err := r.stream.Recv()
		if err != nil {
			return 0, err
		}
		r.pending = message.Data
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
func (h *pluginHarness) copyOut(ctx context.Context, id, path string) (*tar.Reader, error) {
	stream, err := h.plugin.CopyOut(ctx, &pluginv1alpha.CopyOutRequest{EnvironmentId: id, SrcPath: path})
	if err != nil {
		return nil, err
	}
	return tar.NewReader(&pluginArchiveReader{stream: stream}), nil
}
func (h *pluginHarness) removeError(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	_, err := h.plugin.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: id})
	return err
}
func (h *pluginHarness) remove(id string) {
	h.t.Helper()
	if err := h.removeError(id); err != nil {
		h.t.Fatalf("Remove: %v; %s", err, h.log())
	}
}

func ownedVMMs(root string) map[int]string {
	result := make(map[int]string)
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(data), "\x00")
		for i, arg := range args {
			if arg == "--api-sock" && i+1 < len(args) && strings.HasPrefix(args[i+1], root+string(filepath.Separator)) {
				result[pid] = args[i+1]
			}
		}
	}
	return result
}
func (h *pluginHarness) assertClean(id string, owned map[int]string, vmIDs []string) {
	h.t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for {
		clean := len(ownedVMMs(h.config.StateDir)) == 0
		for _, socket := range owned {
			if _, err := os.Stat(filepath.Dir(socket)); !errors.Is(err, os.ErrNotExist) {
				clean = false
			}
		}
		for _, vmID := range vmIDs {
			if _, err := os.Stat(filepath.Join("/var/run/netns", vmID)); !errors.Is(err, os.ErrNotExist) {
				clean = false
			}
		}
		for name := range h.snapshots() {
			if !h.baseSnapshots[name] {
				clean = false
			}
		}
		for name := range h.leases() {
			if !h.baseLeases[name] {
				clean = false
			}
		}
		if len(h.machines()) != 0 {
			clean = false
		}
		if clean {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("owned VM/network/snapshot/lease cleanup incomplete; %s", h.log())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for pid, socket := range owned {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err == nil && bytes.Contains(data, []byte(socket)) {
			h.t.Fatalf("original VMM PID %d remains alive", pid)
		}
		if _, err = os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
			h.t.Fatalf("owned API socket remains: %s (%v)", socket, err)
		}
	}
	for _, profile := range h.config.Pools {
		entries, err := os.ReadDir(filepath.Join(h.config.StateDir, "pools", profile.Name))
		if err == nil && len(entries) != 0 {
			h.t.Fatalf("owned VM socket/log/CNI-cache paths remain: %v", entries)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			h.t.Fatal(err)
		}
	}
	// host-local CNI allocations contain ContainerID on their first line.
	for _, root := range []string{"/var/run/cni/fireactions", "/var/run/cni/networks/fireactions", "/var/lib/cni/networks/fireactions"} {
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, _ := os.ReadFile(filepath.Join(root, entry.Name()))
			for _, vmID := range vmIDs {
				if strings.Split(strings.TrimSpace(string(data)), "\n")[0] == vmID {
					h.t.Fatalf("CNI address allocation remains for %s", vmID)
				}
			}
		}
	}
	h.assertUnknownEnvironment(id)
}

func vmIDs(machines []*serverv1.Machine) []string {
	result := make([]string, 0, len(machines))
	for _, machine := range machines {
		result = append(result, machine.ID)
	}
	return result
}

func TestRealPluginColdLifecycle(t *testing.T) {
	h := newPluginHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	capabilities, err := h.plugin.Capabilities(ctx, &pluginv1alpha.CapabilitiesRequest{})
	if err != nil || capabilities.Name != "firecracker" {
		t.Fatalf("Capabilities: %v %v", capabilities, err)
	}
	health, err := healthv1.NewHealthClient(h.conn).Check(ctx, &healthv1.HealthCheckRequest{})
	if err != nil || health.Status != healthv1.HealthCheckResponse_SERVING {
		t.Fatalf("empty-service health: %v %v", health, err)
	}
	for _, request := range []*pluginv1alpha.CreateRequest{
		{Image: "missing-profile"}, {Image: h.config.Pools[0].Name, BackendOptions: map[string]string{"unsupported": "x"}},
		{Image: h.config.Pools[0].Name, Services: []*pluginv1alpha.ServiceContainer{{Name: "db", Image: "postgres"}}},
	} {
		_, err = h.plugin.Create(ctx, request)
		want := codes.InvalidArgument
		if len(request.Services) > 0 {
			want = codes.Unimplemented
		}
		if status.Code(err) != want {
			t.Fatalf("unsupported Create: %v", err)
		}
		if len(h.machines()) != 0 {
			t.Fatal("unsupported request allocated a VM")
		}
	}
	environment := h.create(h.config.Pools[0].Name)
	h.start(environment.EnvironmentId)
	owned := ownedVMMs(h.config.StateDir)
	machines := h.machines()
	if len(owned) != 1 || len(machines) != 1 {
		t.Fatalf("cold environment did not own exactly one VMM: %v %#v", owned, machines)
	}
	input, writer := io.Pipe()
	defer input.Close()
	script := []byte("#!/bin/sh\nprintf 'vm-stdout\\n'\nprintf 'vm-stderr\\n' >&2\nprintf '\\000\\377\\200\\n' > generated.bin\nchmod 0751 generated.bin\nid -u > uid\ntouch job-only-marker\nexit 42\n")
	go func() {
		archive := tar.NewWriter(writer)
		err := archive.WriteHeader(&tar.Header{Name: "tool", Mode: 0751, Size: int64(len(script))})
		if err == nil {
			_, err = archive.Write(script)
		}
		if err == nil {
			err = archive.Close()
		}
		_ = writer.CloseWithError(err)
	}()
	if err = h.copyIn(ctx, environment.EnvironmentId, "/workspace", input); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exit, err := h.exec(ctx, environment.EnvironmentId, []string{"/workspace/tool"})
	if err != nil || exit != 42 || stdout != "vm-stdout\n" || stderr != "vm-stderr\n" {
		t.Fatalf("actual VM exec: exit=%d stdout=%q stderr=%q err=%v", exit, stdout, stderr, err)
	}
	archive, err := h.copyOut(ctx, environment.EnvironmentId, "/workspace/generated.bin")
	if err != nil {
		t.Fatal(err)
	}
	header, err := archive.Next()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4)
	if _, err = io.ReadFull(archive, data); err != nil {
		t.Fatal(err)
	}
	if header.Name != "generated.bin" || header.Mode != 0751 || !bytes.Equal(data, []byte{0, 255, 128, 10}) {
		t.Fatalf("VM file roundtrip: header=%#v data=%v", header, data)
	}
	if _, err = archive.Next(); err != io.EOF {
		t.Fatalf("archive end: %v", err)
	}
	stdout, _, exit, err = h.exec(ctx, environment.EnvironmentId, []string{"/bin/cat", "/workspace/uid"})
	if err != nil || exit != 0 || stdout != "1000\n" {
		t.Fatalf("guest default user was not ci1000: %q %d %v", stdout, exit, err)
	}
	archive, err = h.copyOut(ctx, environment.EnvironmentId, "/workspace/missing")
	if err == nil {
		_, err = archive.Next()
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing file: %v", err)
	}
	archive, err = h.copyOut(ctx, environment.EnvironmentId, "/workspace/../etc/passwd")
	if err == nil {
		_, err = archive.Next()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unsafe workspace path: %v", err)
	}
	_, _, _, err = h.exec(ctx, environment.EnvironmentId, []string{"missing-executable"})
	if err == nil || !strings.HasPrefix(err.Error(), "launch failed:") {
		t.Fatalf("missing executable fabricated an exit: %v", err)
	}
	h.remove(environment.EnvironmentId)
	h.remove(environment.EnvironmentId)
	h.assertClean(environment.EnvironmentId, owned, vmIDs(machines))
	next := h.create(h.config.Pools[0].Name)
	if next.EnvironmentId == environment.EnvironmentId {
		t.Fatal("environment ID reused")
	}
	h.start(next.EnvironmentId)
	stdout, stderr, exit, err = h.exec(ctx, next.EnvironmentId, []string{"/bin/sh", "-c", "test ! -e /workspace/job-only-marker"})
	if err != nil || exit != 0 {
		t.Fatalf("dirty filesystem reused: %q %q %d %v", stdout, stderr, exit, err)
	}
	owned = ownedVMMs(h.config.StateDir)
	machines = h.machines()
	h.remove(next.EnvironmentId)
	h.assertClean(next.EnvironmentId, owned, vmIDs(machines))
	t.Log("all seven plugin RPCs exercised against real Firecracker; ci1000, exact stdout/stderr42, binary/mode, fresh filesystem and full owned-resource teardown verified")
}

func TestRealPluginWarmPoolLifecycle(t *testing.T) {
	h := newPluginHarness(t, func(config *server.Config) {
		if len(config.Pools) == 0 {
			t.Fatal("warm-pool integration config has no profiles")
		}
		for _, pool := range config.Pools {
			pool.Replicas = 0
		}
		config.Pools[0].Replicas = 1
	})
	profile := h.config.Pools[0].Name
	timeout := h.config.Guest.StartupTimeout + time.Minute

	owned := make(map[int]string)
	ownedVMIDs := make(map[string]struct{})
	remember := func(machines []*serverv1.Machine) {
		for pid, socket := range ownedVMMs(h.config.StateDir) {
			owned[pid] = socket
		}
		for _, machine := range machines {
			ownedVMIDs[machine.ID] = struct{}{}
		}
	}
	poolInfo := func() *serverv1.Pool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		response, err := h.admin.GetPool(ctx, &serverv1.GetPoolRequest{Name: profile})
		if err != nil {
			t.Fatalf("GetPool: %v", err)
		}
		if response.Pool == nil {
			t.Fatal("GetPool returned no pool")
		}
		return response.Pool
	}
	poolSettled := func(machines []*serverv1.Machine, environments map[string]bool, idleCount int) bool {
		if len(machines) != len(environments)+idleCount {
			return false
		}
		foundEnvironments := make(map[string]bool, len(environments))
		foundIdle := 0
		foundVMIDs := make(map[string]bool, len(machines))
		for _, machine := range machines {
			if machine.ID == "" || foundVMIDs[machine.ID] {
				return false
			}
			foundVMIDs[machine.ID] = true
			switch machine.State {
			case "claimed":
				if machine.EnvironmentId == "" || !environments[machine.EnvironmentId] || foundEnvironments[machine.EnvironmentId] {
					return false
				}
				foundEnvironments[machine.EnvironmentId] = true
			case "idle":
				if machine.AgentVersion == "" {
					return false
				}
				foundIdle++
			default:
				return false
			}
		}
		return len(foundEnvironments) == len(environments) && foundIdle == idleCount
	}
	claimedVMs := func(machines []*serverv1.Machine, environments map[string]bool) map[string]string {
		result := make(map[string]string, len(environments))
		for _, machine := range machines {
			if machine.State != "claimed" || machine.EnvironmentId == "" || !environments[machine.EnvironmentId] {
				continue
			}
			if _, exists := result[machine.EnvironmentId]; exists {
				return nil
			}
			result[machine.EnvironmentId] = machine.ID
		}
		if len(result) != len(environments) {
			return nil
		}
		return result
	}
	sameMachineMap := func(left, right map[string]string) bool {
		if len(left) != len(right) {
			return false
		}
		for environment, machine := range left {
			if right[environment] != machine {
				return false
			}
		}
		return true
	}
	run := func(environmentID, description, command string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stdout, stderr, exit, err := h.exec(ctx, environmentID, []string{"/bin/sh", "-c", command})
		if err != nil || exit != 0 {
			t.Fatalf("%s: stdout=%q stderr=%q exit=%d err=%v", description, stdout, stderr, exit, err)
		}
	}

	initial := h.waitForMachines("the published ready warm VM", timeout, func(machines []*serverv1.Machine) bool {
		return len(machines) == 1 && machines[0].Pool == profile &&
			machines[0].State == "idle" && machines[0].AgentVersion != ""
	})
	warmVMID := initial[0].ID
	remember(initial)
	for _, pool := range h.config.Pools {
		if strings.Contains(strings.ToLower(pool.Name), "large") && pool.Replicas != 0 {
			t.Fatalf("large profile %q unexpectedly has warm replicas: %d", pool.Name, pool.Replicas)
		}
	}

	type createResult struct {
		response *pluginv1alpha.CreateResponse
		err      error
	}
	results := make(chan createResult, 2)
	for index := range 2 {
		go func(index int) {
			ctx, cancel := context.WithTimeout(context.Background(), h.config.Guest.StartupTimeout+20*time.Second)
			defer cancel()
			response, err := h.plugin.Create(ctx, &pluginv1alpha.CreateRequest{
				Image: profile,
				Name:  fmt.Sprintf("warm-concurrent-%d", index),
			})
			results <- createResult{response: response, err: err}
		}(index)
	}
	environmentIDs := make([]string, 0, 2)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent warm Create: %v; %s", result.err, h.log())
		}
		if result.response == nil || len(result.response.EnvironmentId) != 43 {
			t.Fatalf("concurrent Create returned a non-opaque environment ID: %#v", result.response)
		}
		environmentIDs = append(environmentIDs, result.response.EnvironmentId)
	}
	if environmentIDs[0] == environmentIDs[1] {
		t.Fatalf("concurrent Create reused environment ID %q", environmentIDs[0])
	}
	expectedTwo := map[string]bool{environmentIDs[0]: true, environmentIDs[1]: true}
	originalEnvironmentID := ""
	claimed := h.waitForMachines("both concurrent acquisitions to claim distinct VMs", timeout, func(machines []*serverv1.Machine) bool {
		found := make(map[string]string, 2)
		originalClaims := 0
		for _, machine := range machines {
			if machine.ID == warmVMID {
				originalClaims++
				if machine.State != "claimed" || !expectedTwo[machine.EnvironmentId] {
					return false
				}
				originalEnvironmentID = machine.EnvironmentId
			}
			if machine.State == "claimed" && expectedTwo[machine.EnvironmentId] {
				if _, duplicate := found[machine.EnvironmentId]; duplicate {
					return false
				}
				found[machine.EnvironmentId] = machine.ID
			}
		}
		return len(found) == 2 && originalClaims == 1
	})
	remember(claimed)
	stableTwo := h.waitForMachines("clean replacement after concurrent claims", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, expectedTwo, 1)
	})
	remember(stableTwo)
	initialClaims := claimedVMs(stableTwo, expectedTwo)
	if initialClaims == nil || initialClaims[originalEnvironmentID] != warmVMID {
		t.Fatalf("original idle VM was not claimed exactly once: warm=%q environment=%q claims=%v", warmVMID, originalEnvironmentID, initialClaims)
	}
	firstEnvironmentID := originalEnvironmentID
	secondEnvironmentID := environmentIDs[0]
	if secondEnvironmentID == firstEnvironmentID {
		secondEnvironmentID = environmentIDs[1]
	}
	if initialClaims[firstEnvironmentID] == initialClaims[secondEnvironmentID] {
		t.Fatalf("concurrent environments share VM %q", initialClaims[firstEnvironmentID])
	}

	h.start(firstEnvironmentID)
	h.start(secondEnvironmentID)
	run(firstEnvironmentID, "write first-job marker", "touch /workspace/warm-job-marker")
	run(secondEnvironmentID, "second job observed first-job marker", "test ! -e /workspace/warm-job-marker")

	var pausedIdleVMID string
	for _, machine := range stableTwo {
		if machine.State == "idle" {
			pausedIdleVMID = machine.ID
		}
	}
	if pausedIdleVMID == "" || pausedIdleVMID == warmVMID {
		t.Fatalf("no fresh idle replacement was published: %q", pausedIdleVMID)
	}
	pauseCtx, cancelPause := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := h.admin.PausePool(pauseCtx, &serverv1.PausePoolRequest{Name: profile})
	cancelPause()
	if err != nil {
		t.Fatalf("PausePool: %v", err)
	}
	if state := poolInfo().State; state != serverv1.PoolState_POOL_STATE_PAUSED {
		t.Fatalf("PausePool did not publish paused state: %v", state)
	}

	third := h.create(profile)
	if expectedTwo[third.EnvironmentId] {
		t.Fatalf("paused-pool Create reused environment ID %q", third.EnvironmentId)
	}
	expectedThree := map[string]bool{
		firstEnvironmentID:  true,
		secondEnvironmentID: true,
		third.EnvironmentId: true,
	}
	pausedClaim := h.waitForMachines("paused pool to claim its existing idle VM", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, expectedThree, 0)
	})
	remember(pausedClaim)
	pausedClaims := claimedVMs(pausedClaim, expectedThree)
	if pausedClaims == nil || pausedClaims[third.EnvironmentId] != pausedIdleVMID {
		t.Fatalf("paused acquisition did not claim the existing idle VM %q: %v", pausedIdleVMID, pausedClaims)
	}
	h.start(third.EnvironmentId)
	run(third.EnvironmentId, "replacement VM carried dirty workspace", "test ! -e /workspace/warm-job-marker")

	emptyCtx, cancelEmpty := context.WithTimeout(context.Background(), 10*time.Second)
	emptyResponse, emptyErr := h.plugin.Create(emptyCtx, &pluginv1alpha.CreateRequest{Image: profile, Name: "warm-paused-empty"})
	cancelEmpty()
	if status.Code(emptyErr) != codes.Unavailable {
		if emptyResponse != nil {
			h.remove(emptyResponse.EnvironmentId)
		}
		t.Fatalf("empty paused pool Create: want Unavailable, got response=%#v err=%v", emptyResponse, emptyErr)
	}
	stillPaused := h.waitForMachines("paused pool to remain empty after rejected Create", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, expectedThree, 0)
	})
	remember(stillPaused)
	if !sameMachineMap(pausedClaims, claimedVMs(stillPaused, expectedThree)) {
		t.Fatal("rejected paused Create changed claimed VM ownership")
	}

	resumeCtx, cancelResume := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = h.admin.ResumePool(resumeCtx, &serverv1.ResumePoolRequest{Name: profile})
	cancelResume()
	if err != nil {
		t.Fatalf("ResumePool: %v", err)
	}
	if state := poolInfo().State; state != serverv1.PoolState_POOL_STATE_ACTIVE {
		t.Fatalf("ResumePool did not publish active state: %v", state)
	}
	beforeScale := h.waitForMachines("resumed pool to publish one clean idle VM", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, expectedThree, 1)
	})
	remember(beforeScale)
	claimedBeforeScale := claimedVMs(beforeScale, expectedThree)
	if claimedBeforeScale == nil {
		t.Fatal("resumed pool changed claimed environment ownership")
	}

	scaleCtx, cancelScale := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = h.admin.ScalePool(scaleCtx, &serverv1.ScalePoolRequest{Name: profile, Replicas: 0})
	cancelScale()
	if err != nil {
		t.Fatalf("ScalePool(0): %v", err)
	}
	if pool := poolInfo(); pool.DesiredReplicas != 0 || pool.State != serverv1.PoolState_POOL_STATE_ACTIVE {
		t.Fatalf("scale-to-zero changed wrong pool state: %+v", pool)
	}
	scaledDown := h.waitForMachines("scale-down to remove only the idle VM", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, expectedThree, 0)
	})
	remember(scaledDown)
	if !sameMachineMap(claimedBeforeScale, claimedVMs(scaledDown, expectedThree)) {
		t.Fatalf("scale-down changed claimed VM ownership: before=%v after=%v", claimedBeforeScale, claimedVMs(scaledDown, expectedThree))
	}
	run(firstEnvironmentID, "claimed warm VM stopped after scale-down", "test -e /workspace/warm-job-marker")
	run(secondEnvironmentID, "second claimed VM stopped after scale-down", "test ! -e /workspace/warm-job-marker")
	run(third.EnvironmentId, "replacement claimed VM stopped after scale-down", "test ! -e /workspace/warm-job-marker")

	h.remove(firstEnvironmentID)
	remaining := map[string]bool{secondEnvironmentID: true, third.EnvironmentId: true}
	afterFirstRemove := h.waitForMachines("first environment cleanup at zero replicas", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, remaining, 0)
	})
	remember(afterFirstRemove)
	for _, machine := range afterFirstRemove {
		if machine.ID == warmVMID {
			t.Fatalf("removed warm VM %q remained registered", warmVMID)
		}
	}
	previousVMIDs := make(map[string]struct{}, len(ownedVMIDs))
	for vmID := range ownedVMIDs {
		previousVMIDs[vmID] = struct{}{}
	}
	if pool := poolInfo(); pool.DesiredReplicas != 0 || pool.State != serverv1.PoolState_POOL_STATE_ACTIVE {
		t.Fatalf("cold acquisition precondition was not active at zero replicas: %+v", pool)
	}

	fresh := h.create(profile)
	expectedFresh := map[string]bool{
		secondEnvironmentID: true,
		third.EnvironmentId: true,
		fresh.EnvironmentId: true,
	}
	freshMachines := h.waitForMachines("zero-replica active Create to cold-provision a VM", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, expectedFresh, 0)
	})
	freshClaims := claimedVMs(freshMachines, expectedFresh)
	if freshClaims == nil {
		t.Fatal("fresh cold environment was not claimed")
	}
	if _, existed := previousVMIDs[freshClaims[fresh.EnvironmentId]]; existed {
		t.Fatalf("zero-replica Create reused an existing VM %q", freshClaims[fresh.EnvironmentId])
	}
	remember(freshMachines)
	h.start(fresh.EnvironmentId)
	run(fresh.EnvironmentId, "zero-replica cold VM carried dirty workspace", "test ! -e /workspace/warm-job-marker")

	finalMachines := h.waitForMachines("all active environments before teardown", timeout, func(machines []*serverv1.Machine) bool {
		return poolSettled(machines, expectedFresh, 0)
	})
	remember(finalMachines)
	h.remove(secondEnvironmentID)
	h.remove(third.EnvironmentId)
	h.remove(fresh.EnvironmentId)

	allVMIDList := make([]string, 0, len(ownedVMIDs))
	for vmID := range ownedVMIDs {
		allVMIDList = append(allVMIDList, vmID)
	}
	for _, environmentID := range []string{firstEnvironmentID, secondEnvironmentID, third.EnvironmentId, fresh.EnvironmentId} {
		h.assertClean(environmentID, owned, allVMIDList)
	}
	t.Log("warm publication, concurrent unique claims, clean replacement, paused idle-only claim, claimed-VM scale-down protection, zero-replica cold filesystem and complete resource cleanup verified")
}

func TestRealPluginConfiguredLargeProfileSMP(t *testing.T) {
	h := newPluginHarness(t)
	if len(h.config.Pools) < 2 {
		t.Skip("requires a second configured Firecracker profile")
	}
	profile := h.config.Pools[1]
	expectedVCPUs := profile.Firecracker.MachineConfig.VcpuCount
	environment := h.create(profile.Name)
	h.start(environment.EnvironmentId)

	machines := h.machines()
	owned := ownedVMMs(h.config.StateDir)
	if len(machines) != 1 || machines[0].State != "claimed" || machines[0].EnvironmentId != environment.EnvironmentId || machines[0].Pool != profile.Name {
		t.Fatalf("large-profile environment did not own one claimed VM: %#v", machines)
	}
	if len(owned) != 1 {
		t.Fatalf("large-profile environment did not own one VMM: %v", owned)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	stdout, stderr, exit, err := h.exec(ctx, environment.EnvironmentId, []string{"nproc"})
	cancel()
	if err != nil || exit != 0 {
		t.Fatalf("large-profile nproc: stdout=%q stderr=%q exit=%d err=%v", stdout, stderr, exit, err)
	}
	actualVCPUs, parseErr := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if parseErr != nil || actualVCPUs != expectedVCPUs {
		t.Fatalf("guest reported %q CPUs, configured Firecracker profile %q requires %d: parse error=%v", strings.TrimSpace(stdout), profile.Name, expectedVCPUs, parseErr)
	}

	h.remove(environment.EnvironmentId)
	h.assertClean(environment.EnvironmentId, owned, vmIDs(machines))
	t.Logf("configured profile %s exposed all %d vCPUs to the guest", profile.Name, expectedVCPUs)
}

func TestRealPluginSilentCancellationAndRemove(t *testing.T) {
	h := newPluginHarness(t)
	environment := h.create(h.config.Pools[0].Name)
	h.start(environment.EnvironmentId)
	owned, machines := ownedVMMs(h.config.StateDir), h.machines()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := h.plugin.Exec(ctx, &pluginv1alpha.ExecRequest{EnvironmentId: environment.EnvironmentId, Command: []string{"/bin/sh", "-c", "setsid sh -c 'echo $$ > child.pid; exec sleep 300' & while [ ! -s child.pid ]; do sleep 0.01; done; echo scope-ready; sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for !seen {
		message, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if data := message.GetData(); data != nil && bytes.Contains(data.Data, []byte("scope-ready")) {
			seen = true
		}
	}
	time.Sleep(200 * time.Millisecond) // No further output: cancellation must not rely on Send failure.
	cancel()
	removed := make(chan error, 1)
	go func() { removed <- h.removeError(environment.EnvironmentId) }()
	_, err = stream.Recv()
	if status.Code(err) != codes.Canceled {
		t.Fatalf("Exec cancellation: %v", err)
	}
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("overlapping Remove: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("Remove deadlocked behind silent Exec")
	}
	h.assertClean(environment.EnvironmentId, owned, vmIDs(machines))
	t.Log("silent-after-readiness shell and setsid descendant cancelled; overlapping Remove destroyed VM and all owned resources")
}

func TestRealPluginLongSilentKeepaliveCancellation(t *testing.T) {
	h := newPluginHarness(t)
	environment := h.create(h.config.Pools[0].Name)
	h.start(environment.EnvironmentId)
	owned, machines := ownedVMMs(h.config.StateDir), h.machines()

	const timeout = 105 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	stdout, stderr, _, err := h.exec(ctx, environment.EnvironmentId, []string{"sleep", "300"})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("silent Exec did not reach the 105s client deadline: stdout=%q stderr=%q error=%v (elapsed %s)", stdout, stderr, err, time.Since(started))
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("sleep unexpectedly produced workflow output: stdout=%q stderr=%q", stdout, stderr)
	}
	t.Logf("silent Exec remained connected until its 105s client deadline and was cancelled (observed elapsed %s)", time.Since(started))

	h.assertClean(environment.EnvironmentId, owned, vmIDs(machines))
}

func processRSS(pid int) int64 {
	data, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				value, _ := strconv.ParseInt(fields[1], 10, 64)
				return value * 1024
			}
		}
	}
	return 0
}

func TestRealPluginLargeStreamingTransfer(t *testing.T) {
	h := newPluginHarness(t)
	environment := h.create(h.config.Pools[0].Name)
	h.start(environment.EnvironmentId)
	owned, machines := ownedVMMs(h.config.StateDir), h.machines()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	const size = 256 * 1024 * 1024
	block := make([]byte, 32*1024)
	for i := range block {
		block[i] = byte(i*17 + 3)
	}
	expected := sha256.New()
	for range size / len(block) {
		_, _ = expected.Write(block)
	}
	baseline := processRSS(h.process.Process.Pid)
	var peak int64 = baseline
	stopSampling := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-ticker.C:
				rss := processRSS(h.process.Process.Pid)
				if rss > peak {
					peak = rss
				}
			}
		}
	}()
	var sampleStop sync.Once
	stopSampler := func() { sampleStop.Do(func() { close(stopSampling) }); <-sampled }
	defer stopSampler()
	input, writer := io.Pipe()
	defer input.Close()
	go func() {
		archive := tar.NewWriter(writer)
		err := archive.WriteHeader(&tar.Header{Name: "large", Mode: 0751, Size: size, ModTime: time.Unix(1700000000, 0)})
		for range size / len(block) {
			if err != nil {
				break
			}
			_, err = archive.Write(block)
		}
		if err == nil {
			err = archive.Close()
		}
		_ = writer.CloseWithError(err)
	}()
	if err := h.copyIn(ctx, environment.EnvironmentId, "/workspace", input); err != nil {
		t.Fatal(err)
	}
	archive, err := h.copyOut(ctx, environment.EnvironmentId, "/workspace/large")
	if err != nil {
		t.Fatal(err)
	}
	header, err := archive.Next()
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.New()
	n, err := io.CopyBuffer(actual, archive, make([]byte, 32*1024))
	if err != nil {
		t.Fatal(err)
	}
	if n != size || !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) || header.Mode != 0751 || !header.ModTime.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("large stream corrupted: n=%d header=%#v digest=%x", n, header, actual.Sum(nil))
	}
	if _, err = archive.Next(); err != io.EOF {
		t.Fatalf("large archive EOF: %v", err)
	}
	stopSampler() // Join the sampler before reading its peak.
	if baseline == 0 || peak-baseline >= 128*1024*1024 {
		t.Fatalf("plugin RSS grew with archive: baseline=%d peak=%d", baseline, peak)
	}
	h.remove(environment.EnvironmentId)
	h.assertClean(environment.EnvironmentId, owned, vmIDs(machines))
	t.Logf("real VM stream bytes=%d sha256=%x plugin_RSS_baseline=%d peak=%d", n, actual.Sum(nil), baseline, peak)
}

func TestRealPluginAcquisitionCancellation(t *testing.T) {
	h := newPluginHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type createResult struct {
		response *pluginv1alpha.CreateResponse
		err      error
	}
	result := make(chan createResult, 1)
	go func() {
		response, err := h.plugin.Create(ctx, &pluginv1alpha.CreateRequest{Image: h.config.Pools[0].Name})
		result <- createResult{response: response, err: err}
	}()
	deadline := time.Now().Add(h.config.Guest.StartupTimeout)
	var owned map[int]string
	for len(owned) == 0 {
		select {
		case done := <-result:
			if done.response != nil {
				h.remove(done.response.EnvironmentId)
			}
			t.Fatalf("acquisition completed before its VMM could be observed: %v", done.err)
		default:
		}
		owned = ownedVMMs(h.config.StateDir)
		if time.Now().After(deadline) {
			t.Fatal("cold acquisition never started a VMM")
		}
		if len(owned) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case done := <-result:
		if status.Code(done.err) != codes.Canceled {
			if done.response != nil {
				h.remove(done.response.EnvironmentId)
			}
			t.Fatalf("unfinished acquisition cancellation: %v", done.err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("cancelled acquisition remained blocked")
	}
	ids := make([]string, 0, len(owned))
	for _, socket := range owned {
		ids = append(ids, filepath.Base(filepath.Dir(socket)))
	}
	h.assertClean(strings.Repeat("A", 43), owned, ids)
	t.Log("cancellation during observed cold VM startup unwound VMM, netns, CNI, snapshot, lease and private sockets")
}

func TestRealPluginStartCancellationWithUnresponsiveGuest(t *testing.T) {
	h := newPluginHarness(t)
	environment := h.create(h.config.Pools[0].Name)
	owned, machines := ownedVMMs(h.config.StateDir), h.machines()
	if len(owned) != 1 {
		t.Fatalf("expected one cold VMM: %v", owned)
	}
	var socket string
	for _, path := range owned {
		socket = filepath.Join(filepath.Dir(path), "vsock")
	}
	conn, err := grpc.NewClient("passthrough:private-guest", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return sdkvsock.DialContext(ctx, socket, 9001) }))
	if err != nil {
		t.Fatal(err)
	}
	client := guest.New(conn)
	defer client.Close()
	probe, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	// Host-root fault injection into its disposable guest, not a host-exec fallback.
	// The private agent must not be trusted to cooperate after guest root stops it.
	stopScript := `for f in /proc/[0-9]*/comm; do read -r name < "$f" || continue; if [ "$name" = fireactions ]; then pid=${f%/comm}; pid=${pid##*/}; (sleep 1; kill -STOP "$pid") </dev/null >/dev/null 2>&1 & echo agent-stop-scheduled; exit 0; fi; done; exit 1`
	var output bytes.Buffer
	result, err := client.Exec(probe, executor.ExecSpec{Command: []string{"/bin/sh", "-c", stopScript}, User: "root", Env: map[string]string{"PATH": "/usr/bin:/bin"}}, &output, io.Discard)
	if err != nil || result.ExitCode != 0 || output.String() != "agent-stop-scheduled\n" {
		t.Fatalf("guest fault injection: %v %#v %q", err, result, output.String())
	}
	time.Sleep(1200 * time.Millisecond)
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err = client.Ready(short, executor.ReadySpec{})
	cancelShort()
	if executor.KindOf(err) != executor.DeadlineExceeded {
		t.Fatalf("guest control did not become unresponsive: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := h.plugin.Start(ctx, &pluginv1alpha.StartRequest{EnvironmentId: environment.EnvironmentId})
	if err != nil {
		t.Fatal(err)
	}
	message, err := stream.Recv()
	if err != nil || message.GetStartComplete() != nil {
		t.Fatalf("Start did not wait for frozen guest readiness: %v %v", message, err)
	}
	cancel()
	_, err = stream.Recv()
	if status.Code(err) != codes.Canceled {
		t.Fatalf("Start cancellation: %v", err)
	}
	h.remove(environment.EnvironmentId)
	h.assertClean(environment.EnvironmentId, owned, vmIDs(machines))
	t.Log("Start cancellation destroyed a VM whose root-owned guest agent could not acknowledge Kill")
}

func TestRealPluginCancelledTransferRemovesPartialFile(t *testing.T) {
	h := newPluginHarness(t)
	environment := h.create(h.config.Pools[0].Name)
	h.start(environment.EnvironmentId)
	owned, machines := ownedVMMs(h.config.StateDir), h.machines()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.plugin.CopyIn(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	destination := "/workspace/cancelled"
	if err = stream.Send(&pluginv1alpha.CopyInChunk{EnvironmentId: &environment.EnvironmentId, DestPath: &destination}); err != nil {
		cancel()
		t.Fatal(err)
	}
	var header bytes.Buffer
	writer := tar.NewWriter(&header)
	if err = writer.WriteHeader(&tar.Header{Name: "partial", Mode: 0755, Size: 256 * 1024 * 1024}); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err = stream.Send(&pluginv1alpha.CopyInChunk{Data: header.Bytes()}); err != nil {
		cancel()
		t.Fatal(err)
	}
	block := make([]byte, 32*1024)
	for range 8 {
		if err = stream.Send(&pluginv1alpha.CopyInChunk{Data: block}); err != nil {
			cancel()
			t.Fatal(err)
		}
	}
	// A second, privileged private channel observes extraction without waiting
	// behind the public environment operation gate or relying on a guessed delay.
	var socket string
	for _, path := range owned {
		socket = filepath.Join(filepath.Dir(path), "vsock")
	}
	conn, err := grpc.NewClient("passthrough:transfer-observer", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return sdkvsock.DialContext(ctx, socket, 9001) }))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	client := guest.New(conn)
	defer client.Close()
	probe, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	result, err := client.Exec(probe, executor.ExecSpec{Command: []string{"/bin/sh", "-c", `while [ -z "$(ls -A /workspace/cancelled 2>/dev/null)" ]; do sleep 0.01; done`}, Env: map[string]string{"PATH": "/usr/bin:/bin"}}, io.Discard, io.Discard)
	cancelProbe()
	if err != nil || result.ExitCode != 0 {
		cancel()
		t.Fatalf("partial extraction never began: %v %#v", err, result)
	}
	cancel()
	_, err = stream.CloseAndRecv()
	if status.Code(err) != codes.Canceled {
		t.Fatalf("transfer cancellation did not unblock: %v", err)
	}
	check, cancelCheck := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCheck()
	_, stderr, exit, err := h.exec(check, environment.EnvironmentId, []string{"/bin/sh", "-c", `while [ -n "$(ls -A /workspace/cancelled)" ]; do sleep 0.01; done; test ! -e /workspace/cancelled/partial`})
	if err != nil || exit != 0 {
		t.Fatalf("cancelled transfer left a partial file/temp: %q %d %v", stderr, exit, err)
	}
	h.remove(environment.EnvironmentId)
	h.assertClean(environment.EnvironmentId, owned, vmIDs(machines))
	t.Log("client cancellation interrupted an observed partial extraction and removed temporary data without dirty VM reuse")
}

// recoveryRecord is read from the real daemon's durable journal, not assembled
// from guessed resource names. Unknown fields remain intact during PID fault injection.
type recoveryRecord struct {
	Version             int             `json:"version"`
	InstanceID          string          `json:"instance_id"`
	ContainerdNamespace string          `json:"containerd_namespace"`
	Snapshotter         string          `json:"snapshotter"`
	VMID                string          `json:"vm_id"`
	State               string          `json:"state"`
	OwnerPID            int             `json:"owner_pid"`
	OwnerStartTime      string          `json:"owner_start_time"`
	VMMPID              int             `json:"vmm_pid"`
	VMMStartTime        string          `json:"vmm_start_time"`
	SnapshotID          string          `json:"snapshot_id"`
	LeaseID             string          `json:"lease_id"`
	APISocketPath       string          `json:"api_socket_path"`
	VsockPath           string          `json:"vsock_path"`
	NetNSPath           string          `json:"netns_path"`
	CNICacheDir         string          `json:"cni_cache_dir"`
	CNIConfig           json.RawMessage `json:"cni_config"`
	ExpiresAt           *time.Time      `json:"expires_at"`
}

func (h *pluginHarness) journal(vmID string) recoveryRecord {
	h.t.Helper()
	path := filepath.Join(h.config.StateDir, "journal", vmID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read published VM journal %s: %v", path, err)
	}
	var record recoveryRecord
	if err = json.Unmarshal(data, &record); err != nil {
		h.t.Fatal(err)
	}
	if record.Version != 1 || record.VMID != vmID || record.InstanceID == "" || record.ContainerdNamespace != h.config.Containerd.Namespace || record.Snapshotter == "" || record.VMMPID <= 0 || record.VMMStartTime == "" || record.SnapshotID == "" || record.LeaseID == "" || record.APISocketPath == "" || record.NetNSPath == "" || record.CNICacheDir == "" {
		h.t.Fatalf("published VM has incomplete durable ownership: %+v", record)
	}
	return record
}

func hostProcessStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func recoveryVMMAlive(record recoveryRecord) bool {
	start, err := hostProcessStartTime(record.VMMPID)
	if err != nil || strconv.FormatUint(start, 10) != record.VMMStartTime {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(record.VMMPID), "cmdline"))
	return err == nil && bytes.Contains(data, []byte(record.APISocketPath))
}

func (h *pluginHarness) crash() {
	h.t.Helper()
	if h.conn != nil {
		_ = h.conn.Close()
		h.conn = nil
	}
	if err := h.process.Process.Kill(); err != nil {
		h.t.Fatal(err)
	}
	select {
	case err := <-h.exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			h.t.Fatalf("expected actual daemon SIGKILL: %v", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Fatal("SIGKILL daemon did not exit")
	}
	h.process = nil
}

func (h *pluginHarness) reap() error {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, h.binary, "reap", "--config", h.configPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("independent reap: %w: %s", err, output)
	}
	return nil
}

func (h *pluginHarness) pauseWarmPool(replicas int) []*serverv1.Machine {
	h.t.Helper()
	profile := h.config.Pools[0].Name
	machines := h.waitForMachines("ready idle pool before lease/recovery", h.config.Guest.StartupTimeout+time.Minute, func(machines []*serverv1.Machine) bool {
		if len(machines) != replicas {
			return false
		}
		for _, machine := range machines {
			if machine.Pool != profile || machine.State != "idle" || machine.AgentVersion == "" {
				return false
			}
		}
		return true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.admin.PausePool(ctx, &serverv1.PausePoolRequest{Name: profile}); err != nil {
		h.t.Fatal(err)
	}
	return machines
}

func (h *pluginHarness) claimedRecord(environmentID string) recoveryRecord {
	h.t.Helper()
	machines := h.waitForMachines("durably published claimed VM", 5*time.Second, func(machines []*serverv1.Machine) bool {
		for _, machine := range machines {
			if machine.State == "claimed" && machine.EnvironmentId == environmentID {
				return true
			}
		}
		return false
	})
	for _, machine := range machines {
		if machine.EnvironmentId == environmentID {
			record := h.journal(machine.ID)
			if record.State != "claimed" || record.ExpiresAt == nil {
				h.t.Fatalf("claim published without hard durable expiry: %+v", record)
			}
			return record
		}
	}
	h.t.Fatal("claimed machine disappeared")
	return recoveryRecord{}
}

func cniAllocationRoots(record recoveryRecord) []string {
	// Journal []byte encoding is base64; accept a raw JSON list as well so the
	// assertions inspect the persisted effective conflist, not host configuration.
	data := []byte(record.CNIConfig)
	var decoded []byte
	if json.Unmarshal(record.CNIConfig, &decoded) == nil {
		data = decoded
	}
	var config struct {
		Name    string `json:"name"`
		Plugins []struct {
			IPAM struct {
				Type    string `json:"type"`
				DataDir string `json:"dataDir"`
			} `json:"ipam"`
		} `json:"plugins"`
	}
	if json.Unmarshal(data, &config) != nil || config.Name == "" {
		return nil
	}
	roots := []string{filepath.Join("/var/run/cni", config.Name), filepath.Join("/var/run/cni/networks", config.Name), filepath.Join("/var/lib/cni/networks", config.Name)}
	for _, plugin := range config.Plugins {
		if plugin.IPAM.Type == "host-local" && plugin.IPAM.DataDir != "" {
			roots = append(roots, filepath.Join(plugin.IPAM.DataDir, config.Name))
		}
	}
	return roots
}

func (h *pluginHarness) recoveryResourcesGone(record recoveryRecord) error {
	if recoveryVMMAlive(record) {
		return fmt.Errorf("original VMM %d remains live", record.VMMPID)
	}
	for _, path := range []string{record.APISocketPath, record.VsockPath, record.NetNSPath, record.NetNSPath + ".owner", record.CNICacheDir, filepath.Dir(record.APISocketPath), filepath.Join(h.config.StateDir, "journal", record.VMID+".json")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("owned path remains %s: %v", path, err)
		}
	}
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	if _, err := h.containerd.SnapshotService(record.Snapshotter).Stat(ctx, record.SnapshotID); !errdefs.IsNotFound(err) {
		return fmt.Errorf("owned snapshot %s remains: %v", record.SnapshotID, err)
	}
	list, err := h.containerd.LeasesService().List(ctx)
	if err != nil {
		return err
	}
	for _, lease := range list {
		if lease.ID == record.LeaseID {
			return fmt.Errorf("owned lease %s remains", record.LeaseID)
		}
	}
	roots := cniAllocationRoots(record)
	if len(roots) == 0 {
		return fmt.Errorf("cannot inspect captured CNI allocation identity for %s", record.VMID)
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, entry.Name()))
			if err != nil {
				return err
			}
			if strings.Split(strings.TrimSpace(string(data)), "\n")[0] == record.VMID {
				return fmt.Errorf("owned CNI allocation remains: %s", filepath.Join(root, entry.Name()))
			}
		}
	}
	return nil
}

func (h *pluginHarness) waitRecoveryClean(records []recoveryRecord) {
	h.t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for _, record := range records {
		for {
			err := h.recoveryResourcesGone(record)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				h.t.Fatalf("recovery cleanup: %v; %s", err, h.log())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func (h *pluginHarness) assertUnknownEnvironment(id string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	for {
		probe, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
		stream, err := h.plugin.CopyOut(probe, &pluginv1alpha.CopyOutRequest{EnvironmentId: id, SrcPath: "/workspace"})
		if err == nil {
			_, err = stream.Recv()
		}
		cancelProbe()
		if status.Code(err) == codes.NotFound {
			return
		}
		// Durable resources disappear inside Destroy before Manager.cleanup
		// publishes registry deletion. An external reap can likewise complete
		// before the daemon's reconciliation removes its cached environment.
		// Only this observed removal transition may converge; success still
		// requires NotFound, never permanent FailedPrecondition or a live entry.
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "environment is being removed") {
			h.t.Fatalf("removed environment lookup must converge to NotFound: %v", err)
		}
		select {
		case <-ctx.Done():
			h.t.Fatalf("removed environment registry did not converge to NotFound within 35s: %v; %s", err, h.log())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (h *pluginHarness) waitHardExpirations(records []recoveryRecord) {
	h.t.Helper()
	remaining := append([]recoveryRecord(nil), records...)
	for len(remaining) != 0 {
		for index := 0; index < len(remaining); {
			record := remaining[index]
			now := time.Now()
			if recoveryVMMAlive(record) {
				// This bound is intentionally much shorter than cleanup_grace.
				if now.After(record.ExpiresAt.Add(2 * time.Second)) {
					h.t.Fatalf("VMM %d executed past hard expiry %s (cleanup grace %s); %s", record.VMMPID, record.ExpiresAt, h.config.Leases.CleanupGrace, h.log())
				}
				index++
				continue
			}
			if now.Before(record.ExpiresAt.Add(-250 * time.Millisecond)) {
				h.t.Fatalf("VMM %s died before configured lifetime: now=%s expiry=%s; %s", record.VMID, now, record.ExpiresAt, h.log())
			}
			remaining = append(remaining[:index], remaining[index+1:]...)
		}
		if len(remaining) != 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestRealPluginHardLeaseExpiration(t *testing.T) {
	h := newPluginHarness(t, func(config *server.Config) {
		config.Pools[0].Replicas = 1
		if config.Leases.MaxLifetime < 30*time.Second {
			config.Leases.MaxLifetime = 30 * time.Second
		}
		config.Leases.CleanupGrace = 20 * time.Second
		config.Leases.ReapInterval = time.Second
	})
	h.pauseWarmPool(1)
	before := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	environment, err := h.plugin.Create(ctx, &pluginv1alpha.CreateRequest{Image: h.config.Pools[0].Name, Name: "hard-lease", EnvironmentTimeout: durationpb.New(8 * time.Second)})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	record := h.claimedRecord(environment.EnvironmentId)
	if record.ExpiresAt.Before(before.Add(8*time.Second)) || record.ExpiresAt.After(after.Add(8*time.Second)) {
		t.Fatalf("hard expiry not bounded by Create receipt: %s before=%s after=%s", record.ExpiresAt, before, after)
	}
	h.start(environment.EnvironmentId)
	execCtx, cancelExec := context.WithDeadline(context.Background(), record.ExpiresAt.Add(3*time.Second))
	defer cancelExec()
	stream, err := h.plugin.Exec(execCtx, &pluginv1alpha.ExecRequest{EnvironmentId: environment.EnvironmentId, Command: []string{"/bin/sh", "-c", "echo lease-exec-running; sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "lease-exec-running\n") {
		message, err := stream.Recv()
		if err != nil {
			t.Fatalf("execution did not start before hard expiry: %v", err)
		}
		if chunk := message.GetData(); chunk != nil {
			output.Write(chunk.Data)
		}
	}
	h.waitHardExpirations([]recoveryRecord{record})
	for {
		message, err := stream.Recv()
		if err != nil {
			break
		}
		if message.GetExecComplete() != nil || message.GetExecFailed() != nil {
			break
		}
	}
	if time.Now().After(record.ExpiresAt.Add(3 * time.Second)) {
		t.Fatal("active Exec did not terminate with hard lease")
	}
	h.waitRecoveryClean([]recoveryRecord{record})
	h.assertClean(environment.EnvironmentId, map[int]string{record.VMMPID: record.APISocketPath}, []string{record.VMID})
	t.Log("requested 8s lifetime killed an executing prewarmed VMM without Remove or execution cleanup grace")
}

func TestRealPluginLifetimeCaps(t *testing.T) {
	const maximum = 45 * time.Second
	h := newPluginHarness(t, func(config *server.Config) {
		config.Pools[0].Replicas = 3
		config.Leases.MaxLifetime = maximum
		config.Leases.CleanupGrace = 20 * time.Second
		config.Leases.ReapInterval = time.Second
		if config.Guest.StartupTimeout > maximum {
			config.Guest.StartupTimeout = maximum
		}
	})
	h.pauseWarmPool(3)
	var records []recoveryRecord
	var environments []string
	for _, test := range []struct {
		name    string
		timeout *durationpb.Duration
	}{
		{name: "Absent"},
		{name: "Zero", timeout: durationpb.New(0)},
		{name: "Oversized", timeout: durationpb.New(2 * maximum)},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			response, err := h.plugin.Create(ctx, &pluginv1alpha.CreateRequest{Image: h.config.Pools[0].Name, Name: test.name, EnvironmentTimeout: test.timeout})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			after := time.Now()
			record := h.claimedRecord(response.EnvironmentId)
			if record.ExpiresAt.Before(before.Add(maximum)) || record.ExpiresAt.After(after.Add(maximum)) {
				t.Fatalf("%s effective cap is not %s: receipt=[%s,%s] expiry=%s", test.name, maximum, before, after, record.ExpiresAt)
			}
			h.start(response.EnvironmentId)
			records = append(records, record)
			environments = append(environments, response.EnvironmentId)
		})
	}
	if len(records) != 3 {
		t.Fatal("not all lifetime variants acquired real VMs")
	}
	h.waitHardExpirations(records)
	h.waitRecoveryClean(records)
	for _, id := range environments {
		h.assertUnknownEnvironment(id)
	}
	h.waitForMachines("all capped environments removed", 5*time.Second, func(machines []*serverv1.Machine) bool { return len(machines) == 0 })
	t.Log("absent, zero and oversized lifetimes each used the configured 45s cap and actual VMM timers without Remove")
}

func (h *pluginHarness) assertRecoveryLive(record recoveryRecord) {
	h.t.Helper()
	if !recoveryVMMAlive(record) {
		h.t.Fatalf("healthy owner's VMM %d was killed", record.VMMPID)
	}
	current := h.journal(record.VMID)
	if current.OwnerPID != record.OwnerPID || current.OwnerStartTime != record.OwnerStartTime || current.State != record.State || current.VMMPID != record.VMMPID {
		h.t.Fatalf("healthy journal changed ownership/state: before=%+v after=%+v", record, current)
	}
	for _, path := range []string{record.APISocketPath, record.VsockPath, record.NetNSPath, record.NetNSPath + ".owner", record.CNICacheDir} {
		if _, err := os.Stat(path); err != nil {
			h.t.Fatalf("healthy resource %s disappeared: %v", path, err)
		}
	}
	if !h.snapshots()[record.SnapshotID] || !h.leases()[record.LeaseID] {
		h.t.Fatalf("healthy VMM's snapshot/lease disappeared: %+v", record)
	}
}

func (h *pluginHarness) recoveryPair() (string, []recoveryRecord) {
	h.t.Helper()
	h.waitForMachines("initial warm VM", h.config.Guest.StartupTimeout+time.Minute, func(machines []*serverv1.Machine) bool {
		return len(machines) == 1 && machines[0].State == "idle" && machines[0].AgentVersion != ""
	})
	environment := h.create(h.config.Pools[0].Name)
	h.start(environment.EnvironmentId)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	stdout, stderr, exit, err := h.exec(ctx, environment.EnvironmentId, []string{"/bin/sh", "-c", "printf old-job > /workspace/recovery-job-marker"})
	cancel()
	if err != nil || exit != 0 {
		h.t.Fatalf("write old-job marker: stdout=%q stderr=%q exit=%d err=%v", stdout, stderr, exit, err)
	}
	machines := h.waitForMachines("claimed job and replacement idle VM", h.config.Guest.StartupTimeout+time.Minute, func(machines []*serverv1.Machine) bool {
		if len(machines) != 2 {
			return false
		}
		claimed, idle := 0, 0
		for _, machine := range machines {
			if machine.State == "claimed" && machine.EnvironmentId == environment.EnvironmentId {
				claimed++
			}
			if machine.State == "idle" && machine.AgentVersion != "" {
				idle++
			}
		}
		return claimed == 1 && idle == 1
	})
	records := make([]recoveryRecord, 0, len(machines))
	for _, machine := range machines {
		record := h.journal(machine.ID)
		if record.State != machine.State || (record.State == "idle" && record.ExpiresAt != nil) || (record.State == "claimed" && record.ExpiresAt == nil) {
			h.t.Fatalf("published state and durable expiry disagree: machine=%+v record=%+v", machine, record)
		}
		records = append(records, record)
	}
	return environment.EnvironmentId, records
}

func TestRealPluginCrashIndependentReaper(t *testing.T) {
	h := newPluginHarness(t, configureRecoveryPool)
	_, records := h.recoveryPair()

	// Both sentinels inhabit the harness namespace, but lack Fireactions instance
	// ownership. A namespace-wide sweep would destroy them and fail this test.
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	const sentinelID = "integration-unrelated-snapshot"
	_, err := h.containerd.SnapshotService("devmapper").Prepare(ctx, sentinelID, "")
	cancel()
	if err != nil {
		t.Fatalf("create unrelated real devmapper snapshot: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		if err := h.containerd.SnapshotService("devmapper").Remove(ctx, sentinelID); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("cleanup unrelated snapshot: %v", err)
		}
	})
	ctx, cancel = context.WithTimeout(h.ctx, 10*time.Second)
	sentinelLease, err := h.containerd.LeasesService().Create(ctx, leases.WithID("integration-unrelated-lease"))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		if err := h.containerd.LeasesService().Delete(ctx, sentinelLease, leases.SynchronousDelete); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("cleanup unrelated lease: %v", err)
		}
	})
	ctx, cancel = context.WithTimeout(h.ctx, 10*time.Second)
	err = h.containerd.LeasesService().AddResource(ctx, sentinelLease, leases.Resource{ID: sentinelID, Type: "snapshots/devmapper"})
	cancel()
	if err != nil {
		t.Fatalf("pin unrelated snapshot against containerd GC: %v", err)
	}
	sleeper := exec.Command("sleep", "600")
	if err = sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	sleeperStart, err := hostProcessStartTime(sleeper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	assertUnrelated := func() {
		t.Helper()
		start, err := hostProcessStartTime(sleeper.Process.Pid)
		if err != nil || start != sleeperStart || sleeper.Process.Signal(syscall.Signal(0)) != nil {
			t.Fatalf("unrelated process did not survive: pid=%d start=%d err=%v", sleeper.Process.Pid, start, err)
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(sleeper.Process.Pid), "cmdline"))
		if err != nil || !bytes.Equal(cmdline, []byte("sleep\x00600\x00")) {
			t.Fatalf("unrelated process exited or became zombie: pid=%d cmdline=%q err=%v", sleeper.Process.Pid, cmdline, err)
		}
		if !h.snapshots()[sentinelID] || !h.leases()[sentinelLease.ID] {
			t.Fatal("independent reaper deleted unowned containerd resources")
		}
	}
	if err = h.reap(); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		h.assertRecoveryLive(record)
	}
	assertUnrelated()

	h.crash()
	for _, record := range records {
		if !recoveryVMMAlive(record) {
			t.Fatalf("daemon SIGKILL did not leave VMM %d for independent recovery", record.VMMPID)
		}
	}
	// Simulate recycled owner and VMM PID evidence in an otherwise genuine
	// durable record. The independent scanner must locate the original owned
	// API-socket process, never signal this live PID with a different starttime.
	var claimed recoveryRecord
	for _, record := range records {
		if record.State == "claimed" {
			claimed = record
		}
	}
	path := filepath.Join(h.config.StateDir, "journal", claimed.VMID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]any{
		"owner_pid": sleeper.Process.Pid, "owner_start_time": strconv.FormatUint(sleeperStart+1, 10),
		"vmm_pid": sleeper.Process.Pid, "vmm_start_time": strconv.FormatUint(sleeperStart+1, 10),
	} {
		fields[key], err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path+".test-tmp", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".test-tmp", path); err != nil {
		t.Fatal(err)
	}
	if err = h.reap(); err != nil {
		t.Fatal(err)
	}
	h.waitRecoveryClean(records)
	assertUnrelated()
	if len(ownedVMMs(h.config.StateDir)) != 0 {
		t.Fatal("owned VMM survived plugin-independent reap")
	}
	if h.process != nil {
		t.Fatal("independent recovery restarted the plugin")
	}
	t.Log("healthy claimed/idle live-owner VMs survived reap; SIGKILL owner and recycled PID evidence were recovered while stopped, with VMM/CNI/netns/socket/snapshot/lease removal and unrelated process/resources intact")
}

func TestRealPluginRestartReconcilesBeforeServing(t *testing.T) {
	h := newPluginHarness(t, configureRecoveryPool)
	oldEnvironment, records := h.recoveryPair()
	h.crash()
	for _, record := range records {
		if !recoveryVMMAlive(record) {
			t.Fatalf("old VMM %d disappeared before startup reconciliation", record.VMMPID)
		}
	}
	// No reaper is invoked here. At the first observed SERVING response, every
	// old claimed AND idle resource must already have been destroyed by startup.
	h.launch(func() {
		for _, record := range records {
			if err := h.recoveryResourcesGone(record); err != nil {
				t.Fatalf("restart advertised SERVING before reconciling old VM %s: %v", record.VMID, err)
			}
		}
	})
	h.assertUnknownEnvironment(oldEnvironment)
	freshIdle := h.pauseWarmPool(1)
	for _, record := range records {
		if freshIdle[0].ID == record.VMID {
			t.Fatal("restart resumed an old idle/claimed VM")
		}
	}
	fresh := h.create(h.config.Pools[0].Name)
	if fresh.EnvironmentId == oldEnvironment {
		t.Fatal("restart resumed an old job environment")
	}
	h.start(fresh.EnvironmentId)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	stdout, stderr, exit, err := h.exec(ctx, fresh.EnvironmentId, []string{"/bin/sh", "-c", "test ! -e /workspace/recovery-job-marker && printf fresh-job-filesystem"})
	cancel()
	if err != nil || exit != 0 || stdout != "fresh-job-filesystem" {
		t.Fatalf("restarted job filesystem: stdout=%q stderr=%q exit=%d err=%v", stdout, stderr, exit, err)
	}
	freshRecord := h.claimedRecord(fresh.EnvironmentId)
	if freshRecord.VMID != freshIdle[0].ID {
		t.Fatal("fresh job did not claim the restarted idle pool")
	}
	h.remove(fresh.EnvironmentId)
	h.waitRecoveryClean([]recoveryRecord{freshRecord})
	h.assertClean(fresh.EnvironmentId, map[int]string{freshRecord.VMMPID: freshRecord.APISocketPath}, []string{freshRecord.VMID})
	t.Log("restart destroyed all old claimed and idle resources before SERVING, forgot the old job and supplied a fresh ready idle VM/filesystem")
}

func configureRecoveryPool(config *server.Config) {
	config.Pools[0].Replicas = 1
	// Recovery setup includes a second real boot. Keep its healthy claimed VM
	// unexpired while testing owner death independently of lease expiry.
	minimum := config.Guest.StartupTimeout + 3*time.Minute
	if config.Leases.MaxLifetime < minimum {
		config.Leases.MaxLifetime = minimum
	}
}
