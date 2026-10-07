// Package guestfs provides filesystem operations rooted at a guest workspace.
package guestfs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	ErrInvalidPath       = errors.New("invalid workspace path")
	ErrUnsafeFilesystem  = errors.New("workspace path crosses an unsafe filesystem boundary")
	ErrUnsupportedObject = errors.New("unsupported filesystem object")
)

// RootFS holds an open root directory. Operations remain confined to this
// directory even if a path component is concurrently replaced by a symlink.
type RootFS struct {
	root      *os.Root
	rootDev   uint64
	rootMntID uint64
}

// OpenRoot opens a workspace root once. The path is a guest-local host path,
// not an RPC path.
func OpenRoot(name string) (*RootFS, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return nil, ErrInvalidPath
	}
	if err := os.MkdirAll(name, 0755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	rootFS := &RootFS{root: root}
	file, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	defer file.Close()
	if err := rootFS.inspectDescriptor(file, true); err != nil {
		_ = root.Close()
		return nil, err
	}
	return rootFS, nil
}

// Root returns the rooted filesystem handle for safe rooted operations.
func (r *RootFS) Root() *os.Root { return r.root }

func (r *RootFS) Close() error {
	if r == nil || r.root == nil {
		return nil
	}
	return r.root.Close()
}

// ResolvePath maps a guest RPC path into the configured /workspace root.
// Absolute paths must begin with /workspace; relative paths are workspace-local.
func (r *RootFS) ResolvePath(name string) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return "", ErrInvalidPath
	}
	if path.IsAbs(name) {
		if name != "/workspace" && !strings.HasPrefix(name, "/workspace/") {
			return "", ErrInvalidPath
		}
		name = strings.TrimLeft(strings.TrimPrefix(name, "/workspace"), "/")
		if name == "" {
			return ".", nil
		}
	}
	return cleanRelative(name, true)
}

func cleanRelative(name string, allowDot bool) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 || path.IsAbs(name) {
		return "", ErrInvalidPath
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", ErrInvalidPath
		}
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") || (!allowDot && clean == ".") {
		return "", ErrInvalidPath
	}
	return clean, nil
}

func (r *RootFS) normalize(name string, allowDot bool) (string, error) {
	return cleanRelative(name, allowDot)
}

func (r *RootFS) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	clean, err := r.normalize(name, true)
	if err != nil {
		return nil, err
	}
	return r.root.OpenFile(clean, flag, perm)
}

func (r *RootFS) Open(name string) (*os.File, error) {
	return r.OpenFile(name, os.O_RDONLY, 0)
}

func (r *RootFS) OpenRoot(name string) (*RootFS, error) {
	clean, err := r.normalize(name, true)
	if err != nil {
		return nil, err
	}
	root, err := r.root.OpenRoot(clean)
	if err != nil {
		return nil, err
	}
	child := &RootFS{root: root, rootDev: r.rootDev, rootMntID: r.rootMntID}
	file, err := root.Open(".")
	if err == nil {
		err = child.CheckDirectory(file)
		_ = file.Close()
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return child, nil
}

func (r *RootFS) OpenParent(name string) (*RootFS, string, error) {
	clean, err := r.normalize(name, false)
	if err != nil {
		return nil, "", err
	}
	parent, err := r.OpenRoot(path.Dir(clean))
	if err != nil {
		return nil, "", err
	}
	return parent, path.Base(clean), nil
}

func (r *RootFS) EnsureDirectory(name string, uid, gid uint32, mode fs.FileMode) error {
	clean, err := r.normalize(name, true)
	if err != nil {
		return err
	}
	if clean == "." {
		return ownDirectory(r, ".", uid, gid)
	}

	current := r
	ownsCurrent := false
	defer func() {
		if ownsCurrent {
			_ = current.Close()
		}
	}()
	for _, component := range strings.Split(clean, "/") {
		info, err := current.root.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			if err := current.root.Mkdir(component, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = current.root.Lstat(component)
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return ErrInvalidPath
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			return err
		}
		openedInfo, err := next.root.Stat(".")
		if err == nil && !os.SameFile(info, openedInfo) {
			err = ErrInvalidPath
		}
		if err == nil {
			err = ownDirectory(next, ".", uid, gid)
		}
		if err != nil {
			_ = next.Close()
			return err
		}
		if ownsCurrent {
			_ = current.Close()
		}
		current = next
		ownsCurrent = true
	}
	return nil
}

func ownDirectory(r *RootFS, name string, uid, gid uint32) error {
	file, err := r.root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := r.CheckDirectory(file); err != nil {
		return err
	}
	return file.Chown(int(uid), int(gid))
}

func (r *RootFS) Lstat(name string) (fs.FileInfo, error) {
	clean, err := r.normalize(name, true)
	if err != nil {
		return nil, err
	}
	return r.root.Lstat(clean)
}

func (r *RootFS) Remove(name string) error {
	clean, err := r.normalize(name, false)
	if err != nil {
		return err
	}
	return r.root.Remove(clean)
}

func (r *RootFS) Rename(oldname, newname string) error {
	oldClean, err := r.normalize(oldname, false)
	if err != nil {
		return err
	}
	newClean, err := r.normalize(newname, false)
	if err != nil {
		return err
	}
	return r.root.Rename(oldClean, newClean)
}

func (r *RootFS) Symlink(target, newname string) error {
	if target == "" || strings.IndexByte(target, 0) >= 0 || path.IsAbs(target) {
		return ErrInvalidPath
	}
	newClean, err := r.normalize(newname, false)
	if err != nil {
		return err
	}
	return r.root.Symlink(target, newClean)
}

func (r *RootFS) Readlink(name string) (string, error) {
	clean, err := r.normalize(name, false)
	if err != nil {
		return "", err
	}
	return r.root.Readlink(clean)
}

// CleanArchiveName validates an archive member path. A single dot is the
// canonical name for the transfer root.
func CleanArchiveName(name string) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 || path.IsAbs(name) {
		return "", ErrInvalidPath
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", ErrInvalidPath
		}
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrInvalidPath
	}
	return clean, nil
}

