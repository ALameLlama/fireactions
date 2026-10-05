// Package guest translates the private guest protocol without exposing wire types
// to execution ownership.
package guest

import (
	"context"
	"io"
	"math"

	"github.com/hostinger/fireactions/internal/executor"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
)

// Client owns one VM-specific control connection.
type Client struct {
	conn *grpc.ClientConn
	rpc  agentv1.AgentServiceClient
}

var _ executor.Guest = (*Client)(nil)

func New(conn *grpc.ClientConn) *Client {
	return &Client{conn: conn, rpc: agentv1.NewAgentServiceClient(conn)}
}

// Ready verifies the private agent and applies trusted identity, layout and
// transfer limits before an environment can execute.
func (c *Client) Ready(ctx context.Context, spec executor.ReadySpec) (string, error) {
	if spec.MaxTransferBytes < 0 || spec.MaxArchiveEntries < 0 || spec.MaxArchiveEntries > math.MaxInt32 {
		return "", executor.NewError(executor.InvalidArgument, "invalid guest transfer limits", nil)
	}
	response, err := c.rpc.Ready(ctx, &agentv1.ReadyRequest{
		Directories:       spec.Directories,
		DefaultUser:       spec.DefaultUser,
		MaxTransferBytes:  spec.MaxTransferBytes,
		MaxArchiveEntries: int32(spec.MaxArchiveEntries),
	})
	if err != nil {
		return "", translateError(err)
	}
	return response.Version, nil
}

func (c *Client) Logs(ctx context.Context, follow bool, tailLines int32, send func(string) error) error {
	stream, err := c.rpc.GetLogs(ctx, &agentv1.GetLogsRequest{Follow: follow, TailLines: tailLines})
	if err != nil {
		return translateError(err)
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return translateError(err)
		}
		if err = send(chunk.Line); err != nil {
			return err
		}
	}
}

func (c *Client) Close() error { return c.conn.Close() }
