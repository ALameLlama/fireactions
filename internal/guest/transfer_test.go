package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type copyInTestServer struct {
	agentv1.UnimplementedAgentServiceServer
	copyIn func(agentv1.AgentService_CopyInServer) error
}

func (s copyInTestServer) CopyIn(stream agentv1.AgentService_CopyInServer) error {
	return s.copyIn(stream)
}

func newCopyInTestClient(t *testing.T, handler func(agentv1.AgentService_CopyInServer) error) *Client {
	t.Helper()
	listener := bufconn.Listen(64 * 1024)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, copyInTestServer{copyIn: handler})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:early-copy-in", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestCopyInRejectsSuccessfulEarlyClose(t *testing.T) {
	client := newCopyInTestClient(t, func(stream agentv1.AgentService_CopyInServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		return stream.SendAndClose(&agentv1.CopyInResponse{})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source := bytes.NewReader(make([]byte, 8<<20))
	err := client.CopyIn(ctx, "/workspace/dest", source)
	if err == nil || executor.KindOf(err) != executor.Internal || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("CopyIn() error = %v, want an Internal unexpected-EOF failure", err)
	}
	if source.Len() == 0 {
		t.Fatal("upload consumed the whole source before the server closed; early closure was not exercised")
	}
}

// blockedCopyInSource models a Runner that sent metadata but has not sent its
// next frame. Only Close, not a new input frame, can release Read.
type blockedCopyInSource struct {
	reading   chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
}

func newBlockedCopyInSource() *blockedCopyInSource {
	return &blockedCopyInSource{reading: make(chan struct{}), closed: make(chan struct{})}
}

func (s *blockedCopyInSource) Read([]byte) (int, error) {
	s.readOnce.Do(func() { close(s.reading) })
	<-s.closed
	return 0, io.ErrClosedPipe
}

func (s *blockedCopyInSource) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func TestCopyInClosesBlockedSourceOnGuestRejection(t *testing.T) {
	testCopyInBlockedSourceTerminal(t, false)
}

func TestCopyInClosesBlockedSourceOnSuccessfulEarlyClose(t *testing.T) {
	testCopyInBlockedSourceTerminal(t, true)
}

func testCopyInBlockedSourceTerminal(t *testing.T, success bool) {
	t.Helper()
	source := newBlockedCopyInSource()
	guestErr := status.Error(codes.InvalidArgument, "destination escapes workspace")
	client := newCopyInTestClient(t, func(stream agentv1.AgentService_CopyInServer) error {
		metadata, err := stream.Recv()
		if err != nil {
			return err
		}
		if metadata.GetDestPath() != "/workspace/dest" {
			return status.Error(codes.Internal, "missing destination metadata")
		}
		select {
		case <-source.reading:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		if success {
			return stream.SendAndClose(&agentv1.CopyInResponse{})
		}
		return guestErr
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.CopyIn(ctx, "/workspace/dest", source) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		cancel()
		_ = source.Close()
		<-done
		t.Fatal("CopyIn did not return the guest's terminal status while the source was blocked")
	}
	select {
	case <-source.closed:
	default:
		t.Fatal("CopyIn returned without closing its blocked source")
	}
	if success {
		if executor.KindOf(err) != executor.Internal || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("CopyIn() error = %v, want an Internal unexpected-EOF failure", err)
		}
		return
	}
	if executor.KindOf(err) != executor.InvalidArgument || err.Error() != status.Convert(guestErr).Message() || !errors.Is(err, guestErr) {
		t.Fatalf("CopyIn() error = %v, want the original guest rejection %v", err, guestErr)
	}
}

type copyInFinalRead struct {
	data []byte
	err  error
}

func (r *copyInFinalRead) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestCopyInNormalCompletion(t *testing.T) {
	payload := bytes.Repeat([]byte("archive"), transferChunkSize)
	for _, test := range []struct {
		name     string
		data     []byte
		finalEOF bool
		guestErr error
	}{
		{name: "empty"},
		{name: "multiple chunks", data: payload},
		{name: "data and EOF together", data: payload, finalEOF: true},
		{name: "guest rejects completed upload", data: payload, guestErr: status.Error(codes.InvalidArgument, "invalid archive")},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newCopyInTestClient(t, func(stream agentv1.AgentService_CopyInServer) error {
				metadata, err := stream.Recv()
				if err != nil {
					return err
				}
				if metadata.GetDestPath() != "/workspace/dest" {
					return status.Error(codes.Internal, "missing destination metadata")
				}
				var received bytes.Buffer
				for {
					chunk, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						return err
					}
					if len(chunk.GetData()) > transferChunkSize {
						return status.Error(codes.Internal, "oversized upload chunk")
					}
					received.Write(chunk.GetData())
				}
				if !bytes.Equal(received.Bytes(), test.data) {
					return status.Error(codes.Internal, "upload data differs")
				}
				if test.guestErr != nil {
					return test.guestErr
				}
				return stream.SendAndClose(&agentv1.CopyInResponse{})
			})
			var source io.Reader = bytes.NewReader(test.data)
			if test.finalEOF {
				source = &copyInFinalRead{data: test.data, err: io.EOF}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := client.CopyIn(ctx, "/workspace/dest", source)
			if test.guestErr == nil {
				if err != nil {
					t.Fatalf("CopyIn() error = %v", err)
				}
			} else if executor.KindOf(err) != executor.InvalidArgument || err.Error() != status.Convert(test.guestErr).Message() || !errors.Is(err, test.guestErr) {
				t.Fatalf("CopyIn() error = %v, want the original guest error %v", err, test.guestErr)
			}
		})
	}
}

type copyInReadFunc func([]byte) (int, error)

func (f copyInReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestCopyInPreservesSourceReadError(t *testing.T) {
	for _, data := range []string{"", "final archive bytes"} {
		t.Run(data, func(t *testing.T) {
			metadataReceived := make(chan struct{})
			guestCanceled := make(chan struct{})
			client := newCopyInTestClient(t, func(stream agentv1.AgentService_CopyInServer) error {
				if _, err := stream.Recv(); err != nil {
					return err
				}
				close(metadataReceived)
				<-stream.Context().Done()
				close(guestCanceled)
				return stream.Context().Err()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			readErr := errors.New("upstream archive read failed")
			source := copyInReadFunc(func(p []byte) (int, error) {
				select {
				case <-metadataReceived:
					return copy(p, data), readErr
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			})
			err := client.CopyIn(ctx, "/workspace/dest", source)
			if err != readErr {
				t.Fatalf("CopyIn() error = %v, want original source error %v", err, readErr)
			}
			select {
			case <-guestCanceled:
			case <-ctx.Done():
				t.Fatal("CopyIn left the guest RPC active after a source read failure")
			}
		})
	}
}

func TestCopyInParentCancellationClosesBlockedSource(t *testing.T) {
	source := newBlockedCopyInSource()
	client := newCopyInTestClient(t, func(stream agentv1.AgentService_CopyInServer) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.CopyIn(ctx, "/workspace/dest", source) }()
	select {
	case <-source.reading:
	case <-time.After(2 * time.Second):
		cancel()
		_ = source.Close()
		<-done
		t.Fatal("CopyIn did not begin reading its source")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CopyIn() error = %v, want parent cancellation", err)
		}
	case <-time.After(2 * time.Second):
		_ = source.Close()
		<-done
		t.Fatal("CopyIn did not unblock after parent cancellation")
	}
	select {
	case <-source.closed:
	default:
		t.Fatal("parent cancellation did not close the source")
	}
}