// ValidateLinkTargets resolves each relative link component by component. In
// particular, ".." is applied after expanding any preceding symlink, rather
// than cleaning the target lexically. Lookup must describe the final rooted
// tree. Missing tails containing only names remain safely dangling, but ".."
// after a missing component cannot establish containment.
func ValidateLinkTargets(links map[string]string, lookup func(string) (fs.FileMode, string, error)) error {
	for entry, target := range links {
		clean, err := CleanArchiveName(entry)
		if err != nil || clean == "." || target == "" || strings.IndexByte(target, 0) >= 0 || path.IsAbs(target) {
			return ErrInvalidPath
		}
		components := strings.Split(path.Dir(clean)+"/"+target, "/")
		resolved := make([]string, 0, len(components))
		mode := fs.ModeDir
		followed := 0
		missing := false
		for len(components) != 0 {
			component := components[0]
			components = components[1:]
			if !mode.IsDir() {
				return ErrInvalidPath
			}
			switch component {
			case "", ".":
				continue
			case "..":
				if len(resolved) == 0 || missing {
					return ErrInvalidPath
				}
				resolved = resolved[:len(resolved)-1]
				continue
			}
			name := strings.Join(append(resolved, component), "/")
			if link, ok := links[name]; ok {
				mode = fs.ModeSymlink
				target = link
			} else {
				mode, link, err = lookup(name)
				if errors.Is(err, os.ErrNotExist) {
					missing = true
					mode = fs.ModeDir
				} else if err != nil {
					return err
				}
				target = link
			}
			if mode&fs.ModeSymlink != 0 {
				followed++
				if followed > 40 || target == "" || strings.IndexByte(target, 0) >= 0 || path.IsAbs(target) {
					return ErrInvalidPath
				}
				components = append(strings.Split(target, "/"), components...)
				mode = fs.ModeDir
				continue
			}
			resolved = append(resolved, component)
		}
	}
	return nil
}

// ValidateLinkTargets checks the final workspace tree with proposed links
// overlaid on it. All filesystem inspection remains beneath the open root.
func (r *RootFS) ValidateLinkTargets(links map[string]string) error {
	return ValidateLinkTargets(links, func(name string) (fs.FileMode, string, error) {
		info, err := r.Lstat(name)
		if err != nil {
			return 0, "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := r.Readlink(name)
			return info.Mode(), target, err
		}
		return info.Mode(), "", nil
	})
}

func (r *RootFS) CheckRegular(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsupportedObject
	}
	return r.inspectDescriptor(file, false)
}

func (r *RootFS) CheckDirectory(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsupportedObject
	}
	return r.inspectDescriptor(file, false)
}

func (r *RootFS) inspectDescriptor(file *os.File, root bool) error {
	fd := int(file.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return err
	}
	if unsafeFilesystem(uint64(filesystem.Type)) {
		return ErrUnsafeFilesystem
	}
	var mount unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &mount); err != nil {
		return err
	}
	if mount.Mask&unix.STATX_MNT_ID == 0 {
		return ErrUnsafeFilesystem
	}
	if root {
		r.rootDev = uint64(stat.Dev)
		r.rootMntID = mount.Mnt_id
		return nil
	}
	if uint64(stat.Dev) != r.rootDev || mount.Mnt_id != r.rootMntID {
		return ErrUnsafeFilesystem
	}
	return nil
}

func unsafeFilesystem(kind uint64) bool {
	switch kind {
	case 0x00009fa0, // proc
		0x62656572, // sysfs
		0x00001cd1, // devpts
		0x000027e0, // cgroup
		0x63677270, // cgroup2
		0x73636673, // securityfs
		0x64626720, // debugfs
		0x74726163, // tracefs
		0xcafe4a11, // bpf
		0x62656570: // configfs
		return true
	default:
		return false
	}
}
