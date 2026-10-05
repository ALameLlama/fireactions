package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type copyInTestStream struct {
	agentv1.AgentService_CopyInServer
	ctx      context.Context
	chunks   []*agentv1.CopyInChunk
	response *agentv1.CopyInResponse
	index    int
	onEnd    func() error
}

func (s *copyInTestStream) Context() context.Context { return s.ctx }
func (s *copyInTestStream) Recv() (*agentv1.CopyInChunk, error) {
	if s.index == len(s.chunks) {
		if s.onEnd != nil {
			onEnd := s.onEnd
			s.onEnd = nil
			return nil, onEnd()
		}
		return nil, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}
func (s *copyInTestStream) SendAndClose(response *agentv1.CopyInResponse) error {
	s.response = response
	return nil
}

type copyOutTestStream struct {
	agentv1.AgentService_CopyOutServer
	ctx         context.Context
	chunks      [][]byte
	onFirstSend func()
}

func (s *copyOutTestStream) Context() context.Context { return s.ctx }
func (s *copyOutTestStream) Send(chunk *agentv1.CopyOutChunk) error {
	s.chunks = append(s.chunks, append([]byte(nil), chunk.Data...))
	if len(s.chunks) == 1 && s.onFirstSend != nil {
		s.onFirstSend()
	}
	return nil
}

func newTransferTestAgent(t *testing.T, maxBytes int64, maxEntries int) (*Agent, string) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a, err := New(Config{
		Port:              9001,
		LogLevel:          "info",
		WorkspaceRoot:     root,
		DefaultUser:       current.Username,
		MaxTransferBytes:  maxBytes,
		MaxArchiveEntries: maxEntries,
	}, func(a *Agent) { a.logFile = filepath.Join(root, "agent.log") })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.applyReady(&agentv1.ReadyRequest{DefaultUser: current.Username, Directories: []string{"/workspace"}, MaxTransferBytes: int64(maxBytes), MaxArchiveEntries: int32(maxEntries)}); err != nil {
		t.Fatalf("applyReady() error = %v", err)
	}
	return a, root
}

