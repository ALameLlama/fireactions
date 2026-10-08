package agent

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ALameLlama/fireactions/internal/guestfs"
	agentv1 "github.com/ALameLlama/fireactions/proto/agent/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxTransferChunkSize = 1 << 20
	transferBufferSize   = 32 << 10
)

// CopyIn incrementally validates and extracts one tar archive beneath the
// requested workspace destination.
func (a *Agent) CopyIn(stream agentv1.AgentService_CopyInServer) error {
	if !a.isReady() {
		return status.Error(codes.FailedPrecondition, "guest is not ready")
	}
	maxBytes, maxEntries := a.transferLimits()
	input, err := newCopyInReader(stream, maxBytes)
	if err != nil {
		return transferError(err)
	}
	destination, err := a.workspacePath(input.destination)
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid workspace destination")
	}
	identity := a.currentIdentity()
	if err := a.fs.EnsureDirectory(destination, identity.UID, identity.GID, 0755); err != nil {
		return filesystemError(err)
	}
	destRoot, err := a.fs.OpenRoot(destination)
	if err != nil {
		return filesystemError(err)
	}
	defer destRoot.Close()

	tracked := &blockTracker{source: input, blockIsZero: true}
	archive := tar.NewReader(tracked)
	fileBuffer := make([]byte, transferBufferSize)
	var extractedBytes int64
	metadata := make(map[string]directoryMetadata)
	links := make(map[string]string)
	linkMetadata := make(map[string]time.Time)
	uploaded, err := newUploadedFiles(destRoot)
	if err != nil {
		return filesystemError(err)
	}
	defer uploaded.close()
	entries := 0
	for {
		if err := stream.Context().Err(); err != nil {
			return transferError(err)
		}
		tracked.readBytes = 0
		header, nextErr := archive.Next()
		if nextErr == io.EOF {
			if tracked.totalBytes != 0 && (tracked.readBytes < 1024 || tracked.trailingZeroBlocks < 2 || tracked.blockOffset != 0) {
				return status.Error(codes.InvalidArgument, "invalid or truncated tar archive")
			}
			if err := validateArchiveTrailer(tracked, input); err != nil {
				return transferError(err)
			}
			break
		}
		if nextErr != nil {
			if input.failure != nil {
				return transferError(input.failure)
			}
			return status.Error(codes.InvalidArgument, "invalid or truncated tar archive")
		}
		entries++
		if entries > maxEntries {
			return status.Error(codes.ResourceExhausted, "archive entry limit exceeded")
		}
		name, cleanErr := guestfs.CleanArchiveName(header.Name)
		if cleanErr != nil {
			return status.Error(codes.InvalidArgument, "unsafe archive entry path")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return status.Error(codes.InvalidArgument, "directory archive entry has data")
			}
			if err := ensureArchiveDirectory(destRoot, name, identity); err != nil {
				return filesystemError(err)
			}
			metadata[name] = directoryMetadata{mode: archiveMode(header.Mode), mtime: header.ModTime}
		case tar.TypeReg, tar.TypeRegA:
			if name == "." || header.Size < 0 {
				return status.Error(codes.InvalidArgument, "invalid regular file entry")
			}
			if header.Size > maxBytes-extractedBytes {
				return status.Error(codes.ResourceExhausted, "extracted file size limit exceeded")
			}
			if err := extractRegularFile(destRoot, name, archive, header, identity, stream.Context(), fileBuffer, uploaded); err != nil {
				return transferError(err)
			}
			extractedBytes += header.Size
		case tar.TypeSymlink:
			if header.Size != 0 || name == "." {
				return status.Error(codes.InvalidArgument, "invalid symbolic link entry")
			}
			// Install links only after the final tree has been validated, so a
			// later pivot cannot change the meaning of an earlier target.
			if header.Linkname == "" || strings.IndexByte(header.Linkname, 0) >= 0 || path.IsAbs(header.Linkname) {
				return status.Error(codes.InvalidArgument, "unsafe symbolic link target")
			}
			if err := ensureParent(destRoot, name, identity); err != nil {
				return filesystemError(err)
			}
			links[name] = header.Linkname
			linkMetadata[name] = header.ModTime
		case tar.TypeLink:
			if header.Size != 0 || name == "." {
				return status.Error(codes.InvalidArgument, "invalid hard link entry")
			}
			target, err := guestfs.CleanArchiveName(header.Linkname)
			if err != nil || target == "." {
				return status.Error(codes.InvalidArgument, "unsafe hard link target")
			}
			if err := extractHardlink(destRoot, target, name, header, identity, uploaded); err != nil {
				return filesystemError(err)
			}
		default:
			return status.Error(codes.InvalidArgument, "unsupported tar entry type")
		}
	}
	if err := uploaded.close(); err != nil {
		return filesystemError(err)
	}
	if err := destRoot.ValidateLinkTargets(links); err != nil {
		return status.Error(codes.InvalidArgument, "unsafe symbolic link target")
	}
	for name, target := range links {
		if err := stream.Context().Err(); err != nil {
			return transferError(err)
		}
		if err := extractSymlink(destRoot, name, target, linkMetadata[name], identity); err != nil {
			return filesystemError(err)
		}
	}

	if err := applyDirectoryMetadata(destRoot, metadata, identity); err != nil {
		return filesystemError(err)
	}
	if err := stream.Context().Err(); err != nil {
		return transferError(err)
	}
	if err := stream.SendAndClose(&agentv1.CopyInResponse{}); err != nil {
		return transferError(err)
	}
	return nil
}

