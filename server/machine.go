package server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Machine holds metadata about a Firecracker machine and its associated resources.
type Machine struct {
	*firecracker.Machine

	Name          string
	Pool          string
	CreatedAt     time.Time
	State         string
	AgentVersion  string
	EnvironmentID string

	vsockCID    uint32
	vsockPath   string
	leaseCancel func(context.Context) error // containerd lease cancel function
	vmmCtx      context.Context
	vmmCancel   context.CancelFunc
}

func (m *Machine) ConnectToGuestAgent(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		return vsock.DialContext(ctx, m.vsockPath, 9001)
	}

	// Create gRPC client with VSOCK transport
	// Use "passthrough:" resolver to bypass name resolution and pass directly to dialer
	conn, err := grpc.NewClient(
		"passthrough:vsock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("grpc dial: %w", err)
	}

	client := agentv1.NewAgentServiceClient(conn)
	return conn, client, nil
}

func (m *Machine) GetAddr() string {
	addr := ""
	if len(m.Cfg.NetworkInterfaces) > 0 && m.Cfg.NetworkInterfaces[0].StaticConfiguration != nil && m.Cfg.NetworkInterfaces[0].StaticConfiguration.IPConfiguration != nil {
		addr = m.Cfg.NetworkInterfaces[0].StaticConfiguration.IPConfiguration.IPAddr.IP.String()
	}

	return addr
}