func makeTar(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, header := range headers {
		if header == nil {
			continue
		}
		wireHeader := *header
		if wireHeader.Typeflag == tar.TypeReg || wireHeader.Typeflag == tar.TypeRegA {
			wireHeader.Linkname = ""
		}
		if err := writer.WriteHeader(&wireHeader); err != nil {
			t.Fatalf("WriteHeader(%q): %v", header.Name, err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			if _, err := io.CopyN(writer, strings.NewReader(header.Linkname), header.Size); err != nil {
				t.Fatalf("writing %q: %v", header.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("tar Close(): %v", err)
	}
	return buffer.Bytes()
}

func copyInChunks(ctx context.Context, chunks ...*agentv1.CopyInChunk) *copyInTestStream {
	return &copyInTestStream{ctx: ctx, chunks: chunks}
}

func firstChunk(path string, data []byte) *agentv1.CopyInChunk {
	return &agentv1.CopyInChunk{DestPath: &path, Data: data}
}

func requireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if got := status.Code(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}

func TestCopyInExtractsNestedMetadataAndLinks(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 1<<20, 100)
	stamp := time.Unix(1_700_000_000, 123_456_789)
	payload := []byte{0, 1, 2, 0xff, 0x80, '\n'}
	archive := makeTar(t,
		&tar.Header{Name: "nested/", Typeflag: tar.TypeDir, Mode: 0750, ModTime: stamp, Format: tar.FormatPAX},
		&tar.Header{Name: "nested/program", Typeflag: tar.TypeReg, Mode: 0751, Size: int64(len(payload)), ModTime: stamp, Linkname: string(payload), Format: tar.FormatPAX},
		&tar.Header{Name: "nested/hard", Typeflag: tar.TypeLink, Linkname: "nested/program", Mode: 0751, ModTime: stamp, Format: tar.FormatPAX},
		&tar.Header{Name: "nested/relative", Typeflag: tar.TypeSymlink, Linkname: "program", Mode: 0777, ModTime: stamp, Format: tar.FormatPAX},
	)
	stream := copyInChunks(context.Background(), firstChunk("/workspace/dest", archive))
	if err := a.CopyIn(stream); err != nil {
		t.Fatalf("CopyIn() error = %v", err)
	}
	if stream.response == nil {
		t.Fatal("CopyIn() did not close successfully")
	}
	file := filepath.Join(workspace, "dest", "nested", "program")
	got, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("regular file content = %v, %v", got, err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0751 || !info.ModTime().Equal(stamp) {
		t.Fatalf("file mode/mtime = %o/%v", info.Mode().Perm(), info.ModTime())
	}
	dir, err := os.Stat(filepath.Join(workspace, "dest", "nested"))
	if err != nil || dir.Mode().Perm() != 0750 || !dir.ModTime().Equal(stamp) {
		t.Fatalf("directory metadata = %v, %v", dir, err)
	}
	hardInfo, err := os.Stat(filepath.Join(workspace, "dest", "nested", "hard"))
	if err != nil || !os.SameFile(info, hardInfo) {
		t.Fatalf("hard link does not share file: %v", err)
	}
	link, err := os.Readlink(filepath.Join(workspace, "dest", "nested", "relative"))
	if err != nil || link != "program" {
		t.Fatalf("relative symlink target = %q, %v", link, err)
	}
	linkInfo, err := os.Lstat(filepath.Join(workspace, "dest", "nested", "relative"))
	if err != nil || !linkInfo.ModTime().Equal(stamp) {
		t.Fatalf("symlink metadata = %v, %v", linkInfo, err)
	}
}

func TestCopyInAcceptsEmptyArchiveAndEmptyDataChunks(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 1<<20, 10)
	t.Run("metadata only", func(t *testing.T) {
		stream := copyInChunks(context.Background(), firstChunk("/workspace/empty", nil))
		if err := a.CopyIn(stream); err != nil {
			t.Fatalf("CopyIn() empty tar error = %v", err)
		}
		if stream.response == nil {
			t.Fatal("metadata-only stream did not succeed")
		}
		if info, err := os.Stat(filepath.Join(workspace, "empty")); err != nil || !info.IsDir() {
			t.Fatalf("empty destination = %v, %v", info, err)
		}
	})
	t.Run("tar writer empty archive", func(t *testing.T) {
		stream := copyInChunks(context.Background(), firstChunk("/workspace/empty-tar", makeTar(t)))
		if err := a.CopyIn(stream); err != nil {
			t.Fatalf("CopyIn() empty archive error = %v", err)
		}
	})
	t.Run("empty intermediate payload", func(t *testing.T) {
		archive := makeTar(t, &tar.Header{Name: "value", Typeflag: tar.TypeReg, Mode: 0600, Size: 2, Linkname: "ok"})
		split := len(archive) / 2
		chunks := []*agentv1.CopyInChunk{firstChunk("/workspace/chunked", archive[:split]), {Data: []byte{}}, {Data: archive[split:]}}
		if err := a.CopyIn(copyInChunks(context.Background(), chunks...)); err != nil {
			t.Fatalf("CopyIn() with empty data chunk error = %v", err)
		}
		got, err := os.ReadFile(filepath.Join(workspace, "chunked", "value"))
		if err != nil || string(got) != "ok" {
			t.Fatalf("file = %q, %v", got, err)
		}
	})
}

func TestCopyInRejectsUnsafeArchiveEntries(t *testing.T) {
	cases := []struct {
		name   string
		header *tar.Header
		setup  func(t *testing.T, destination string)
		want   codes.Code
	}{
		{name: "traversal", header: &tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0600, Size: 1, Linkname: "x"}, want: codes.InvalidArgument},
		{name: "absolute", header: &tar.Header{Name: "/tmp/outside", Typeflag: tar.TypeReg, Mode: 0600, Size: 1, Linkname: "x"}, want: codes.InvalidArgument},
		{name: "escaped symlink", header: &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}, want: codes.InvalidArgument},
		{name: "absolute symlink", header: &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, want: codes.InvalidArgument},
		{name: "escaping hard link", header: &tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../outside"}, want: codes.InvalidArgument},
		{name: "fifo", header: &tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0600}, want: codes.InvalidArgument},
		{name: "character device", header: &tar.Header{Name: "device", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, want: codes.InvalidArgument},
		{
			name:   "intermediate escaping symlink",
			header: &tar.Header{Name: "link/file", Typeflag: tar.TypeReg, Mode: 0600, Size: 1, Linkname: "x"},
			setup: func(t *testing.T, destination string) {
				t.Helper()
				if err := os.MkdirAll(destination, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(destination, "link")); err != nil {
					t.Fatal(err)
				}
			},
			want: codes.InvalidArgument,
		},
		{
			name:   "file directory conflict",
			header: &tar.Header{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0600, Size: 1, Linkname: "x"},
			setup: func(t *testing.T, destination string) {
				t.Helper()
				if err := os.MkdirAll(destination, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(destination, "dir"), []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: codes.InvalidArgument,
		},
		{
			name:   "directory file conflict",
			header: &tar.Header{Name: "file/", Typeflag: tar.TypeDir, Mode: 0755},
			setup: func(t *testing.T, destination string) {
				t.Helper()
				if err := os.MkdirAll(destination, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(destination, "file"), []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: codes.InvalidArgument,
		},
		{
			name: "escaping destination symlink",
			setup: func(t *testing.T, destination string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), destination); err != nil {
					t.Fatal(err)
				}
			},
			want: codes.InvalidArgument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, workspace := newTransferTestAgent(t, 1<<20, 20)
			if tc.setup != nil {
				tc.setup(t, filepath.Join(workspace, "dest"))
			}
			archive := makeTar(t, tc.header)
			err := a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/dest", archive)))
			requireCode(t, err, tc.want)
		})
	}
}

func TestCopyInRejectsMalformedAndOversizedArchives(t *testing.T) {
	t.Run("one zero block truncated trailer", func(t *testing.T) {
		a, _ := newTransferTestAgent(t, 1<<20, 20)
		archive := makeTar(t, &tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0600, Size: 1, Linkname: "x"})
		archive = archive[:len(archive)-512]
		err := a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/dest", archive)))
		requireCode(t, err, codes.InvalidArgument)
	})
	t.Run("missing first metadata", func(t *testing.T) {
		a, _ := newTransferTestAgent(t, 1<<20, 20)
		requireCode(t, a.CopyIn(copyInChunks(context.Background(), &agentv1.CopyInChunk{Data: makeTar(t)})), codes.InvalidArgument)
	})
	t.Run("later empty metadata presence", func(t *testing.T) {
		a, _ := newTransferTestAgent(t, 1<<20, 20)
		empty := ""
		stream := copyInChunks(context.Background(), firstChunk("/workspace/dest", nil), &agentv1.CopyInChunk{DestPath: &empty})
		requireCode(t, a.CopyIn(stream), codes.InvalidArgument)
	})
	t.Run("nonzero data after complete trailer", func(t *testing.T) {
		a, _ := newTransferTestAgent(t, 1<<20, 20)
		archive := append(makeTar(t), []byte("not padding")...)
		err := a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/dest", archive)))
		requireCode(t, err, codes.InvalidArgument)
	})
	t.Run("framing byte limit", func(t *testing.T) {
		a, _ := newTransferTestAgent(t, 512, 20)
		archive := makeTar(t)
		requireCode(t, a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/dest", archive))), codes.ResourceExhausted)
	})
	t.Run("chunk size limit", func(t *testing.T) {
		a, _ := newTransferTestAgent(t, 2<<20, 20)
		err := a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/dest", make([]byte, maxTransferChunkSize+1))))
		requireCode(t, err, codes.ResourceExhausted)
	})
	t.Run("NUL in RPC path", func(t *testing.T) {
		a, _ := newTransferTestAgent(t, 1<<20, 20)
		err := a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/bad\x00name", makeTar(t))))
		requireCode(t, err, codes.InvalidArgument)
	})
}

func TestCopyInConcurrentDestinationSymlinkReplacementCannotEscape(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 1<<20, 20)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "marker")
	if err := os.WriteFile(outsideFile, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(workspace, "dest")
	if err := os.MkdirAll(filepath.Join(dest, "swap"), 0755); err != nil {
		t.Fatal(err)
	}
	archive := makeTar(t, &tar.Header{Name: "swap/marker", Typeflag: tar.TypeReg, Mode: 0600, Size: 7, Linkname: "changed"})
	done := make(chan struct{})
	var replace sync.WaitGroup
	replace.Add(1)
	go func() {
		defer replace.Done()
		for i := range 500 {
			select {
			case <-done:
				return
			default:
			}
			link := filepath.Join(dest, "swap")
			_ = os.RemoveAll(link)
			if i%2 == 0 {
				_ = os.Symlink(outside, link)
			} else {
				_ = os.Mkdir(link, 0755)
			}
		}
	}()
	for range 20 {
		_ = a.CopyIn(copyInChunks(context.Background(), firstChunk("/workspace/dest", archive)))
	}
	close(done)
	replace.Wait()
	got, err := os.ReadFile(outsideFile)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("outside file was modified: %q, %v", got, err)
	}
}

func TestCopyOutPreservesFileAndLinkMetadata(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 1<<20, 20)
	tree := filepath.Join(workspace, "tree")
	if err := os.Mkdir(tree, 0755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 0)
	payload := []byte{0, 0xff, 0x80, 1, 2, 3}
	file := filepath.Join(tree, "binary")
	if err := os.WriteFile(file, payload, 0741); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0741); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("binary", filepath.Join(tree, "relative")); err != nil {
		t.Fatal(err)
	}
	stream := &copyOutTestStream{ctx: context.Background()}
	if err := a.CopyOut(&agentv1.CopyOutRequest{SrcPath: "/workspace/tree"}, stream); err != nil {
		t.Fatalf("CopyOut() error = %v", err)
	}
	var encoded bytes.Buffer
	for _, chunk := range stream.chunks {
		encoded.Write(chunk)
	}
	reader := tar.NewReader(&encoded)
	if _, err := reader.Next(); err != nil {
		t.Fatalf("directory header: %v", err)
	}
	header, err := reader.Next()
	if err != nil {
		t.Fatalf("file header: %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("file contents = %v, %v", got, err)
	}
	if header.Name != "binary" || header.Mode != 0741 || !header.ModTime.Equal(stamp) {
		t.Fatalf("file header = %#v", header)
	}
	link, err := reader.Next()
	if err != nil || link.Name != "relative" || link.Typeflag != tar.TypeSymlink || link.Linkname != "binary" {
		t.Fatalf("symlink header = %#v, %v", link, err)
	}
}

