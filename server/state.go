package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ALameLlama/fireactions/helper/stringid"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containernetworking/cni/libcni"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const instanceLabel = "fireactions.instance_id"
const vmLabel = "fireactions.vm_id"

// stateRecord is the versioned write-ahead authority for exactly one VM. No
// cleanup step consults the current pool/CNI configuration to reconstruct it.
type stateRecord struct {
	Version             int         `json:"version"`
	InstanceID          string      `json:"instance_id"`
	OwnerPID            int         `json:"owner_pid"`
	OwnerStartTime      string      `json:"owner_start_time"`
	VMID                string      `json:"vm_id"`
	Profile             string      `json:"profile"`
	SnapshotID          string      `json:"snapshot_id"`
	LeaseID             string      `json:"lease_id"`
	CNIConfig           []byte      `json:"cni_config"`
	CNIBinPaths         []string    `json:"cni_bin_paths"`
	CNICacheDir         string      `json:"cni_cache_dir"`
	CNIIfName           string      `json:"cni_if_name"`
	CNIArgs             [][2]string `json:"cni_args"`
	NetNSPath           string      `json:"netns_path"`
	VMMPID              int         `json:"vmm_pid"`
	VMMStartTime        string      `json:"vmm_start_time"`
	VMMBinary           string      `json:"vmm_binary"`
	VMMDevice           uint64      `json:"vmm_device"`
	VMMInode            uint64      `json:"vmm_inode"`
	CID                 uint32      `json:"cid"`
	APISocketPath       string      `json:"api_socket_path"`
	VsockPath           string      `json:"vsock_path"`
	State               string      `json:"state"`
	CreatedAt           time.Time   `json:"created_at"`
	ExpiresAt           *time.Time  `json:"expires_at"`
	ContainerdNamespace string      `json:"containerd_namespace"`
	Snapshotter         string      `json:"snapshotter"`
	ProvisioningActive  bool        `json:"provisioning_active"`
	// No CNI/containerd/VMM allocation may start until this marker barrier is durable.
	AllocationReady bool `json:"allocation_ready"`
}

// StateStore uses stable advisory locks for short journal metadata sections and
// serialized resource reclamation. Process isolation never waits for reclamation.
type StateStore struct {
	root           string
	dir            string
	instanceID     string
	ownerPID       int
	ownerStartTime string
	client         *containerd.Client
	cleanupGrace   time.Duration
	profiles       map[string]bool
	// Fault seams exercise real filesystem persistence and ordered retries.
	syncFile          func(*os.File) error
	syncDir           func(*os.File) error
	cleanup           func(context.Context, *stateRecord, bool) error
	removeResourceDir func(string) error
}

func NewStateStore(config *Config, client *containerd.Client) (*StateStore, error) {
	if config == nil || !filepath.IsAbs(config.StateDir) || filepath.Clean(config.StateDir) != config.StateDir {
		return nil, fmt.Errorf("state directory must be a clean absolute path")
	}
	if err := secureStateDirectory(config.StateDir); err != nil {
		return nil, err
	}
	dir := filepath.Join(config.StateDir, "journal")
	if err := secureStateDirectory(dir); err != nil {
		return nil, err
	}
	start, err := processStartTime(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("reading daemon process identity: %w", err)
	}
	s := &StateStore{root: config.StateDir, dir: dir, instanceID: stringid.New(), ownerPID: os.Getpid(), ownerStartTime: start, client: client, cleanupGrace: config.Leases.CleanupGrace}
	s.profiles = make(map[string]bool, len(config.Pools))
	for _, pool := range config.Pools {
		if pool != nil {
			s.profiles[pool.Name] = true
		}
	}
	s.syncFile = func(f *os.File) error { return f.Sync() }
	s.syncDir = func(f *os.File) error { return f.Sync() }
	s.cleanup = s.cleanupRecord
	return s, nil
}

func secureStateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return fmt.Errorf("state directory must be a clean absolute path below root")
	}
	// Create one component at a time so an untrusted path cannot redirect MkdirAll.
	current := "/"
	for _, part := range strings.Split(path, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err == nil {
				parent, err := os.Open(filepath.Dir(current))
				if err != nil {
					return err
				}
				err = parent.Sync()
				_ = parent.Close()
				if err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe state directory %s", current)
		}
		if current != path {
			if err := validateUnixSocketAncestor(current, info); err != nil {
				return fmt.Errorf("unsafe state ancestor: %w", err)
			}
		}
	}
	fd, err := unix.Open(path, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	err = unix.Fstat(fd, &stat)
	if err != nil {
		return err
	}
	if stat.Uid != 0 {
		return fmt.Errorf("state directory must be root-owned: %s", path)
	}
	if err := unix.Fchmod(fd, 0700); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func (s *StateStore) lock(vmID string) (*os.File, error) {
	return s.lockMetadata(context.Background(), context.Background(), vmID)
}

func (s *StateStore) lockMetadata(ctx, caller context.Context, vmID string) (*os.File, error) {
	if !profileNamePattern.MatchString(vmID) {
		return nil, fmt.Errorf("invalid journal VM ID")
	}
	// Never unlink this lock: waiters must always open the same inode. Legacy
	// per-VM .lock files can only be retired with every old daemon/reaper stopped;
	// flock cannot prove there are no old-version waiters on those inodes.
	return s.lockJournal(ctx, caller, ".metadata.lock")
}

// These lock files must never be unlinked: independently opened stores and
// processes must rendezvous on the same inode, regardless of VM churn.
func (s *StateStore) lockJournal(ctx, caller context.Context, name string) (*os.File, error) {
	fd, err := unix.Open(filepath.Join(s.dir, name), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	var ticker *time.Ticker
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		if ticker != nil {
			if err := caller.Err(); err != nil {
				f.Close()
				return nil, err
			}
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			f.Close()
			return nil, err
		}
		// An already-canceled caller may still isolate and clean up immediately,
		// but contention must not extend its wait or the cleanup grace.
		if ticker == nil {
			ticker = time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-caller.Done():
			err = caller.Err()
		case <-ticker.C:
			continue
		}
		f.Close()
		return nil, err
	}
}

func (s *StateStore) recordPath(vmID string) string { return filepath.Join(s.dir, vmID+".json") }

func (s *StateStore) writeRecord(r *stateRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".intent-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := s.syncFile(file); err != nil {
		return fmt.Errorf("sync journal record: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.recordPath(r.VMID)); err != nil {
		return err
	}
	return s.syncDirectory()
}

func (s *StateStore) syncDirectory() error {
	file, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := s.syncDir(file); err != nil {
		return fmt.Errorf("sync journal directory: %w", err)
	}
	return nil
}

func (s *StateStore) readRecord(vmID string) (*stateRecord, error) {
	fd, err := unix.Open(s.recordPath(vmID), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), vmID+".json")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, fmt.Errorf("invalid journal file")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	var record stateRecord
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("journal has trailing data")
	}
	if err := s.validateRecord(vmID, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *StateStore) validateRecord(vmID string, r *stateRecord) error {
	if r.Version != 1 || r.VMID != vmID || !profileNamePattern.MatchString(r.Profile) || !strings.HasPrefix(vmID, r.Profile+"-") || !profileNamePattern.MatchString(r.InstanceID) || r.OwnerPID <= 0 || r.OwnerStartTime == "" || r.CreatedAt.IsZero() {
		return fmt.Errorf("invalid journal ownership/version")
	}
	if _, err := strconv.ParseUint(r.OwnerStartTime, 10, 64); err != nil {
		return fmt.Errorf("invalid owner start time")
	}
	resourceDir := filepath.Join(s.root, "pools", r.Profile, vmID)
	if r.SnapshotID != vmID || r.LeaseID != "fireactions/pools/"+r.Profile+"/"+vmID || r.APISocketPath != filepath.Join(resourceDir, "api.sock") || r.VsockPath != filepath.Join(resourceDir, "vsock") || r.CNICacheDir != filepath.Join(resourceDir, "cni") || r.NetNSPath != filepath.Join("/var/run/netns", vmID) {
		return fmt.Errorf("journal resource paths do not match ownership")
	}
	if !filepath.IsAbs(r.VMMBinary) || filepath.Clean(r.VMMBinary) != r.VMMBinary || r.VMMInode == 0 || r.CID < 3 || r.CID == ^uint32(0) || r.ContainerdNamespace == "" || r.Snapshotter != defaultSnapshotter || r.CNIIfName != "eth0" || len(r.CNIBinPaths) == 0 {
		return fmt.Errorf("incomplete journal runtime identity")
	}
	for _, path := range r.CNIBinPaths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("invalid CNI binary path")
		}
	}
	if _, err := libcni.ConfListFromBytes(r.CNIConfig); err != nil {
		return fmt.Errorf("invalid saved CNI configuration: %w", err)
	}
	if r.VMMPID < 0 || (r.VMMPID == 0) != (r.VMMStartTime == "") {
		return fmt.Errorf("incomplete VMM process identity")
	}
	if r.VMMStartTime != "" {
		if _, err := strconv.ParseUint(r.VMMStartTime, 10, 64); err != nil {
			return fmt.Errorf("invalid VMM start time")
		}
	}
	switch r.State {
	case "idle":
		if r.ExpiresAt != nil || r.ProvisioningActive {
			return fmt.Errorf("invalid idle lifetime")
		}
	case "provisioning", "claimed":
		if r.ExpiresAt == nil || r.ExpiresAt.IsZero() {
			return fmt.Errorf("missing hard deadline")
		}
	case "removing":
	default:
		return fmt.Errorf("unknown journal state")
	}
	return nil
}