// CopyOut streams a safe tar representation of a workspace file or directory.
func (a *Agent) CopyOut(req *agentv1.CopyOutRequest, stream agentv1.AgentService_CopyOutServer) error {
	if !a.isReady() {
		return status.Error(codes.FailedPrecondition, "guest is not ready")
	}
	source, err := a.workspacePath(req.GetSrcPath())
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid workspace source")
	}
	info, err := a.fs.Lstat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status.Error(codes.NotFound, "workspace source does not exist")
		}
		return filesystemError(err)
	}
	// Pin the transfer root before taking the archive snapshot. The snapshot's
	// link graph is validated before any header reaches the streaming consumer.
	transferRoot := a.fs
	if info.IsDir() || source == "." {
		transferRoot, err = a.fs.OpenRoot(source)
		if err != nil {
			return filesystemError(err)
		}
		defer transferRoot.Close()
		source = "."
	}
	maxBytes, maxEntries := a.transferLimits()
	output := &copyOutWriter{ctx: stream.Context(), stream: stream, maxBytes: maxBytes, buffer: make([]byte, transferBufferSize)}
	archive := tar.NewWriter(output)
	state := &copyOutState{
		ctx:        stream.Context(),
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
		buffer:     make([]byte, transferBufferSize),
		snapshot:   make(map[string]archiveSnapshot),
		links:      make(map[string]string),
	}
	archiveName := path.Base(source)
	if info.IsDir() || source == "." {
		archiveName = "."
	}
	if err := captureArchiveEntry(transferRoot, source, archiveName, info, state); err != nil {
		return transferError(err)
	}
	if err := guestfs.ValidateLinkTargets(state.links, func(name string) (fs.FileMode, string, error) {
		entry, ok := state.snapshot[name]
		if !ok {
			return 0, "", os.ErrNotExist
		}
		return entry.info.Mode(), entry.target, nil
	}); err != nil {
		return status.Error(codes.InvalidArgument, "workspace symbolic link escapes the transfer root")
	}
	if err := writeArchiveEntry(transferRoot, archive, source, archiveName, info, state); err != nil {
		return transferError(err)
	}
	if err := archive.Close(); err != nil {
		return transferError(err)
	}
	if err := output.Flush(); err != nil {
		return transferError(err)
	}
	return nil
}

type copyInReader struct {
	stream      agentv1.AgentService_CopyInServer
	destination string
	maxBytes    int64
	framing     int64
	data        []byte
	offset      int
	failure     error
}

