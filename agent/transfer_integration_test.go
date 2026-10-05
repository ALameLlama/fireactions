package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/hostinger/fireactions/internal/guest"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestGuestTransferStreamingRoundTrip(t *testing.T) {
	identity, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a, err := New(Config{Port: 9001, LogLevel: "info", WorkspaceRoot: root, DefaultUser: identity.Username}, func(a *Agent) { a.logFile = filepath.Join(root, "diagnostics.log") })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	listener := bufconn.Listen(64 * 1024)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, a)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:transfer", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	client := guest.New(conn)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = a.applyReady(&agentv1.ReadyRequest{Directories: []string{"/workspace/.fireactions/act", "/workspace/.fireactions/tmp"}, DefaultUser: identity.Username}); err != nil {
		t.Fatal(err)
	}
	const size = 64 * 1024 * 1024
	block := make([]byte, 32*1024)
	for i := range block {
		block[i] = byte(i*17 + 3)
	}
	expected := sha256.New()
	for range size / len(block) {
		expected.Write(block)
	}
	input, inputWriter := io.Pipe()
	defer input.Close()
	go func() {
		archive := tar.NewWriter(inputWriter)
		e := archive.WriteHeader(&tar.Header{Name: "binary", Mode: 0751, Size: size, ModTime: time.Unix(1700000000, 0)})
		for i := 0; e == nil && i < size/len(block); i++ {
			_, e = archive.Write(block)
		}
		if e == nil {
			e = archive.Close()
		}
		inputWriter.CloseWithError(e)
	}()
	if err = client.CopyIn(ctx, "/workspace/payload", input); err != nil {
		t.Fatal(err)
	}
	output, outputWriter := io.Pipe()
	defer output.Close()
	copied := make(chan error, 1)
	go func() {
		e := client.CopyOut(ctx, "/workspace/payload/binary", outputWriter)
		outputWriter.CloseWithError(e)
		copied <- e
	}()
	archive := tar.NewReader(output)
	header, err := archive.Next()
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.New()
	n, err := io.CopyBuffer(actual, archive, make([]byte, 32*1024))
	if err != nil {
		t.Fatal(err)
	}
	if n != size || !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) || header.Mode != 0751 || !header.ModTime.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("transfer mismatch bytes=%d mode=%o mtime=%v", n, header.Mode, header.ModTime)
	}
	if _, err = archive.Next(); err != io.EOF {
		t.Fatalf("archive end: %v", err)
	}
	if _, err = io.Copy(io.Discard, output); err != nil {
		t.Fatal(err)
	}
	if err = <-copied; err != nil {
		t.Fatal(err)
	}
	t.Logf("private gRPC transfer: bytes=%d sha256=%x mode=%o mtime=%s", n, actual.Sum(nil), header.Mode, header.ModTime.UTC().Format(time.RFC3339))
}
