package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALameLlama/fireactions/internal/executor"
	"github.com/ALameLlama/fireactions/internal/guest"
	agentv1 "github.com/ALameLlama/fireactions/proto/agent/v1"
	"golang.org/x/sys/unix"
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

func TestGuestCopyInPreservesMidUploadRejection(t *testing.T) {
	const size = 8 << 20
	archive := makeTar(t, &tar.Header{
		Name:     "payload",
		Mode:     0600,
		Size:     size,
		Linkname: strings.Repeat("x", size),
	})
	for _, test := range []struct {
		name        string
		destination string
		maxBytes    int64
		wantKind    executor.Kind
		detail      string
	}{
		{
			name:        "invalid destination",
			destination: "/outside",
			maxBytes:    16 << 20,
			wantKind:    executor.InvalidArgument,
			detail:      "destination",
		},
		{
			name:        "oversized archive",
			destination: "/workspace/payload",
			maxBytes:    1 << 20,
			wantKind:    executor.ResourceExhausted,
			detail:      "limit",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, _ := newTransferTestAgent(t, test.maxBytes, 20)
			listener := bufconn.Listen(64 * 1024)
			server := grpc.NewServer()
			agentv1.RegisterAgentServiceServer(server, a)
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient("passthrough:transfer-rejection", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return listener.DialContext(ctx)
			}))
			if err != nil {
				t.Fatal(err)
			}
			client := guest.New(conn)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			source := bytes.NewReader(archive)
			err = client.CopyIn(ctx, test.destination, source)
			if err == nil || executor.KindOf(err) != test.wantKind {
				t.Fatalf("CopyIn() error = %v (kind %v), want kind %v", err, executor.KindOf(err), test.wantKind)
			}
			if !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("CopyIn() lost guest rejection diagnostic: %v", err)
			}
			if source.Len() == 0 {
				t.Fatal("upload consumed the whole archive before rejection; mid-upload rejection was not exercised")
			}
		})
	}
}

func runTransferDescriptorSubprocess(t *testing.T, scenario string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestTransferDescriptorHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "FIREACTIONS_TRANSFER_FD_HELPER="+scenario)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("low-descriptor %s subprocess: %v\n%s", scenario, err, output)
	}
}

func TestCopyOutDeepTreeUnderLowDescriptorLimit(t *testing.T) {
	runTransferDescriptorSubprocess(t, "deep")
}

func TestCopyInWideUploadUnderLowDescriptorLimit(t *testing.T) {
	runTransferDescriptorSubprocess(t, "wide")
}

func TestTransferDescriptorHelper(t *testing.T) {
	scenario := os.Getenv("FIREACTIONS_TRANSFER_FD_HELPER")
	if scenario == "" {
		t.Skip("subprocess helper")
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	switch scenario {
	case "deep":
		limit.Cur = 1024
	case "wide":
		limit.Cur = 128
	default:
		t.Fatalf("unknown descriptor helper scenario %q", scenario)
	}
	if limit.Cur > limit.Max {
		t.Fatalf("descriptor hard limit %d is below required soft limit %d", limit.Max, limit.Cur)
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	if scenario == "wide" {
		testWideUploadWithBoundedDescriptors(t)
	} else {
		testDeepCopyOutWithBoundedDescriptors(t)
	}
}

func testWideUploadWithBoundedDescriptors(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 1<<20, 1000)
	stamp := time.Unix(1_700_000_000, 123_456_789)
	headers := make([]*tar.Header, 200)
	for i := range headers {
		headers[i] = &tar.Header{Name: fmt.Sprintf("file-%03d", i), Typeflag: tar.TypeReg, Mode: 0640, Size: 7, Linkname: "payload", ModTime: stamp, Format: tar.FormatPAX}
	}
	if err := a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/dest", makeTar(t, headers...)))); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(workspace, "dest")
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != len(headers) {
		t.Fatalf("upload names: count=%d err=%v", len(entries), err)
	}
	for i, entry := range entries {
		if entry.Name() != headers[i].Name {
			t.Fatalf("upload name %q, want %q", entry.Name(), headers[i].Name)
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(destination, entry.Name()))
		if err != nil || string(data) != "payload" || info.Mode().Perm() != 0640 || !info.ModTime().Equal(stamp) {
			t.Fatalf("upload %q: info=%v data=%q err=%v", entry.Name(), info, data, err)
		}
	}
}

func testDeepCopyOutWithBoundedDescriptors(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 8<<20, 1000)
	source := filepath.Join(workspace, "tree")
	if err := os.Mkdir(source, 0750); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 123_456_789)
	expected := map[string]*tar.Header{
		".": {Name: ".", Typeflag: tar.TypeDir, Mode: 0750, ModTime: stamp},
	}
	directories := []string{source}
	name := "."
	for range 600 {
		name = path.Join(name, "d")
		directory := filepath.Join(source, filepath.FromSlash(name))
		if err := os.Mkdir(directory, 0750); err != nil {
			t.Fatal(err)
		}
		directories = append(directories, directory)
		expected[name] = &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0750, ModTime: stamp}
	}
	payloadName := path.Join(name, "payload")
	payload := "deep payload\x00\xff"
	fullPayload := filepath.Join(source, filepath.FromSlash(payloadName))
	if err := os.WriteFile(fullPayload, []byte(payload), 0641); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fullPayload, 0641); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fullPayload, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	expected[payloadName] = &tar.Header{Name: payloadName, Typeflag: tar.TypeReg, Mode: 0641, Size: int64(len(payload)), ModTime: stamp}
	linkName := path.Join(name, "relative")
	fullLink := filepath.Join(source, filepath.FromSlash(linkName))
	if err := os.Symlink("payload", fullLink); err != nil {
		t.Fatal(err)
	}
	times := []unix.Timespec{unix.NsecToTimespec(stamp.UnixNano()), unix.NsecToTimespec(stamp.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, fullLink, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	expected[linkName] = &tar.Header{Name: linkName, Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: "payload", ModTime: stamp}
	for _, directory := range directories {
		if err := os.Chmod(directory, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(directory, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	stream := &copyOutTestStream{ctx: context.Background()}
	if err := a.CopyOut(&agentv1.CopyOutRequest{SrcPath: "/workspace/tree"}, stream); err != nil {
		t.Fatalf("CopyOut 600-level tree at FD limit 1024: %v", err)
	}
	reader := tar.NewReader(bytes.NewReader(bytes.Join(stream.chunks, nil)))
	seen := make(map[string]bool)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		want, ok := expected[header.Name]
		if !ok || seen[header.Name] {
			t.Fatalf("unexpected or duplicate full archive path %q", header.Name)
		}
		seen[header.Name] = true
		if header.Typeflag != want.Typeflag || header.Mode != want.Mode || header.Size != want.Size || header.Linkname != want.Linkname || !header.ModTime.Equal(want.ModTime) {
			t.Fatalf("archive metadata %q: got=%+v want=%+v", header.Name, header, want)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		wantData := ""
		if header.Name == payloadName {
			wantData = payload
		}
		if string(data) != wantData {
			t.Fatalf("archive content %q: got=%q want=%q", header.Name, data, wantData)
		}
	}
	if len(seen) != len(expected) {
		t.Fatalf("archive graph has %d entries, want %d", len(seen), len(expected))
	}
}