func newCopyInReader(stream agentv1.AgentService_CopyInServer, maxBytes int64) (*copyInReader, error) {
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil, status.Error(codes.InvalidArgument, "copy-in stream is empty")
		}
		return nil, err
	}
	if first.DestPath == nil || *first.DestPath == "" {
		return nil, status.Error(codes.InvalidArgument, "first copy-in chunk must include a destination")
	}
	reader := &copyInReader{
		stream:      stream,
		destination: *first.DestPath,
		maxBytes:    maxBytes,
	}
	if err := reader.acceptData(first.Data); err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *copyInReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	for r.offset == len(r.data) {
		if err := r.stream.Context().Err(); err != nil {
			r.failure = err
			return 0, err
		}
		chunk, err := r.stream.Recv()
		if err != nil {
			if err == io.EOF {
				return 0, io.EOF
			}
			r.failure = err
			return 0, err
		}
		if chunk.DestPath != nil {
			r.failure = status.Error(codes.InvalidArgument, "destination is only allowed in the first copy-in chunk")
			return 0, r.failure
		}
		if err := r.acceptData(chunk.Data); err != nil {
			r.failure = err
			return 0, err
		}
	}
	n := copy(buffer, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *copyInReader) acceptData(data []byte) error {
	if len(data) > maxTransferChunkSize {
		return status.Error(codes.ResourceExhausted, "copy-in chunk exceeds 1 MiB")
	}
	if int64(len(data)) > r.maxBytes-r.framing {
		return status.Error(codes.ResourceExhausted, "archive transfer size limit exceeded")
	}
	r.framing += int64(len(data))
	r.data = data
	r.offset = 0
	return nil
}

type blockTracker struct {
	source             io.Reader
	blockOffset        int
	blockIsZero        bool
	trailingZeroBlocks int
	totalBytes         int64
	readBytes          int64
}

func (r *blockTracker) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	r.totalBytes += int64(n)
	r.readBytes += int64(n)
	for _, value := range buffer[:n] {
		if value != 0 {
			r.blockIsZero = false
		}
		r.blockOffset++
		if r.blockOffset == 512 {
			if r.blockIsZero {
				r.trailingZeroBlocks++
			} else {
				r.trailingZeroBlocks = 0
			}
			r.blockOffset = 0
			r.blockIsZero = true
		}
	}
	return n, err
}

func validateArchiveTrailer(tracked *blockTracker, input *copyInReader) error {
	buffer := make([]byte, transferBufferSize)
	for {
		n, err := tracked.Read(buffer)
		if n > 0 && !allZero(buffer[:n]) {
			return status.Error(codes.InvalidArgument, "nonzero data follows tar end marker")
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if input.failure != nil {
				return input.failure
			}
			return err
		}
	}
}

type directoryMetadata struct {
	mode  fs.FileMode
	mtime time.Time
}

func ensureArchiveDirectory(root *guestfs.RootFS, name string, identity Identity) error {
	if name == "." {
		return root.EnsureDirectory(".", identity.UID, identity.GID, 0755)
	}
	if err := root.EnsureDirectory(name, identity.UID, identity.GID, 0755); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return guestfs.ErrInvalidPath
	}
	return nil
}

func ensureParent(root *guestfs.RootFS, name string, identity Identity) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	return root.EnsureDirectory(parent, identity.UID, identity.GID, 0755)
}

// Private hard links keep created inodes alive without retaining a descriptor
// per entry. Recorded inode numbers cannot then be reused after workspace swaps.
type uploadedFiles struct {
	names     map[string]fs.FileInfo
	anchors   []string
	root      *guestfs.RootFS
	directory *os.File
	parent    *guestfs.RootFS
	name      string
}

func newUploadedFiles(root *guestfs.RootFS) (*uploadedFiles, error) {
	for range 8 {
		name, err := randomTemporaryName(".")
		if err != nil {
			return nil, err
		}
		if err := root.Root().Mkdir(name, 0700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		anchorRoot, err := root.OpenRoot(name)
		if err != nil {
			_ = root.Remove(name)
			return nil, err
		}
		directory, err := anchorRoot.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = anchorRoot.Close()
			_ = root.Remove(name)
			return nil, err
		}
		return &uploadedFiles{names: make(map[string]fs.FileInfo), root: anchorRoot, directory: directory, parent: root, name: name}, nil
	}
	return nil, fmt.Errorf("could not create an upload anchor directory")
}