func TestCopyOutEmptyDirectoryAndRejectsEscapedLink(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 1<<20, 20)
	if err := os.Mkdir(filepath.Join(workspace, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	stream := &copyOutTestStream{ctx: context.Background()}
	if err := a.CopyOut(&agentv1.CopyOutRequest{SrcPath: "/workspace/empty"}, stream); err != nil {
		t.Fatalf("CopyOut(empty directory) error = %v", err)
	}
	var archiveBytes bytes.Buffer
	for _, chunk := range stream.chunks {
		archiveBytes.Write(chunk)
	}
	reader := tar.NewReader(&archiveBytes)
	if header, err := reader.Next(); err != nil || header.Name != "." || header.Typeflag != tar.TypeDir {
		t.Fatalf("empty directory archive header = %v, %v", header, err)
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("empty directory archive end = %v", err)
	}
	if err := os.Symlink("../../outside", filepath.Join(workspace, "escaped")); err != nil {
		t.Fatal(err)
	}
	err := a.CopyOut(&agentv1.CopyOutRequest{SrcPath: "/workspace/escaped"}, &copyOutTestStream{ctx: context.Background()})
	requireCode(t, err, codes.InvalidArgument)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "intermediate")); err != nil {
		t.Fatal(err)
	}
	err = a.CopyOut(&agentv1.CopyOutRequest{SrcPath: "/workspace/intermediate/secret"}, &copyOutTestStream{ctx: context.Background()})
	requireCode(t, err, codes.InvalidArgument)

}

