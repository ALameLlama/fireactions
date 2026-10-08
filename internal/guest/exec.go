package guest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"

	"github.com/ALameLlama/fireactions/internal/executor"
	agentv1 "github.com/ALameLlama/fireactions/proto/agent/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (c *Client) Exec(ctx context.Context, spec executor.ExecSpec, stdout, stderr io.Writer) (executor.ExecResult, error) {
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return executor.ExecResult{}, executor.NewError(executor.InvalidArgument, "command is empty", nil)
	}
	handleBytes := make([]byte, 16)
	if _, err := rand.Read(handleBytes); err != nil {
		return executor.ExecResult{}, executor.NewError(executor.Internal, "generate guest process handle", err)
	}
	handle := hex.EncodeToString(handleBytes)
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	stream, err := c.rpc.Exec(streamCtx, &agentv1.ExecRequest{
		Command:   spec.Command,
		Env:       spec.Env,
		User:      spec.User,
		Workdir:   spec.Workdir,
		ProcessId: handle,
	})
	if err != nil {
		return executor.ExecResult{}, translateError(err)
	}
	for {
		msg, recvErr := stream.Recv()
		if recvErr == io.EOF {
			if ctx.Err() != nil {
				return executor.ExecResult{}, ctx.Err()
			}
			return executor.ExecResult{}, executor.NewError(executor.Unavailable, "guest process stream ended without completion", nil)
		}
		if recvErr != nil {
			return executor.ExecResult{}, translateError(recvErr)
		}
		switch result := msg.Result.(type) {
		case *agentv1.ExecOutput_Data:
			if result.Data == nil {
				return executor.ExecResult{}, executor.NewError(executor.Unavailable, "guest returned an invalid process output", nil)
			}
			var dst io.Writer
			switch result.Data.Stream {
			case agentv1.ExecStream_STDOUT:
				dst = stdout
			case agentv1.ExecStream_STDERR:
				dst = stderr
			default:
				return executor.ExecResult{}, executor.NewError(executor.Unavailable, "guest returned an unknown output stream", nil)
			}
			if dst != nil {
				n, writeErr := dst.Write(result.Data.Data)
				if writeErr != nil {
					return executor.ExecResult{}, executor.NewError(executor.Unavailable, "host output sink failed", writeErr)
				}
				if n != len(result.Data.Data) {
					return executor.ExecResult{}, executor.NewError(executor.Unavailable, "host output sink returned a short write", io.ErrShortWrite)
				}
			}
		case *agentv1.ExecOutput_Complete:
			if result.Complete == nil {
				return executor.ExecResult{}, executor.NewError(executor.Unavailable, "guest returned an invalid completion", nil)
			}
			return executor.ExecResult{ExitCode: result.Complete.ExitCode}, nil
		case *agentv1.ExecOutput_Failed:
			if result.Failed == nil {
				return executor.ExecResult{}, executor.NewError(executor.Unavailable, "guest returned an invalid launch failure", nil)
			}
			return executor.ExecResult{}, &executor.LaunchError{Message: result.Failed.ErrorMessage}
		default:
			return executor.ExecResult{}, executor.NewError(executor.Unavailable, "guest returned an unterminated process stream", nil)
		}
	}
}

func (c *Client) Kill(ctx context.Context, handle string) error {
	_, err := c.rpc.Kill(ctx, &agentv1.KillRequest{ProcessId: handle})
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return context.Canceled
	}
	return translateError(err)
}
