package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
	"github.com/hostinger/fireactions/internal/guest"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestCIDAllocationReservedValuesAndOverflow(t *testing.T) {
	var counter atomic.Uint32
	cid, err := allocateCID(&counter)
	if err != nil || cid != 3 {
		t.Fatalf("first CID must skip reserved values: %d, %v", cid, err)
	}
	counter.Store(^uint32(0) - 2)
	cid, err = allocateCID(&counter)
	if err != nil || cid != ^uint32(0)-1 {
		t.Fatalf("last usable CID rejected: %d, %v", cid, err)
	}
	if _, err := allocateCID(&counter); err == nil {
		t.Fatal("reserved ANY CID was allocated")
	}
	if counter.Load() != ^uint32(0)-1 {
		t.Fatal("exhausted allocator wrapped")
	}
}

func TestConcurrentCIDAllocationNeverDuplicates(t *testing.T) {
	var counter atomic.Uint32
	counter.Store(2)
	ids := make(chan uint32, 128)
	var workers sync.WaitGroup
	for range 128 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			cid, err := allocateCID(&counter)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- cid
		}()
	}
	workers.Wait()
	close(ids)
	seen := make(map[uint32]bool)
	for id := range ids {
		if id < 3 || id == ^uint32(0) || seen[id] {
			t.Fatalf("invalid or duplicate CID %d", id)
		}
		seen[id] = true
	}
	if len(seen) != 128 {
		t.Fatalf("only allocated %d CIDs", len(seen))
	}
}

func TestImageInfoUsesResolvedOCIConfiguration(t *testing.T) {
	image := ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		Config:   ocispec.ImageConfig{User: "1000:1000", Env: []string{"PATH=/image/bin", "IMAGE_ONLY=value", "DUP=first", "DUP=last"}},
	}
	info, err := imageVMInfo(image)
	if err != nil {
		t.Fatal(err)
	}
	if info.Layout.Root != "/workspace" || info.Layout.Act != "/workspace/.fireactions/act" ||
		info.Layout.ToolCache != "/workspace/.fireactions/toolcache" || info.Layout.Temp != "/workspace/.fireactions/tmp" ||
		info.Layout.OS != "Linux" || info.Layout.Arch != "X64" {
		t.Fatalf("wrong canonical guest layout: %#v", info.Layout)
	}
	if info.ImageEnv["PATH"] != "/image/bin" || info.ImageEnv["DUP"] != "last" || info.DefaultUser != "1000:1000" {
		t.Fatalf("resolved image configuration lost: %#v", info)
	}
	if len(info.ImageEnv) != 3 {
		t.Fatalf("daemon environment leaked into image environment: %#v", info.ImageEnv)
	}
	machine := &Machine{info: info, defaultUser: "profile-user"}
	copy := machine.Info()
	if copy.DefaultUser != "profile-user" {
		t.Fatalf("profile default user did not override image user: %#v", copy)
	}
	copy.ImageEnv["PATH"] = "caller-mutated"
	if machine.Info().ImageEnv["PATH"] != "/image/bin" {
		t.Fatal("caller modified machine-owned image environment")
	}
	image.Architecture = "arm64"
	image.Config = ocispec.ImageConfig{}
	info, err = imageVMInfo(image)
	if err != nil || info.Layout.Arch != "ARM64" || info.DefaultUser != "ci" || len(info.ImageEnv) != 0 || info.Layout.DefaultPath == "" {
		t.Fatalf("wrong empty-image defaults: %#v, %v", info, err)
	}
	for _, platform := range []ocispec.Platform{{OS: "windows", Architecture: "amd64"}, {OS: "linux", Architecture: "riscv64"}} {
		image.Platform = platform
		if _, err := imageVMInfo(image); err == nil {
			t.Fatalf("unsupported platform accepted: %#v", platform)
		}
	}
}

func TestMachineConnectionIsCachedAndDestructionOwnsClose(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:test", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client := guest.New(conn)
	machine := &Machine{State: "idle", resources: &ownedResources{}, guestConn: conn, guestClient: client}
	if machine.Guest() != client {
		t.Fatal("guest client was not cached by the machine")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := machine.Destroy(ctx); err != nil {
		t.Fatalf("cancelled caller prevented independent cleanup: %v", err)
	}
	if _, err := machine.ConnectToGuestAgent(context.Background()); err == nil {
		t.Fatal("destroyed VM reopened its control channel")
	}
	if machine.Metadata().State != "removing" {
		t.Fatal("destroyed machine is still advertised usable")
	}
	if err := machine.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMachineDestroyUsesUncancelledBoundedContext(t *testing.T) {
	owned := filepath.Join(t.TempDir(), "owned-resource")
	if err := os.WriteFile(owned, []byte("resource"), 0600); err != nil {
		t.Fatal(err)
	}
	machine := &Machine{resources: &ownedResources{steps: []cleanupStep{{name: "file", run: func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("cleanup inherited cancelled request")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("cleanup is not bounded")
		}
		return os.Remove(owned)
	}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := machine.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned resource retained after destruction: %v", err)
	}
}

func TestMachineMetadataUpdatesAreAtomic(t *testing.T) {
	machine := &Machine{State: "idle"}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				machine.SetState("claimed", "owned")
				machine.SetAgentVersion("version")
				metadata := machine.Metadata()
				if metadata.State == "claimed" && metadata.EnvironmentID != "owned" || metadata.State == "idle" && metadata.EnvironmentID != "" {
					t.Error("observed torn lifecycle metadata")
				}
				machine.SetState("idle", "")
			}
		}()
	}
	workers.Wait()
}

func TestUnprovisionedMachineHasNoAddress(t *testing.T) {
	if addr := (&Machine{}).GetAddr(); addr != "" {
		t.Fatalf("unprovisioned VM has address %q", addr)
	}
}

type invalidArgumentReadyServer struct {
	agentv1.UnimplementedAgentServiceServer
}

func (invalidArgumentReadyServer) Ready(context.Context, *agentv1.ReadyRequest) (*agentv1.ReadyResponse, error) {
	return nil, status.Error(codes.InvalidArgument, "invalid readiness request")
}

func TestConnectToGuestAgentReturnsPermanentReadyError(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, invalidArgumentReadyServer{})
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	machine := &Machine{
		guestConn:      conn,
		guestClient:    guest.New(conn),
		startupTimeout: 5 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err = machine.ConnectToGuestAgent(ctx)
	if got := executor.KindOf(err); got != executor.InvalidArgument {
		t.Fatalf("ConnectToGuestAgent error kind = %v, want %v (err: %v)", got, executor.InvalidArgument, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ConnectToGuestAgent consumed the caller deadline: %v", err)
	}
}