func (u *uploadedFiles) pin(file *os.File) error {
	name := fmt.Sprintf("%d", len(u.anchors))
	// Linking the descriptor, not its mutable workspace name, pins the exact
	// inode that was created. /proc/self/fd does not require AT_EMPTY_PATH caps.
	if err := unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", file.Fd()), int(u.directory.Fd()), name, unix.AT_SYMLINK_FOLLOW); err != nil {
		return err
	}
	u.anchors = append(u.anchors, name)
	return nil
}

func (u *uploadedFiles) close() error {
	if u.root == nil {
		return nil
	}
	var err error
	for _, name := range u.anchors {
		err = errors.Join(err, u.root.Remove(name))
	}
	err = errors.Join(err, u.directory.Close(), u.root.Close(), u.parent.Remove(u.name))
	u.root = nil
	return err
}

func extractRegularFile(root *guestfs.RootFS, name string, archive *tar.Reader, header *tar.Header, identity Identity, ctx context.Context, buffer []byte, uploaded *uploadedFiles) error {
	if err := ensureParent(root, name, identity); err != nil {
		return err
	}
	parent, base, err := root.OpenParent(name)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := replaceableRegular(parent, base); err != nil {
		return err
	}
	for range 8 {
		temporary, err := randomTemporaryName(".")
		if err != nil {
			return err
		}
		file, err := parent.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		keep := false
		func() {
			defer func() {
				_ = file.Close()
				if !keep {
					_ = parent.Remove(temporary)
				}
			}()
			if err = parent.CheckRegular(file); err != nil {
				return
			}
			if err = copyTarFile(ctx, file, archive, header.Size, buffer); err != nil {
				return
			}
			if err = file.Chown(int(identity.UID), int(identity.GID)); err != nil {
				return
			}
			if err = file.Chmod(archiveMode(header.Mode)); err != nil {
				return
			}
			if err = setFileMtime(file, header.ModTime); err != nil {
				return
			}
			var info fs.FileInfo
			if info, err = file.Stat(); err != nil {
				return
			}
			if err = uploaded.pin(file); err != nil {
				return
			}
			if err = parent.Rename(temporary, base); err != nil {
				return
			}
			uploaded.names[name] = info
			keep = true
		}()
		if err != nil {
			return err
		}
		if !keep {
			return fmt.Errorf("temporary file was not committed")
		}
		return nil
	}
	return fmt.Errorf("could not create a temporary file")
}

func copyTarFile(ctx context.Context, file *os.File, archive *tar.Reader, size int64, buffer []byte) error {
	written := int64(0)
	for written < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := int64(len(buffer))
		if size-written < want {
			want = size - written
		}
		n, err := io.ReadFull(archive, buffer[:int(want)])
		if n > 0 {
			if _, writeErr := file.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
			written += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return status.Error(codes.InvalidArgument, "truncated regular file data")
			}
			return err
		}
	}
	return nil
}

func replaceableRegular(root *guestfs.RootFS, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return guestfs.ErrInvalidPath
	}
	return nil
}