func (s *StateStore) create(record *stateRecord) error {
	lock, err := s.lock(record.VMID)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := os.Lstat(s.recordPath(record.VMID)); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("journal record already exists")
		}
		return err
	}
	if err := s.validateRecord(record.VMID, record); err != nil {
		return err
	}
	return s.writeRecord(record)
}

func (s *StateStore) update(vmID string, mutate func(*stateRecord) error) error {
	lock, err := s.lock(vmID)
	if err != nil {
		return err
	}
	defer lock.Close()
	record, err := s.readRecord(vmID)
	if err != nil {
		return err
	}
	if record.InstanceID != s.instanceID {
		return fmt.Errorf("journal instance ownership changed")
	}
	if err := mutate(record); err != nil {
		return err
	}
	if err := s.validateRecord(vmID, record); err != nil {
		return err
	}
	return s.writeRecord(record)
}

// State returns durable state; missing records return os.ErrNotExist. Callers
// must not treat corrupt/unreadable ownership as evidence that cleanup succeeded.
func (s *StateStore) State(vmID string) (string, error) {
	lock, err := s.lock(vmID)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	r, err := s.readRecord(vmID)
	if alreadyGone(err) {
		entries, readErr := os.ReadDir(s.dir)
		if readErr != nil {
			return "", readErr
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), vmID+".json.corrupt-") {
				return "", fmt.Errorf("VM ownership evidence is quarantined")
			}
		}
	}
	if err != nil {
		return "", err
	}
	return r.State, nil
}

func (s *StateStore) publish(vmID, state string, expiry *time.Time) error {
	return s.update(vmID, func(r *stateRecord) error {
		if r.State == "removing" {
			return fmt.Errorf("VM is already removing")
		}
		if r.ProvisioningActive {
			return fmt.Errorf("VM provisioning is still active")
		}
		if !r.AllocationReady {
			return fmt.Errorf("VM allocation ownership barrier is not durable")
		}
		if r.ExpiresAt != nil && !time.Now().Before(*r.ExpiresAt) {
			return fmt.Errorf("VM provisioning/lease deadline expired")
		}
		if expiry != nil && !time.Now().Before(*expiry) {
			return fmt.Errorf("VM claim deadline expired")
		}
		if state != "idle" && state != "claimed" {
			return fmt.Errorf("invalid published state")
		}
		if state == "idle" && r.State != "provisioning" {
			return fmt.Errorf("only a fresh provisioning VM can become idle")
		}
		if state == "claimed" && r.State != "idle" && r.State != "provisioning" {
			return fmt.Errorf("VM is already claimed")
		}
		r.State, r.ExpiresAt = state, expiry
		return nil
	})
}

