package v1alpha

import (
	"context"
	"errors"
	"io"

	pluginv1alpha "github.com/hostinger/fireactions/proto/forgejo/plugin/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type streamWriter struct{ send func([]byte) error }

func (w *streamWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) != 0 {
		n := len(p)
		if n > copyOutChunkSize {
			n = copyOutChunkSize
		}
		chunk := append([]byte(nil), p[:n]...)
		if err := w.send(chunk); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

type copyInResult struct {
	chunk *pluginv1alpha.CopyInChunk
	err   error
}
type copyInReader struct {
	ctx          context.Context
	cancel       context.CancelFunc
	chunks       <-chan copyInResult
	current      []byte
	first        *pluginv1alpha.CopyInChunk
	firstPending bool
	err          error
}

func newCopyInReader(stream pluginv1alpha.BackendPlugin_CopyInServer, first *pluginv1alpha.CopyInChunk) *copyInReader {
	ctx, cancel := context.WithCancel(stream.Context())
	chunks := make(chan copyInResult, 1)
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			chunk, err := stream.Recv()
			select {
			case chunks <- copyInResult{chunk: chunk, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return &copyInReader{ctx: ctx, cancel: cancel, chunks: chunks, first: first, firstPending: true}
}

func (r *copyInReader) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	return nil
}

func (r *copyInReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.current) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		var next copyInResult
		isFirst := r.firstPending
		if isFirst {
			next.chunk = r.first
			r.first = nil
			r.firstPending = false
		} else {
			select {
			case next = <-r.chunks:
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			}
		}
		if next.err != nil {
			if errors.Is(next.err, io.EOF) {
				r.err = io.EOF
				continue
			}
			r.err = next.err
			continue
		}
		chunk := next.chunk
		if chunk == nil {
			r.err = status.Error(codes.Internal, "copy-in stream returned an empty frame")
			continue
		}
		if !isFirst && (chunk.EnvironmentId != nil || chunk.DestPath != nil) {
			r.err = status.Error(codes.InvalidArgument, "copy-in metadata is only allowed in the first chunk")
			continue
		}
		if len(chunk.Data) > copyInMaxChunk {
			r.err = status.Error(codes.ResourceExhausted, "copy-in chunk exceeds 1 MiB")
			continue
		}
		r.current = chunk.Data
	}
	n := copy(p, r.current)
	r.current = r.current[n:]
	return n, nil
}
