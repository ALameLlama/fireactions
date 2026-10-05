package guest

import (
	"context"
	"io"

	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
)

const transferChunkSize = 32 << 10

// CopyIn sends destination metadata first, then streams tar data with bounded
// chunks and backpressure from the guest service.
func (c *Client) CopyIn(ctx context.Context, destination string, source io.Reader) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopReaderClose := func() bool { return false }
	if closer, ok := source.(io.Closer); ok {
		stopReaderClose = context.AfterFunc(streamCtx, func() { _ = closer.Close() })
	}
	defer stopReaderClose()
	stream, err := c.rpc.CopyIn(streamCtx)
	if err != nil {
		return translateError(err)
	}
	if err := stream.Send(&agentv1.CopyInChunk{DestPath: &destination}); err != nil {
		return translateError(err)
	}

	buffer := make([]byte, transferChunkSize)
	for {
		if err := streamCtx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if err := stream.Send(&agentv1.CopyInChunk{Data: buffer[:n]}); err != nil {
				return translateError(err)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			if err := streamCtx.Err(); err != nil {
				return err
			}
			cancel()
			return readErr
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return translateError(err)
	}
	return nil
}

// CopyOut receives an archive incrementally and writes every chunk before
// receiving the next one, preserving stream backpressure.
func (c *Client) CopyOut(ctx context.Context, source string, destination io.Writer) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.rpc.CopyOut(streamCtx, &agentv1.CopyOutRequest{SrcPath: source})
	if err != nil {
		return translateError(err)
	}
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return nil
		}
		if recvErr != nil {
			return translateError(recvErr)
		}
		for data := chunk.GetData(); len(data) > 0; {
			n, writeErr := destination.Write(data)
			if n < 0 || n > len(data) {
				cancel()
				return io.ErrShortWrite
			}
			data = data[n:]
			if writeErr != nil {
				cancel()
				return writeErr
			}
			if n == 0 {
				cancel()
				return io.ErrShortWrite
			}
		}
	}
}