func (s *StateStore) finishProvisioning(vmID string) error {
	return s.update(vmID, func(r *stateRecord) error { r.ProvisioningActive = false; return nil })
}

func (s *StateStore) checkProvisioning(vmID string) error {
	state, err := s.State(vmID)
	if err != nil {
		return err
	}
	if state != "provisioning" {
		return fmt.Errorf("VM provisioning was revoked")
	}
	return nil
}

func ownerAlive(r *stateRecord) (bool, error) {
	start, err := processStartTime(r.OwnerPID)
	if alreadyGone(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if start != r.OwnerStartTime {
		return false, nil
	}
	fd, err := unix.PidfdOpen(r.OwnerPID, 0)
	if alreadyGone(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	start, err = processStartTime(r.OwnerPID)
	if alreadyGone(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if start != r.OwnerStartTime {
		return false, nil
	}
	exited, err := pidfdExited(fd, 0)
	return !exited, err
}

// ReconcileStartup removes every prior instance before listeners/pools start.
// A failed cleanup remains durable and blocks serving rather than resuming work.
func (s *StateStore) ReconcileStartup(ctx context.Context) error {
	_, err := s.reconcile(ctx, true)
	return err
}

func (s *StateStore) Reap(ctx context.Context) ([]string, error) { return s.reconcile(ctx, false) }

func (s *StateStore) reconcile(ctx context.Context, startup bool) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	var failures []error
	var reclaim []*stateRecord
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".corrupt-") {
			failures = append(failures, fmt.Errorf("quarantined ownership evidence requires attention: %s", entry.Name()))
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(failures, err)...)
		}
		vmID := strings.TrimSuffix(entry.Name(), ".json")
		wait, cancelWait := cleanupContext(ctx, s.cleanupGrace)
		lock, err := s.lockMetadata(wait, ctx, vmID)
		cancelWait()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		r, err := s.readRecord(vmID)
		if err != nil {
			if !alreadyGone(err) {
				quarantine := s.recordPath(vmID) + ".corrupt-" + stringid.New()
				moveErr := os.Rename(s.recordPath(vmID), quarantine)
				if moveErr == nil {
					moveErr = s.syncDirectory()
				}
				failures = append(failures, fmt.Errorf("quarantining %s: %w", vmID, errors.Join(err, moveErr)))
			}
			lock.Close()
			continue
		}
		alive, err := ownerAlive(r)
		if err != nil {
			failures = append(failures, fmt.Errorf("checking %s owner: %w", vmID, err))
			lock.Close()
			continue
		}
		stale := startup && r.InstanceID != s.instanceID
		expired := r.ExpiresAt != nil && !time.Now().Before(*r.ExpiresAt)
		if !stale && !expired && alive && r.State != "removing" {
			lock.Close()
			continue
		}
		var persistenceErr error
		if r.State != "removing" {
			r.State = "removing"
			persistenceErr = s.writeRecord(r)
			if persistenceErr == nil {
				logrus.WithFields(logrus.Fields{"vm_id": vmID, "profile": r.Profile, "expired": expired, "owner_alive": alive}).Warn("reaping owned Firecracker VM")
				if s.profiles[r.Profile] {
					if expired {
						metricVMTTLExpirations.WithLabelValues(r.Profile).Inc()
					} else {
						metricStaleVMReconciliations.WithLabelValues(r.Profile).Inc()
					}
				}
			}
		} else {
			// A prior rename may be visible after its directory fsync failed.
			// Re-establish the revocation barrier before any reclamation retry.
			persistenceErr = s.syncDirectory()
		}
		lock.Close()
		// Hard-deadline isolation must survive storage failure and must never
		// wait for another VM's CNI/containerd reclamation. Retain the original
		// record and prohibit resource deletion unless revocation committed.
		attempt, cancel := cleanupContext(ctx, s.cleanupGrace)
		isolationErr := stopRecordedProcesses(attempt, r)
		cancel()
		if persistenceErr == nil || isolationErr == nil {
			removed = append(removed, vmID)
		}
		if err := errors.Join(persistenceErr, isolationErr); err != nil {
			if s.profiles[r.Profile] {
				metricCleanupFailures.WithLabelValues(r.Profile).Inc()
			}
			failures = append(failures, fmt.Errorf("isolating %s: %w", vmID, err))
			continue
		}
		reclaim = append(reclaim, r)
	}
	// A single bounded reclaimer is sufficient: all eligible processes are
	// already isolated, so a stalled plugin cannot extend any execution lease.
	for _, r := range reclaim {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(failures, err)...)
		}
		if err := s.destroyRecord(ctx, r.VMID); err != nil {
			if s.profiles[r.Profile] {
				metricCleanupFailures.WithLabelValues(r.Profile).Inc()
			}
			failures = append(failures, fmt.Errorf("reaping %s: %w", r.VMID, err))
		}
	}
	return removed, errors.Join(failures...)
}

