package guest

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	agentv1 "github.com/ALameLlama/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type logTestServer struct {
	agentv1.UnimplementedAgentServiceServer
	canceled chan struct{}
}

func (s logTestServer) GetLogs(_ *agentv1.GetLogsRequest, stream agentv1.AgentService_GetLogsServer) error {
	if err := stream.Send(&agentv1.GetLogsResponse{Line: "log line\n"}); err != nil {
		return err
	}
	<-stream.Context().Done()
	close(s.canceled)
	return stream.Context().Err()
}

func TestLogsCancelsStreamOnSinkFailure(t *testing.T) {
	canceled := make(chan struct{})
	listener := bufconn.Listen(64 * 1024)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, logTestServer{canceled: canceled})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:logs", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sinkErr := errors.New("log sink failed")
	if err := client.Logs(ctx, true, 0, func(string) error { return sinkErr }); err != sinkErr {
		t.Fatalf("Logs() error = %v, want original sink error %v", err, sinkErr)
	}
	select {
	case <-canceled:
		if err := ctx.Err(); err != nil {
			t.Fatalf("Logs() canceled its parent context: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Logs() left the guest stream active after sink failure")
	}
}
