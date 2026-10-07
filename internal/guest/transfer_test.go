package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type earlyCopyInServer struct {
	agentv1.UnimplementedAgentServiceServer
}

func (earlyCopyInServer) CopyIn(stream agentv1.AgentService_CopyInServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.SendAndClose(&agentv1.CopyInResponse{})
}

func TestCopyInRejectsSuccessfulEarlyClose(t *testing.T) {
	listener := bufconn.Listen(64 * 1024)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, earlyCopyInServer{})
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:early-copy-in", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source := bytes.NewReader(make([]byte, 8<<20))
	err = client.CopyIn(ctx, "/workspace/dest", source)
	if err == nil || executor.KindOf(err) != executor.Internal || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("CopyIn() error = %v, want an Internal unexpected-EOF failure", err)
	}
	if source.Len() == 0 {
		t.Fatal("upload consumed the whole source before the server closed; early closure was not exercised")
	}
}