func cleanupContext(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(resourceCleanupTimeout)
	if grace > 0 && grace < resourceCleanupTimeout {
		deadline = time.Now().Add(grace)
	}
	if supplied, ok := ctx.Deadline(); ok && ctx.Err() == nil && supplied.Before(deadline) {
		deadline = supplied
	}
	return context.WithDeadline(context.Background(), deadline)
}

func (s *StateStore) destroyRecord(ctx context.Context, vmID string) error {
	attempt, cancel := cleanupContext(ctx, s.cleanupGrace)
	defer cancel()
	if !profileNamePattern.MatchString(vmID) {
		return fmt.Errorf("invalid journal VM ID")
	}
	lock, err := s.lockJournal(attempt, attempt, ".metadata.lock")
	if err != nil {
		return err
	}
	r, err := s.readRecord(vmID)
	if alreadyGone(err) {
		lock.Close()
		return nil
	}
	if err != nil {
		lock.Close()
		return err
	}
	var persistenceErr error
	if r.State != "removing" {
		r.State = "removing"
		persistenceErr = s.writeRecord(r)
		if persistenceErr == nil && s.profiles[r.Profile] && r.ExpiresAt != nil && !time.Now().Before(*r.ExpiresAt) {
			metricVMTTLExpirations.WithLabelValues(r.Profile).Inc()
		}
	} else {
		persistenceErr = s.syncDirectory()
	}
	lock.Close()
	// Execution isolation precedes the global reclamation queue, even when
	// revocation persistence failed. No resources may be deleted in that case.
	if err := errors.Join(persistenceErr, stopRecordedProcesses(attempt, r)); err != nil {
		return err
	}
	reclamation, err := s.lockJournal(attempt, ctx, ".reclamation.lock")
	if err != nil {
		return err
	}
	defer reclamation.Close()
	lock, err = s.lockJournal(attempt, attempt, ".metadata.lock")
	if err != nil {
		return err
	}
	// A previous reclaimer may have retired or replaced the record while this
	// caller waited. Only the freshly validated authority can permit cleanup.
	latest, err := s.readRecord(vmID)
	if alreadyGone(err) {
		lock.Close()
		return nil
	}
	if err != nil {
		lock.Close()
		return err
	}
	if latest.InstanceID != r.InstanceID || !latest.CreatedAt.Equal(r.CreatedAt) || latest.State != "removing" {
		lock.Close()
		return fmt.Errorf("journal authority changed before cleanup")
	}
	if err := s.syncDirectory(); err != nil {
		lock.Close()
		return err
	}
	alive, err := ownerAlive(latest)
	lock.Close()
	if err != nil {
		return err
	}
	r = latest
	active := alive && r.ProvisioningActive
	if err := s.cleanup(attempt, r, active); err != nil {
		return err
	}
	if active {
		return fmt.Errorf("retained removing tombstone while live provisioning settles")
	}
	lock, err = s.lockJournal(attempt, attempt, ".metadata.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	latest, err = s.readRecord(vmID)
	if alreadyGone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	alive, err = ownerAlive(latest)
	if err != nil {
		return err
	}
	if alive && latest.ProvisioningActive {
		return fmt.Errorf("retained live provisioning tombstone")
	}
	if latest.InstanceID != r.InstanceID || !latest.CreatedAt.Equal(r.CreatedAt) || latest.State != "removing" {
		return fmt.Errorf("journal authority changed during cleanup")
	}
	if err := os.Remove(s.recordPath(vmID)); err != nil && !alreadyGone(err) {
		return err
	}
	return s.syncDirectory()
}

func (s *StateStore) cleanupRecord(ctx context.Context, r *stateRecord, active bool) error {
	if err := stopRecordedProcesses(ctx, r); err != nil {
		return fmt.Errorf("stopping VMM: %w", err)
	}
	// A live creator may still be inside ADD/SDK spawn. Revoke publication and
	// isolate what is present, but never detach disks/network under a later spawn.
	if active {
		return nil
	}
	if recovered, err := s.recoverPreallocation(r); err != nil || recovered {
		return err
	}
	resourceDir := filepath.Dir(r.APISocketPath)
	_, err := ownedDirectoryForCleanup(resourceDir, r)
	if err != nil {
		return err
	}
	markerOwned, err := ownedMarker(r.NetNSPath+".owner", r)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(r.NetNSPath); err == nil {
		if !markerOwned || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("network namespace ownership conflict: %s", r.VMID)
		}
	} else if !alreadyGone(err) {
		return err
	}
	resources := &ownedResources{cniConfig: r.CNIConfig, cniBinPaths: r.CNIBinPaths, cniCacheDir: r.CNICacheDir, netnsPath: r.NetNSPath, cniIfName: r.CNIIfName, cniArgs: r.CNIArgs}
	if err := deleteOwnedNetwork(ctx, resources, r.VMID); err != nil && !alreadyGone(err) {
		return fmt.Errorf("deleting CNI network: %w", err)
	}
	if markerOwned {
		if err := removeOwnedNetNS(r.NetNSPath); err != nil {
			return err
		}
	}
	if markerOwned {
		if err := os.Remove(r.NetNSPath + ".owner"); err != nil && !alreadyGone(err) {
			return err
		}
	}
	if s.client == nil {
		return fmt.Errorf("containerd client unavailable for owned cleanup")
	}
	ctx = namespaces.WithNamespace(ctx, r.ContainerdNamespace)
	snapshots := s.client.SnapshotService(r.Snapshotter)
	info, err := snapshots.Stat(ctx, r.SnapshotID)
	if err == nil {
		if !ownedLabels(info.Labels, r) {
			return fmt.Errorf("snapshot ownership conflict: %s", r.SnapshotID)
		}
		if err := snapshots.Remove(ctx, r.SnapshotID); err != nil && !alreadyGone(err) {
			return err
		}
	} else if !alreadyGone(err) {
		return err
	}
	list, err := s.client.LeasesService().List(ctx, "id=="+strconv.Quote(r.LeaseID))
	if err != nil {
		return err
	}
	for _, lease := range list {
		if lease.ID != r.LeaseID {
			continue
		}
		if !ownedLabels(lease.Labels, r) {
			return fmt.Errorf("lease ownership conflict: %s", r.LeaseID)
		}
		if err := s.client.LeasesService().Delete(ctx, leases.Lease{ID: r.LeaseID}, leases.SynchronousDelete); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if err := s.removeOwnedResourceDirectory(resourceDir, r); err != nil {
		return err
	}
	return ctx.Err()
}

func ownedLabels(labels map[string]string, r *stateRecord) bool {
	return labels[instanceLabel] == r.InstanceID && labels[vmLabel] == r.VMID
}
func ownershipMarker(r *stateRecord) string { return r.InstanceID + "\n" + r.VMID + "\n" }

// The sibling marker survives recursive removal of .owner. Only this exact
// record's complete deletion authority permits a markerless directory retry.
func deletionMarkerPath(path string, r *stateRecord) string {
	return filepath.Join(filepath.Dir(path), ".deleting-"+r.InstanceID+"-"+r.VMID)
}

func ownedDirectoryForCleanup(path string, r *stateRecord) (bool, error) {
	deleting, err := ownedMarker(deletionMarkerPath(path, r), r)
	if err != nil {
		return false, err
	}
	return checkOwnedDirectory(path, r, deleting)
}

func (s *StateStore) removeOwnedResourceDirectory(path string, r *stateRecord) error {
	owned, err := ownedDirectoryForCleanup(path, r)
	if err != nil {
		return err
	}
	marker := deletionMarkerPath(path, r)
	deleting, err := ownedMarker(marker, r)
	if err != nil {
		return err
	}
	if !owned && !deleting {
		return nil
	}
	if !deleting {
		if err := writeOwnershipMarker(marker, r); err != nil {
			return err
		}
	}
	// A preceding publication may have returned after rename but before fsync.
	// Re-establish durable external authority before touching the directory.
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := s.syncDir(parent); err != nil {
		return err
	}
	if owned {
		remove := s.removeResourceDir
		if remove == nil {
			remove = os.RemoveAll
		}
		if err := remove(path); err != nil {
			return err
		}
	}
	// Commit disappearance before retiring the authority needed after a crash.
	if err := s.syncDir(parent); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil && !alreadyGone(err) {
		return err
	}
	return s.syncDir(parent)
}

func ownedDirectory(path string, r *stateRecord) (bool, error) {
	return checkOwnedDirectory(path, r, false)
}

func checkOwnedDirectory(path string, r *stateRecord, deleting bool) (bool, error) {
	info, err := os.Lstat(path)
	absent := alreadyGone(err)
	if absent && !deleting {
		return false, nil
	}
	if err != nil && !absent {
		return false, err
	}
	if !absent && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return false, fmt.Errorf("VM directory ownership conflict")
	}
	for parent := filepath.Dir(path); parent != "/"; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("VM directory traverses unsafe ancestor")
		}
	}
	if absent {
		return false, nil
	}
	owned, err := ownedMarker(filepath.Join(path, ".owner"), r)
	if err != nil {
		return false, err
	}
	if !owned && !deleting {
		return false, fmt.Errorf("VM directory has no ownership marker")
	}
	return true, nil
}

