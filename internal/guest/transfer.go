package guest

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/ALameLlama/fireactions/internal/executor"
	agentv1 "github.com/ALameLlama/fireactions/proto/agent/v1"
)

const transferChunkSize = 32 << 10

// CopyIn sends destination metadata first, then streams tar data with bounded
// chunks and backpressure from the guest service.
func (c *Client) CopyIn(ctx context.Context, destination string, source io.Reader) error {
	streamCtx, cancel := context.WithCancel(ctx)
	closeSource := func() {}
	if closer, ok := source.(io.Closer); ok {
		var once sync.Once
		closeSource = func() { once.Do(func() { _ = closer.Close() }) }
	}
	stopReaderClose := context.AfterFunc(streamCtx, closeSource)
	var receiverDone chan struct{}
	defer func() {
		stopReaderClose()
		cancel()
		if receiverDone != nil {
			<-receiverDone
		}
	}()
	stream, err := c.rpc.CopyIn(streamCtx)
	if err != nil {
		return translateError(err)
	}
	// RecvMsg observes the terminal status without closing the send side, unlike
	// CloseAndRecv. An early response must interrupt a blocked upstream reader.
	var uploadEOF atomic.Bool
	var recvErr error
	received := make(chan struct{})
	receiverDone = make(chan struct{})
	go func() {
		defer close(receiverDone)
		recvErr = translateError(stream.RecvMsg(&agentv1.CopyInResponse{}))
		early := !uploadEOF.Load()
		if early && recvErr == nil {
			recvErr = executor.NewError(executor.Internal, "guest closed copy-in before upload completed", io.ErrUnexpectedEOF)
		}
		close(received)
		if early {
			closeSource()
		}
	}()
	sendError := func(err error) error {
		if err == io.EOF {
			<-received
			return recvErr
		}
		return translateError(err)
	}
	if err := stream.Send(&agentv1.CopyInChunk{DestPath: &destination}); err != nil {
		return sendError(err)
	}

	buffer := make([]byte, transferChunkSize)
	for {
		if err := streamCtx.Err(); err != nil {
			return err
		}
		select {
		case <-received:
			return recvErr
		default:
		}
		n, readErr := source.Read(buffer)
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-received:
			return recvErr
		default:
		}
		if n > 0 {
			if err := stream.Send(&agentv1.CopyInChunk{Data: buffer[:n]}); err != nil {
				return sendError(err)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				uploadEOF.Store(true)
				if err := stream.CloseSend(); err != nil {
					return sendError(err)
				}
				break
			}
			if err := streamCtx.Err(); err != nil {
				return err
			}
			cancel()
			return readErr
		}
	}

	<-received
	return recvErr
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