func extractSymlink(root *guestfs.RootFS, name, target string, mtime time.Time, identity Identity) error {
	if err := ensureParent(root, name, identity); err != nil {
		return err
	}
	parent, base, err := root.OpenParent(name)
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Lstat(base)
	if err == nil {
		if info.Mode()&fs.ModeSymlink == 0 {
			return guestfs.ErrInvalidPath
		}
		// Replacing a symlink does not follow its old target. The new target
		// has already been checked against the final transfer graph.
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for range 8 {
		temporary, err := randomTemporaryName(".")
		if err != nil {
			return err
		}
		if err = parent.Symlink(target, temporary); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return err
		}
		link, err := parent.OpenFile(temporary, unix.O_PATH|unix.O_NOFOLLOW, 0)
		if err == nil {
			var info fs.FileInfo
			info, err = link.Stat()
			if err == nil && info.Mode()&fs.ModeSymlink == 0 {
				err = guestfs.ErrUnsupportedObject
			}
			if err == nil {
				err = unix.Fchownat(int(link.Fd()), "", int(identity.UID), int(identity.GID), unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
			}
			if err == nil {
				err = setFileMtime(link, mtime)
			}
			_ = link.Close()
		}
		if err != nil {
			_ = parent.Remove(temporary)
			return err
		}
		if err := parent.Rename(temporary, base); err != nil {
			_ = parent.Remove(temporary)
			return err
		}
		return nil
	}
	return fmt.Errorf("could not create a temporary symbolic link")
}

func extractHardlink(root *guestfs.RootFS, source, name string, header *tar.Header, identity Identity, uploaded *uploadedFiles) error {
	expected, ok := uploaded.names[source]
	if !ok {
		return status.Error(codes.InvalidArgument, "hard link target was not created by this upload")
	}
	if err := ensureParent(root, name, identity); err != nil {
		return err
	}
	sourceParent, sourceBase, err := root.OpenParent(source)
	if errors.Is(err, os.ErrNotExist) {
		return status.Error(codes.InvalidArgument, "hard link target does not exist")
	}
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	sourceFile, err := sourceParent.OpenFile(sourceBase, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return status.Error(codes.InvalidArgument, "hard link target does not exist")
	}
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	if err := sourceParent.CheckRegular(sourceFile); err != nil {
		return err
	}
	sourceInfo, err := sourceFile.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(expected, sourceInfo) {
		return status.Error(codes.InvalidArgument, "hard link target changed during upload")
	}
	destinationParent, destinationBase, err := root.OpenParent(name)
	if err != nil {
		return err
	}
	defer destinationParent.Close()
	if err := replaceableRegular(destinationParent, destinationBase); err != nil {
		return err
	}
	sourceDirectory, err := sourceParent.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer sourceDirectory.Close()
	destinationDirectory, err := destinationParent.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer destinationDirectory.Close()
	if err := sourceParent.CheckDirectory(sourceDirectory); err != nil {
		return err
	}
	if err := destinationParent.CheckDirectory(destinationDirectory); err != nil {
		return err
	}
	for range 8 {
		temporary, err := randomTemporaryName(".")
		if err != nil {
			return err
		}
		err = unix.Linkat(int(sourceDirectory.Fd()), sourceBase, int(destinationDirectory.Fd()), temporary, 0)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return err
		}
		linked, err := destinationParent.OpenFile(temporary, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
		if err == nil {
			err = destinationParent.CheckRegular(linked)
			if err == nil {
				var linkedInfo fs.FileInfo
				linkedInfo, err = linked.Stat()
				if err == nil && !os.SameFile(sourceInfo, linkedInfo) {
					err = status.Error(codes.InvalidArgument, "hard link target changed during upload")
				}
			}
			if err == nil {
				err = linked.Chown(int(identity.UID), int(identity.GID))
			}
			if err == nil {
				err = linked.Chmod(archiveMode(header.Mode))
			}
			if err == nil {
				err = setFileMtime(linked, header.ModTime)
			}
			_ = linked.Close()
		}
		if err != nil {
			_ = destinationParent.Remove(temporary)
			return err
		}
		if err := destinationParent.Rename(temporary, destinationBase); err != nil {
			_ = destinationParent.Remove(temporary)
			return err
		}
		// rename is a no-op when both names already refer to the same inode.
		if err := destinationParent.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		uploaded.names[name] = sourceInfo
		return nil
	}
	return fmt.Errorf("could not create a temporary hard link")
}

func applyDirectoryMetadata(root *guestfs.RootFS, all map[string]directoryMetadata, identity Identity) error {
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.SliceStable(names, func(i, j int) bool {
		return strings.Count(names[i], "/") > strings.Count(names[j], "/")
	})
	for _, name := range names {
		metadata := all[name]
		directory, err := root.OpenRoot(name)
		if err != nil {
			return err
		}
		file, err := directory.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = directory.Close()
			return err
		}
		err = directory.CheckDirectory(file)
		if err == nil {
			err = file.Chown(int(identity.UID), int(identity.GID))
		}
		if err == nil {
			err = file.Chmod(metadata.mode)
		}
		if err == nil {
			err = setFileMtime(file, metadata.mtime)
		}
		_ = file.Close()
		_ = directory.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func setFileMtime(file *os.File, mtime time.Time) error {
	value := unix.Timespec{Sec: mtime.Unix(), Nsec: int64(mtime.Nanosecond())}
	return unix.UtimesNanoAt(int(file.Fd()), "", []unix.Timespec{value, value}, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
}

func archiveMode(mode int64) fs.FileMode { return fs.FileMode(mode) & 0777 }

func randomTemporaryName(parent string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	name := ".fireactions-transfer-" + hex.EncodeToString(random[:])
	if parent == "." {
		return name, nil
	}
	return path.Join(parent, name), nil
}

type copyOutState struct {
	ctx        context.Context
	fileBytes  int64
	maxBytes   int64
	entries    int
	maxEntries int
	buffer     []byte
	snapshot   map[string]archiveSnapshot
	links      map[string]string
}

type archiveSnapshot struct {
	info     fs.FileInfo
	target   string
	children []string
}

func openArchiveDirectory(root *guestfs.RootFS, source string, expected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	directory, err := root.OpenFile(source, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := root.CheckDirectory(directory); err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	current, err := directory.Stat()
	if err == nil && !os.SameFile(expected, current) {
		err = status.Error(codes.InvalidArgument, "workspace directory changed during archive")
	}
	if err != nil {
		_ = directory.Close()
		return nil, nil, err
	}
	return directory, current, nil
}

// Capture names and link targets before writing. Export always uses this exact
// graph, not targets reread from a workspace that can change while streaming.
func captureArchiveEntry(root *guestfs.RootFS, source, name string, info fs.FileInfo, state *copyOutState) error {
	if err := state.ctx.Err(); err != nil {
		return err
	}
	state.entries++
	if state.entries > state.maxEntries {
		return status.Error(codes.ResourceExhausted, "archive entry limit exceeded")
	}
	entry := archiveSnapshot{info: info}
	switch {
	case info.IsDir():
		// Finish listing and close its handle before descending. All recursive
		// paths stay relative to the original, pinned transfer root.
		if err := func() error {
			directory, current, err := openArchiveDirectory(root, source, info)
			if err != nil {
				return err
			}
			defer directory.Close()
			entry.info = current
			for {
				if err := state.ctx.Err(); err != nil {
					return err
				}
				batch, readErr := directory.ReadDir(128)
				for _, child := range batch {
					childName := child.Name()
					if childName == "." || childName == ".." || strings.ContainsAny(childName, "/\x00") {
						return status.Error(codes.InvalidArgument, "unsafe workspace entry name")
					}
					if len(entry.children) >= state.maxEntries-state.entries {
						return status.Error(codes.ResourceExhausted, "archive entry limit exceeded")
					}
					entry.children = append(entry.children, childName)
				}
				if readErr == io.EOF {
					return nil
				}
				if readErr != nil {
					return readErr
				}
			}
		}(); err != nil {
			return err
		}
		sort.Strings(entry.children)
		for _, child := range entry.children {
			childSource := path.Join(source, child)
			childInfo, err := root.Lstat(childSource)
			if err != nil {
				return err
			}
			if err := captureArchiveEntry(root, childSource, path.Join(name, child), childInfo, state); err != nil {
				return err
			}
		}
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := root.Readlink(source)
		if err != nil {
			return err
		}
		entry.target = target
		state.links[name] = target
	case !info.Mode().IsRegular():
		return status.Error(codes.InvalidArgument, "unsupported workspace filesystem object")
	}
	state.snapshot[name] = entry
	return nil
}

func writeArchiveEntry(root *guestfs.RootFS, archive *tar.Writer, source, archiveName string, info fs.FileInfo, state *copyOutState) error {
	if err := state.ctx.Err(); err != nil {
		return err
	}
	cleanName, err := guestfs.CleanArchiveName(archiveName)
	if err != nil {
		return status.Error(codes.InvalidArgument, "unsafe archive output path")
	}
	mode := info.Mode()
	switch {
	case mode.IsDir():
		directory, current, err := openArchiveDirectory(root, source, info)
		if err != nil {
			return err
		}
		if err := directory.Close(); err != nil {
			return err
		}
		header := &tar.Header{
			Name:     cleanName,
			Typeflag: tar.TypeDir,
			Mode:     int64(current.Mode().Perm()),
			ModTime:  current.ModTime(),
			Format:   tar.FormatPAX,
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		children := state.snapshot[cleanName].children
		for _, childName := range children {
			if err := state.ctx.Err(); err != nil {
				return err
			}
			childArchive := childName
			if cleanName != "." {
				childArchive = path.Join(cleanName, childName)
			}
			childInfo := state.snapshot[childArchive].info
			if err := writeArchiveEntry(root, archive, path.Join(source, childName), childArchive, childInfo, state); err != nil {
				return err
			}
		}
		return nil
	case mode&fs.ModeSymlink != 0:
		target := state.snapshot[cleanName].target
		return archive.WriteHeader(&tar.Header{Name: cleanName, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0777, ModTime: info.ModTime(), Format: tar.FormatPAX})
	case mode.IsRegular():
		parent, base, err := root.OpenParent(source)
		if err != nil {
			return err
		}
		if expected, ok := state.snapshot[path.Dir(cleanName)]; ok {
			directory, _, err := openArchiveDirectory(parent, ".", expected.info)
			if err != nil {
				_ = parent.Close()
				return err
			}
			_ = directory.Close()
		}
		file, err := parent.OpenFile(base, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = parent.Close()
			return err
		}
		if err := parent.CheckRegular(file); err != nil {
			_ = file.Close()
			_ = parent.Close()
			return err
		}
		openedInfo, err := file.Stat()
		if err != nil {
			_ = file.Close()
			_ = parent.Close()
			return err
		}
		if openedInfo.Size() < 0 || openedInfo.Size() > state.maxBytes-state.fileBytes {
			_ = file.Close()
			_ = parent.Close()
			return status.Error(codes.ResourceExhausted, "extracted file size limit exceeded")
		}
		header := &tar.Header{
			Name:     cleanName,
			Typeflag: tar.TypeReg,
			Mode:     int64(openedInfo.Mode().Perm()),
			Size:     openedInfo.Size(),
			ModTime:  openedInfo.ModTime(),
			Format:   tar.FormatPAX,
		}
		if err := archive.WriteHeader(header); err != nil {
			_ = file.Close()
			_ = parent.Close()
			return err
		}
		written, copyErr := io.CopyBuffer(archive, io.LimitReader(file, openedInfo.Size()), state.buffer)
		closeErr := file.Close()
		_ = parent.Close()
		state.fileBytes += written
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != openedInfo.Size() {
			return io.ErrUnexpectedEOF
		}
		return nil
	default:
		return status.Error(codes.InvalidArgument, "unsupported workspace filesystem object")
	}
}

type copyOutWriter struct {
	ctx      context.Context
	stream   agentv1.AgentService_CopyOutServer
	maxBytes int64
	total    int64
	buffer   []byte
	used     int
}

func (w *copyOutWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > w.maxBytes-w.total {
		return 0, status.Error(codes.ResourceExhausted, "archive transfer size limit exceeded")
	}
	written := 0
	for len(data) > 0 {
		count := copy(w.buffer[w.used:], data)
		w.used += count
		w.total += int64(count)
		written += count
		data = data[count:]
		if w.used == len(w.buffer) {
			if err := w.send(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *copyOutWriter) Flush() error {
	if w.used == 0 {
		return nil
	}
	return w.send()
}

func (w *copyOutWriter) send() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := w.stream.Send(&agentv1.CopyOutChunk{Data: w.buffer[:w.used]}); err != nil {
		return err
	}
	w.used = 0
	return nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func transferError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	var wrappedStatus interface{ GRPCStatus() *status.Status }
	if errors.As(err, &wrappedStatus) {
		return wrappedStatus.GRPCStatus().Err()
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "transfer cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "transfer deadline exceeded")
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) && pathError.Err.Error() == "path escapes from parent" {
		return status.Error(codes.InvalidArgument, "workspace path escapes its root")
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTDIR) {
		return status.Error(codes.InvalidArgument, "unsafe workspace path or filesystem object")
	}
	if errors.Is(err, guestfs.ErrInvalidPath) || errors.Is(err, guestfs.ErrUnsupportedObject) {
		return status.Error(codes.InvalidArgument, "unsafe workspace path or filesystem object")
	}
	if errors.Is(err, guestfs.ErrUnsafeFilesystem) {
		return status.Error(codes.InvalidArgument, "workspace path crosses an unsupported filesystem")
	}
	if errors.Is(err, os.ErrNotExist) {
		return status.Error(codes.NotFound, "workspace file does not exist")
	}
	return status.Error(codes.Internal, "guest file transfer failed")
}

func filesystemError(err error) error { return transferError(err) }