func TestCopyOutWalkStaysInsidePinnedDirectoryAfterReplacement(t *testing.T) {
	a, workspace := newTransferTestAgent(t, 1<<20, 100)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-only"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(workspace, "source")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a-large"), bytes.Repeat([]byte{'a'}, 64<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "z-original"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	stream := &copyOutTestStream{
		ctx: context.Background(),
		onFirstSend: func() {
			if err := os.Rename(source, filepath.Join(workspace, "moved")); err != nil {
				t.Errorf("rename source during CopyOut: %v", err)
				return
			}
			if err := os.Symlink(outside, source); err != nil {
				t.Errorf("replace source with symlink: %v", err)
			}
		},
	}
	if err := a.CopyOut(&agentv1.CopyOutRequest{SrcPath: "/workspace/source"}, stream); err != nil {
		t.Fatalf("CopyOut() error = %v", err)
	}
	var encoded bytes.Buffer
	for _, chunk := range stream.chunks {
		encoded.Write(chunk)
	}
	reader := tar.NewReader(&encoded)
	seen := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading CopyOut tar: %v", err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading %q: %v", header.Name, err)
		}
		seen[header.Name] = string(data)
	}
	if seen["z-original"] != "original" || seen["secret"] != "" {
		t.Fatalf("archive followed replaced directory: entries = %v", seen)
	}
}

func TestCopyInCancellationStopsBeforeSuccess(t *testing.T) {
	a, _ := newTransferTestAgent(t, 1<<20, 20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.CopyIn(copyInChunks(ctx, firstChunk("/workspace/dest", makeTar(t))))
	requireCode(t, err, codes.Canceled)

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	archive := makeTar(t, &tar.Header{Name: "large", Typeflag: tar.TypeReg, Mode: 0600, Size: 4096, Linkname: strings.Repeat("x", 4096)})
	stream := copyInChunks(ctx, firstChunk("/workspace/cancelled", archive[:700]))
	stream.onEnd = func() error {
		cancel()
		return ctx.Err()
	}
	requireCode(t, a.CopyIn(stream), codes.Canceled)
}