// Reap runs independently of the daemon; it intentionally does not acquire the
// lifetime owner lock and never resumes jobs or sweeps containerd namespaces.
func Reap(ctx context.Context, config *Config) error {
	if config == nil || config.Containerd == nil {
		return fmt.Errorf("containerd configuration is required")
	}
	client, err := containerd.New(config.Containerd.Address, containerd.WithTimeout(5*time.Second), containerd.WithDefaultNamespace(config.Containerd.Namespace))
	if err != nil {
		return err
	}
	defer client.Close()
	store, err := NewStateStore(config, client)
	if err != nil {
		return err
	}
	_, err = store.Reap(ctx)
	return err
}

func writeOwnershipMarker(path string, r *stateRecord) error {
	return writeOwnershipMarkerWithSync(path, r, (*os.File).Sync, (*os.File).Sync)
}

// Fault seams cover the prepublication and postpublication durability boundary.
func writeOwnershipMarkerWithSync(path string, r *stateRecord, syncFile, syncDir func(*os.File) error) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ownership-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	unpublished := true
	defer func() {
		if unpublished {
			_ = os.Remove(temp)
		}
	}()
	defer file.Close()
	if _, err := file.WriteString(ownershipMarker(r)); err != nil {
		return err
	}
	if err := syncFile(file); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, temp, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	unpublished = false
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := syncDir(parent); err != nil {
		return err
	}
	ancestor, err := os.Open(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return err
	}
	defer ancestor.Close()
	return syncDir(ancestor)
}

func ownedMarker(path string, r *stateRecord) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if alreadyGone(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return false, fmt.Errorf("invalid owned-resource marker file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1024))
	if err != nil {
		return false, err
	}
	if string(data) != ownershipMarker(r) {
		return false, fmt.Errorf("owned-resource marker identity conflict")
	}
	return true, nil
}
