package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These fixtures bypass only production's root prerequisite, not persistence.
func newTestStateStore(t *testing.T) *StateStore {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "journal")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	start, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	s := &StateStore{root: root, dir: dir, instanceID: "test-instance", ownerPID: os.Getpid(), ownerStartTime: start, cleanupGrace: time.Second, profiles: map[string]bool{}}
	s.syncFile = func(file *os.File) error { return file.Sync() }
	s.syncDir = func(file *os.File) error { return file.Sync() }
	s.cleanup = func(context.Context, *stateRecord, bool) error { return nil }
	return s
}

func testStateRecord(s *StateStore, profile, vmID, state string, expiry *time.Time) *stateRecord {
	binary, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", os.Getpid()))
	if err != nil {
		panic(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(binary, &stat); err != nil {
		panic(err)
	}
	dir := filepath.Join(s.root, "pools", profile, vmID)
	return &stateRecord{Version: 1, InstanceID: s.instanceID, OwnerPID: s.ownerPID, OwnerStartTime: s.ownerStartTime, VMID: vmID, Profile: profile, SnapshotID: vmID, LeaseID: "fireactions/pools/" + profile + "/" + vmID, CNIConfig: []byte(`{"cniVersion":"1.0.0","name":"saved-network","plugins":[{"type":"bridge"}]}`), CNIBinPaths: []string{"/opt/cni/bin"}, CNICacheDir: filepath.Join(dir, "cni"), CNIIfName: "eth0", NetNSPath: filepath.Join("/var/run/netns", vmID), VMMBinary: binary, VMMDevice: uint64(stat.Dev), VMMInode: stat.Ino, CID: 3, APISocketPath: filepath.Join(dir, "api.sock"), VsockPath: filepath.Join(dir, "vsock"), State: state, CreatedAt: time.Now().UTC(), ExpiresAt: expiry, ContainerdNamespace: "test-namespace", Snapshotter: defaultSnapshotter, AllocationReady: state != "provisioning"}
}

func startJournalProcess(t *testing.T, r *stateRecord) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestVMMProcessHelper$", "--", "--api-sock", r.APISocketPath)
	cmd.Env = append(os.Environ(), "FIREACTIONS_VMM_PROCESS_HELPER=normal")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("unexpected readiness %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not become ready")
	}
	r.VMMPID = cmd.Process.Pid
	r.VMMStartTime, err = processStartTime(r.VMMPID)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestStatePersistenceAtomicAndPublicationFailure(t *testing.T) {
	s := newTestStateStore(t)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "idle", nil)
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.recordPath(r.VMID))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(s.recordPath(r.VMID))
	if err != nil || file.Mode().Perm() != 0600 {
		t.Fatalf("journal permissions: %v %v", file, err)
	}
	m := &Machine{Name: r.VMID, State: "idle", resources: &ownedResources{journal: s, record: r}}
	failure := errors.New("injected fsync failure")
	s.syncFile = func(*os.File) error { return failure }
	expiry := time.Now().Add(time.Minute)
	if err := m.publishState("claimed", &expiry); !errors.Is(err, failure) {
		t.Fatalf("publication succeeded: %v", err)
	}
	if m.Metadata().State != "idle" {
		t.Fatal("failed durability published metadata")
	}
	after, err := os.ReadFile(s.recordPath(r.VMID))
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed atomic update changed record: %v", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".intent-") {
			t.Fatal("failed persistence leaked temporary record")
		}
	}
	s.syncFile = func(file *os.File) error { return file.Sync() }
	if err := m.publishState("claimed", &expiry); err != nil {
		t.Fatal(err)
	}
	stored, err := s.readRecord(r.VMID)
	if err != nil || stored.State != "claimed" || stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(expiry) {
		t.Fatalf("claimed record not durable: %+v %v", stored, err)
	}
}

func TestDirectorySyncFailureNeverPublishes(t *testing.T) {
	s := newTestStateStore(t)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "idle", nil)
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	m := &Machine{Name: r.VMID, State: "idle", resources: &ownedResources{journal: s, record: r}}
	s.syncDir = func(*os.File) error { return errors.New("directory fsync failed") }
	expiry := time.Now().Add(time.Minute)
	if err := m.publishState("claimed", &expiry); err == nil {
		t.Fatal("directory durability failure was hidden")
	}
	if m.Metadata().State != "idle" {
		t.Fatal("metadata exposed an uncommitted claim")
	}
	// Rename may already be visible, but recovery must see complete JSON only.
	if _, err := s.readRecord(r.VMID); err != nil {
		t.Fatalf("atomic rename exposed partial JSON: %v", err)
	}
}

func TestPersistentJournalFailureStillKillsSelectedVM(t *testing.T) {
	s := newTestStateStore(t)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "idle", nil)
	cmd := startJournalProcess(t, r)
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	m := &Machine{Name: r.VMID, State: "idle", resources: &ownedResources{journal: s, record: r}}
	s.syncFile = func(*os.File) error { return errors.New("persistent fsync failure") }
	expiry := time.Now().Add(time.Minute)
	if err := m.publishState("claimed", &expiry); err == nil {
		t.Fatal("claim succeeded")
	}
	if err := m.Destroy(context.Background()); err == nil {
		t.Fatal("durable cleanup failure was hidden")
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		if !alreadyGone(err) {
			t.Fatal(err)
		}
		return
	}
	defer unix.Close(fd)
	exited, err := pidfdExited(fd, 0)
	if err != nil || !exited {
		t.Fatalf("VMM survived persistent journal failure: %v %v", exited, err)
	}
	if _, err := os.Stat(s.recordPath(r.VMID)); err != nil {
		t.Fatalf("failed cleanup lost retry record: %v", err)
	}
}

func TestReaperPreservesHealthyOwnerAndRecoversExpiredOrDead(t *testing.T) {
	for _, mode := range []string{"healthy-idle", "healthy-claimed", "expired", "dead-owner"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStateStore(t)
			expiry := time.Now().Add(time.Minute)
			state := "claimed"
			var deadline *time.Time = &expiry
			if mode == "healthy-idle" {
				state = "idle"
				deadline = nil
			}
			if mode == "expired" {
				expiry = time.Now().Add(-time.Second)
			}
			r := testStateRecord(s, "ubuntu", "ubuntu-one", state, deadline)
			cmd := startJournalProcess(t, r)
			if mode == "dead-owner" {
				r.OwnerStartTime = "0"
			}
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			s.cleanup = func(ctx context.Context, record *stateRecord, active bool) error {
				return stopRecordedProcesses(ctx, record)
			}
			ids, err := s.Reap(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			exited, err := pidfdExited(fd, 0)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "healthy") {
				if exited || len(ids) != 0 {
					t.Fatalf("healthy VM reaped: %v %v", exited, ids)
				}
				if _, err := s.readRecord(r.VMID); err != nil {
					t.Fatal(err)
				}
			} else {
				if !exited || !reflect.DeepEqual(ids, []string{r.VMID}) {
					t.Fatalf("stale VM survived: %v %v", exited, ids)
				}
				if _, err := s.State(r.VMID); !alreadyGone(err) {
					t.Fatalf("cleaned record remains: %v", err)
				}
			}
		})
	}
}

func TestStartupDestroysPriorInstanceIdleAndClaimed(t *testing.T) {
	s := newTestStateStore(t)
	expiry := time.Now().Add(time.Hour)
	for _, state := range []string{"idle", "claimed"} {
		var deadline *time.Time
		if state == "claimed" {
			deadline = &expiry
		}
		r := testStateRecord(s, "removed-profile", "removed-profile-"+state, state, deadline)
		r.InstanceID = "prior-instance"
		if err := s.create(r); err != nil {
			t.Fatal(err)
		}
	}
	var destroyed []string
	s.cleanup = func(_ context.Context, r *stateRecord, _ bool) error {
		destroyed = append(destroyed, r.VMID)
		if r.ContainerdNamespace != "test-namespace" {
			t.Fatal("lost old namespace")
		}
		return nil
	}
	if err := s.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(destroyed) != 2 {
		t.Fatalf("prior idle/claimed not cleaned: %v", destroyed)
	}
}

func TestCorruptOwnershipQuarantinedWithoutCleanup(t *testing.T) {
	for _, mode := range []string{"invalid-json", "unknown-version", "escaped-path", "symlink", "fifo"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStateStore(t)
			r := testStateRecord(s, "ubuntu", "ubuntu-one", "idle", nil)
			outside := filepath.Join(t.TempDir(), "unrelated")
			if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(r)
			switch mode {
			case "invalid-json":
				data = []byte("{broken")
			case "unknown-version":
				r.Version = 2
				data, _ = json.Marshal(r)
			case "escaped-path":
				r.NetNSPath = outside
				data, _ = json.Marshal(r)
			}
			if mode == "symlink" {
				if err := os.Symlink(outside, s.recordPath(r.VMID)); err != nil {
					t.Fatal(err)
				}
			} else if mode == "fifo" {
				if err := unix.Mkfifo(s.recordPath(r.VMID), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(s.recordPath(r.VMID), data, 0600); err != nil {
				t.Fatal(err)
			}
			s.cleanup = func(context.Context, *stateRecord, bool) error {
				t.Fatal("corrupt record became cleanup authority")
				return nil
			}
			if _, err := s.Reap(context.Background()); err == nil {
				t.Fatal("corruption not reported")
			}
			if _, err := s.State(r.VMID); err == nil || alreadyGone(err) {
				t.Fatalf("quarantine became successful absence: %v", err)
			}
			entries, err := os.ReadDir(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), r.VMID+".json.corrupt-") {
					found = true
				}
			}
			if !found {
				t.Fatal("corrupt evidence lost")
			}
			if _, err := s.Reap(context.Background()); err == nil {
				t.Fatal("retained quarantine no longer reported")
			}
			preserved, err := os.ReadFile(outside)
			if err != nil || string(preserved) != "preserve" {
				t.Fatal("unrelated evidence/resource modified")
			}
		})
	}
}

func TestReaperPIDReuseAndSpawnCrashWindow(t *testing.T) {
	s := newTestStateStore(t)
	expiry := time.Now().Add(-time.Second)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "claimed", &expiry)
	original := startJournalProcess(t, r)
	_, unrelated := startOwnedProcess(t, false)
	r.VMMPID = unrelated.pid
	r.VMMStartTime = "0"
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	s.cleanup = func(ctx context.Context, r *stateRecord, _ bool) error { return stopRecordedProcesses(ctx, r) }
	if _, err := s.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := unrelated.verify(); err != nil {
		t.Fatalf("recycled PID was signalled: %v", err)
	}
	fd, err := unix.PidfdOpen(original.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if exited, err := pidfdExited(fd, 0); err != nil || !exited {
		t.Fatalf("exact socket crash-window process survived: %v %v", exited, err)
	}
}

func TestRetainedCleanupFailureRetriesDurableRecord(t *testing.T) {
	s := newTestStateStore(t)
	expiry := time.Now().Add(-time.Second)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "claimed", &expiry)
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	resource := filepath.Join(t.TempDir(), "owned-resource")
	if err := os.WriteFile(resource, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	s.cleanup = func(context.Context, *stateRecord, bool) error {
		attempts++
		if attempts == 1 {
			return errors.New("transient resource failure")
		}
		return os.Remove(resource)
	}
	ids, err := s.Reap(context.Background())
	if err == nil || !reflect.DeepEqual(ids, []string{r.VMID}) {
		t.Fatalf("partial cleanup became success: %v %v", ids, err)
	}
	if state, err := s.State(r.VMID); err != nil || state != "removing" {
		t.Fatalf("retry record lost: %q %v", state, err)
	}
	if _, err := os.Stat(resource); err != nil {
		t.Fatal("failure removed resource")
	}
	if _, err := s.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resource); !alreadyGone(err) {
		t.Fatalf("retry did not cleanup: %v", err)
	}
	if _, err := s.State(r.VMID); !alreadyGone(err) {
		t.Fatalf("completed retry record remains: %v", err)
	}
}

func TestLiveProvisioningRetainsTombstoneAndCannotPublish(t *testing.T) {
	s := newTestStateStore(t)
	expiry := time.Now().Add(-time.Second)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "provisioning", &expiry)
	r.ProvisioningActive = true
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	var activeSeen []bool
	s.cleanup = func(_ context.Context, _ *stateRecord, active bool) error {
		activeSeen = append(activeSeen, active)
		return nil
	}
	if _, err := s.Reap(context.Background()); err == nil {
		t.Fatal("in-flight tombstone deleted")
	}
	if state, err := s.State(r.VMID); err != nil || state != "removing" {
		t.Fatalf("live creator lost tombstone: %q %v", state, err)
	}
	if err := s.publish(r.VMID, "idle", nil); err == nil {
		t.Fatal("revoked creator published idle")
	}
	if err := s.finishProvisioning(r.VMID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(activeSeen, []bool{true, false}) {
		t.Fatalf("cleanup detached live provisioning resources: %v", activeSeen)
	}
}

func TestSavedCNIConfigurationReplayedByDEL(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload")
	identity := filepath.Join(dir, "identity")
	plugin := filepath.Join(dir, "journal-network")
	script := "#!/bin/sh\ncat > '" + payload + "'\nprintf '%s\\n' \"$CNI_COMMAND\" \"$CNI_CONTAINERID\" \"$CNI_NETNS\" \"$CNI_IFNAME\" \"$CNI_ARGS\" > '" + identity + "'\n"
	if err := os.WriteFile(plugin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	saved := []byte(`{"cniVersion":"1.0.0","name":"saved-network","plugins":[{"type":"journal-network","saved_identity":"original-effective-config"}]}`)
	r := &ownedResources{cniConfig: saved, cniBinPaths: []string{dir}, cniCacheDir: filepath.Join(dir, "cache"), netnsPath: filepath.Join(dir, "netns"), cniIfName: "eth0", cniArgs: [][2]string{{"TEST_IDENTITY", "original"}}}
	if err := deleteOwnedNetwork(context.Background(), r, "owned-vm"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["saved_identity"] != "original-effective-config" || config["name"] != "saved-network" {
		t.Fatalf("DEL did not use captured effective configuration: %s", data)
	}
	data, err = os.ReadFile(identity)
	if err != nil {
		t.Fatal(err)
	}
	want := "DEL\nowned-vm\n" + r.netnsPath + "\neth0\nTEST_IDENTITY=original\n"
	if string(data) != want {
		t.Fatalf("DEL runtime identity = %q, want %q", data, want)
	}
}

func TestStartupRetainsUnsafeLiveIdentityWithoutDetachingResources(t *testing.T) {
	for _, evidence := range []string{"start-time", "binary", "inode"} {
		t.Run(evidence, func(t *testing.T) {
			s := newTestStateStore(t)
			r := testStateRecord(s, "ubuntu", "ubuntu-one", "idle", nil)
			cmd := startJournalProcess(t, r)
			r.InstanceID = "prior-instance"
			switch evidence {
			case "start-time":
				r.VMMStartTime = "0"
			case "binary":
				r.VMMBinary += ".unrelated"
			case "inode":
				r.VMMInode++
			}
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			resource := filepath.Join(t.TempDir(), "owned-disk")
			if err := os.WriteFile(resource, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			s.cleanup = func(ctx context.Context, record *stateRecord, _ bool) error {
				if err := stopRecordedProcesses(ctx, record); err != nil {
					return err
				}
				return os.Remove(resource)
			}
			if err := s.ReconcileStartup(context.Background()); err == nil {
				t.Fatal("unsafe live stale VM did not block serving")
			}
			if state, err := s.State(r.VMID); err != nil || state != "removing" {
				t.Fatalf("unsafe ownership lost retry authority: %q %v", state, err)
			}
			if _, err := os.Stat(resource); err != nil {
				t.Fatal("unsafe process disk was detached")
			}
			fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			if exited, err := pidfdExited(fd, 0); err != nil || exited {
				t.Fatalf("identity conflict was signalled: %v %v", exited, err)
			}
		})
	}
}

func TestWriteAheadIdentityFindsSpawnWithNoSavedPID(t *testing.T) {
	s := newTestStateStore(t)
	expiry := time.Now().Add(-time.Second)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "provisioning", &expiry)
	cmd := startJournalProcess(t, r)
	r.VMMPID, r.VMMStartTime = 0, ""
	r.OwnerStartTime = "0"
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	s.cleanup = func(ctx context.Context, record *stateRecord, _ bool) error {
		return stopRecordedProcesses(ctx, record)
	}
	if _, err := s.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if exited, err := pidfdExited(fd, 0); err != nil || !exited {
		t.Fatalf("spawn-to-PID journal crash window leaked VMM: %v %v", exited, err)
	}
}

func TestForeignNamespaceMarkerBlocksCleanup(t *testing.T) {
	s := newTestStateStore(t)
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "idle", nil)
	resourceDir := filepath.Dir(r.APISocketPath)
	if err := os.MkdirAll(resourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnershipMarker(filepath.Join(resourceDir, ".owner"), r); err != nil {
		t.Fatal(err)
	}
	r.NetNSPath = filepath.Join(t.TempDir(), "unrelated-netns")
	if err := os.WriteFile(r.NetNSPath, []byte("unrelated resource"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.NetNSPath+".owner", []byte("unrelated evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	// A nonexistent plugin would produce a different error if DEL ran before
	// ownership validation. No privileged namespace or containerd is needed.
	err := s.cleanupRecord(context.Background(), r, false)
	if err == nil || !strings.Contains(err.Error(), "marker identity conflict") {
		t.Fatalf("foreign namespace became CNI cleanup authority: %v", err)
	}
	if data, err := os.ReadFile(r.NetNSPath); err != nil || string(data) != "unrelated resource" {
		t.Fatal("foreign namespace path changed")
	}
	if data, err := os.ReadFile(r.NetNSPath + ".owner"); err != nil || string(data) != "unrelated evidence" {
		t.Fatal("foreign ownership evidence changed")
	}
}

func TestJournalCleanupHonorsGraceAndIgnoresCanceledCaller(t *testing.T) {
	s := newTestStateStore(t)
	s.cleanupGrace = 250 * time.Millisecond
	r := testStateRecord(s, "ubuntu", "ubuntu-one", "idle", nil)
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	called := false
	started := time.Now()
	s.cleanup = func(ctx context.Context, _ *stateRecord, _ bool) error {
		called = true
		if err := ctx.Err(); err != nil {
			t.Fatalf("caller cancellation weakened cleanup: %v", err)
		}
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(started.Add(s.cleanupGrace+50*time.Millisecond)) {
			t.Fatalf("cleanup grace was not bounded: %v %v", deadline, ok)
		}
		return errors.New("retain unfinished cleanup")
	}
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	m := &Machine{Name: r.VMID, State: "idle", resources: &ownedResources{journal: s, record: r}}
	if err := m.Destroy(caller); err == nil || !called {
		t.Fatalf("canceled caller abandoned cleanup: %v", err)
	}
	if state, err := s.State(r.VMID); err != nil || state != "removing" {
		t.Fatalf("unfinished bounded cleanup lost retry record: %q %v", state, err)
	}
}

func journalProcessExited(t *testing.T, cmd *exec.Cmd, timeout int) bool {
	t.Helper()
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if alreadyGone(err) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	exited, err := pidfdExited(fd, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return exited
}

func TestStartupRecoversInterruptedDirectoryOwnership(t *testing.T) {
	for _, window := range []string{"before-marker", "partial-marker", "foreign-content", "after-publication", "partial-reservation"} {
		t.Run(window, func(t *testing.T) {
			s := newTestStateStore(t)
			expiry := time.Now().Add(-time.Second)
			r := testStateRecord(s, "ubuntu", "ubuntu-incomplete-directory", "provisioning", &expiry)
			r.InstanceID, r.OwnerStartTime, r.ProvisioningActive = "prior-instance", "0", true
			r.AllocationReady = false
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			stage, reservation := s.allocationPaths(r)
			if err := os.MkdirAll(filepath.Dir(stage), 0700); err != nil {
				t.Fatal(err)
			}
			if window == "partial-reservation" {
				marker := ownershipMarker(r)
				if err := os.WriteFile(reservation, []byte(marker[:len(marker)/2]), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := writeOwnershipMarker(reservation, r); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(stage, 0700); err != nil {
					t.Fatal(err)
				}
			}
			dir := stage
			if window == "partial-marker" {
				marker := ownershipMarker(r)
				if err := os.WriteFile(filepath.Join(stage, ".owner"), []byte(marker[:len(marker)/2]), 0600); err != nil {
					t.Fatal(err)
				}
			}
			foreign := filepath.Join(stage, "unrelated-content")
			if window == "foreign-content" {
				if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if window == "after-publication" {
				if err := writeOwnershipMarker(filepath.Join(stage, ".owner"), r); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Dir(r.APISocketPath)
				if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dir, unix.RENAME_NOREPLACE); err != nil {
					t.Fatal(err)
				}
			}
			s.cleanup = s.cleanupRecord
			err := s.ReconcileStartup(context.Background())
			if window == "foreign-content" {
				if err == nil {
					t.Fatal("foreign staging contents were accepted as owned")
				}
				if data, err := os.ReadFile(foreign); err != nil || string(data) != "preserve" {
					t.Fatal("foreign staging path was removed")
				}
				return
			}
			if err != nil {
				t.Fatalf("interrupted staging ownership cannot recover: %v", err)
			}
			for _, path := range []string{dir, reservation} {
				if _, err := os.Lstat(path); !alreadyGone(err) {
					t.Fatalf("incomplete allocation survived recovery: %s: %v", path, err)
				}
			}
			if _, err := s.State(r.VMID); !alreadyGone(err) {
				t.Fatalf("preallocation retry record never converged: %v", err)
			}
		})
	}
}

func startForeignSocketProcess(t *testing.T, socket string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "printf 'ready\\n'; while :; do read ignored; done", "foreign-vmm", "--api-sock", socket)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("foreign readiness %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreign process did not become ready")
	}
	return cmd
}

func TestReaperForeignSocketArgvCannotVetoOwnedIsolation(t *testing.T) {
	for _, order := range []string{"foreign-first", "owned-first"} {
		t.Run(order, func(t *testing.T) {
			s := newTestStateStore(t)
			expiry := time.Now().Add(-time.Second)
			r := testStateRecord(s, "ubuntu", "ubuntu-owned-process", "claimed", &expiry)
			var owned, foreign *exec.Cmd
			if order == "foreign-first" {
				foreign = startForeignSocketProcess(t, r.APISocketPath)
				owned = startJournalProcess(t, r)
			} else {
				owned = startJournalProcess(t, r)
				foreign = startForeignSocketProcess(t, r.APISocketPath)
			}
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			s.cleanup = func(ctx context.Context, r *stateRecord, _ bool) error { return stopRecordedProcesses(ctx, r) }
			if _, err := s.Reap(context.Background()); err != nil {
				t.Fatalf("foreign copied argv vetoed owned cleanup: %v", err)
			}
			if !journalProcessExited(t, owned, 0) {
				t.Fatal("owned expired VMM survived counterfeit socket argv")
			}
			if journalProcessExited(t, foreign, 0) {
				t.Fatal("foreign executable was signalled")
			}
			if _, err := s.State(r.VMID); !alreadyGone(err) {
				t.Fatalf("counterfeit argv retained cleaned record: %v", err)
			}
		})
	}
}

func TestIndependentReaperIsolatesDespiteRevocationPersistenceFailure(t *testing.T) {
	for _, operation := range []string{"reap", "destroy-record"} {
		t.Run(operation, func(t *testing.T) {
			s := newTestStateStore(t)
			expiry := time.Now().Add(-time.Second)
			r := testStateRecord(s, "ubuntu", "ubuntu-expired-orphan", "claimed", &expiry)
			cmd := startJournalProcess(t, r)
			r.OwnerStartTime = "0"
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			resource := filepath.Join(t.TempDir(), "owned-disk")
			if err := os.WriteFile(resource, []byte("preserve until durable revocation"), 0600); err != nil {
				t.Fatal(err)
			}
			s.cleanup = func(context.Context, *stateRecord, bool) error {
				t.Error("resource cleanup ran without durable removing state")
				return os.Remove(resource)
			}
			failure := errors.New("persistent revocation fsync failure")
			s.syncFile = func(*os.File) error { return failure }
			var err error
			if operation == "reap" {
				_, err = s.Reap(context.Background())
			} else {
				err = s.destroyRecord(context.Background(), r.VMID)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("revocation failure hidden: %v", err)
			}
			if !journalProcessExited(t, cmd, 0) {
				t.Fatal("orphan continued executing because revocation could not persist")
			}
			if record, err := s.readRecord(r.VMID); err != nil || record.State != "claimed" {
				t.Fatalf("failed persist lost original retry evidence: %+v %v", record, err)
			}
			if data, err := os.ReadFile(resource); err != nil || string(data) != "preserve until durable revocation" {
				t.Fatal("failed revocation detached owned resources")
			}
		})
	}
}

func TestReaperIsolatesAllBeforeBlockingCNIReclamation(t *testing.T) {
	s := newTestStateStore(t)
	s.cleanupGrace = 5 * time.Second
	expiry := time.Now().Add(-time.Second)
	first := testStateRecord(s, "ubuntu", "ubuntu-a-blocked", "claimed", &expiry)
	second := testStateRecord(s, "ubuntu", "ubuntu-z-expired", "claimed", &expiry)
	firstProcess := startJournalProcess(t, first)
	secondProcess := startJournalProcess(t, second)
	dir := t.TempDir()
	entered := filepath.Join(dir, "entered")
	release := filepath.Join(dir, "release")
	if err := unix.Mkfifo(release, 0600); err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(dir, "blocking-network")
	script := "#!/bin/sh\nprintf entered > '" + entered + "'\nIFS= read -r release < '" + release + "'\n"
	if err := os.WriteFile(plugin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	first.CNIConfig = []byte(`{"cniVersion":"1.0.0","name":"saved-blocking-network","plugins":[{"type":"blocking-network"}]}`)
	first.CNIBinPaths = []string{dir}
	for _, record := range []*stateRecord{first, second} {
		if err := s.create(record); err != nil {
			t.Fatal(err)
		}
	}
	s.cleanup = func(ctx context.Context, r *stateRecord, _ bool) error {
		if err := stopRecordedProcesses(ctx, r); err != nil {
			return err
		}
		if r.VMID == first.VMID {
			return deleteOwnedNetwork(ctx, &ownedResources{cniConfig: r.CNIConfig, cniBinPaths: r.CNIBinPaths, cniCacheDir: r.CNICacheDir, netnsPath: r.NetNSPath, cniIfName: r.CNIIfName, cniArgs: r.CNIArgs}, r.VMID)
		}
		return nil
	}
	finished := make(chan error, 1)
	go func() { _, err := s.Reap(context.Background()); finished <- err }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CNI reclamation never entered blocking plugin")
		}
		time.Sleep(5 * time.Millisecond)
	}
	firstExited := journalProcessExited(t, firstProcess, 0)
	secondExited := journalProcessExited(t, secondProcess, 1000)
	fd, err := unix.Open(release, unix.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := unix.Write(fd, []byte("release\n"))
	unix.Close(fd)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("reclamation failed after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not settle after release")
	}
	if !firstExited || !secondExited {
		t.Fatalf("blocking CNI delayed hard-deadline isolation: first=%v second=%v", firstExited, secondExited)
	}
}

func TestPreallocationPreservesForeignEmptyDirectory(t *testing.T) {
	for _, phase := range []string{"recovery", "failed-prepare"} {
		t.Run(phase, func(t *testing.T) {
			s := newTestStateStore(t)
			expiry := time.Now().Add(-time.Second)
			r := testStateRecord(s, "ubuntu", "ubuntu-foreign-empty", "provisioning", &expiry)
			r.AllocationReady = false
			r.OwnerStartTime = "0"
			dir := filepath.Dir(r.APISocketPath)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			if phase == "failed-prepare" && s.prepareResourceDirectory(r) == nil {
				t.Fatal("foreign allocation path was adopted")
			}
			s.cleanup = s.cleanupRecord
			if err := s.ReconcileStartup(context.Background()); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(dir)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("foreign empty directory deleted or replaced: %v", err)
			}
		})
	}
}

func TestJournalLocksStayBoundedAcrossDistinctLifecycles(t *testing.T) {
	s := newTestStateStore(t)
	var stableLocks map[string]os.FileInfo
	for i := range 128 {
		vmID := fmt.Sprintf("ubuntu-lifecycle-%d", i)
		if err := s.create(testStateRecord(s, "ubuntu", vmID, "idle", nil)); err != nil {
			t.Fatal(err)
		}
		if err := s.destroyRecord(context.Background(), vmID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.State(vmID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destroyed record remained authoritative: %v", err)
		}
		if i == 0 {
			stableLocks = make(map[string]os.FileInfo)
			entries, err := os.ReadDir(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil {
					t.Fatal(err)
				}
				stableLocks[entry.Name()] = info
			}
		}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(stableLocks) {
		t.Fatalf("successful lifecycles grew journal artifacts: %v", entries)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		before, ok := stableLocks[entry.Name()]
		if err != nil || !ok || !strings.HasSuffix(entry.Name(), ".lock") || !os.SameFile(before, info) {
			t.Fatalf("journal lock was removed/replaced across lifecycles: %s %v", entry.Name(), err)
		}
	}
}

func TestJournalLeavesLegacyLockInodesUntouched(t *testing.T) {
	s := newTestStateStore(t)
	vmID := "ubuntu-legacy"
	path := filepath.Join(s.dir, vmID+".lock")
	legacy, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	before, err := legacy.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// An old daemon/reaper may hold this inode or have a waiter on it.
	if err := unix.Flock(int(legacy.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := s.create(testStateRecord(s, "ubuntu", vmID, "idle", nil)); err != nil {
		t.Fatal(err)
	}
	if err := s.destroyRecord(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("legacy lock inode was removed/replaced while in use: %v", err)
	}
}

func TestJournalMetadataProcessHelper(t *testing.T) {
	mode := os.Getenv("FIREACTIONS_JOURNAL_HELPER")
	if mode == "" {
		return
	}
	root := os.Getenv("FIREACTIONS_JOURNAL_ROOT")
	s := &StateStore{root: root, dir: filepath.Join(root, "journal"), instanceID: "test-instance"}
	s.syncFile = func(file *os.File) error { return file.Sync() }
	s.syncDir = func(file *os.File) error { return file.Sync() }
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	fmt.Println("attempting")
	if mode == "reclamation" {
		s.cleanupGrace = 5 * time.Second
		s.cleanup = cleanupSavedJournalNetwork
		signaled := false
		s.syncDir = func(file *os.File) error {
			err := file.Sync()
			if !signaled {
				fmt.Println("revoked")
				signaled = true
			}
			return err
		}
		if err := s.destroyRecord(context.Background(), "ubuntu-reclamation"); err != nil {
			t.Fatal(err)
		}
		fmt.Println("finished")
		return
	}
	expiry := time.Now().Add(time.Minute)
	if mode == "cleanup-race" {
		if err := s.publish("ubuntu-first", "claimed", &expiry); err == nil {
			t.Fatal("cleanup revocation was overwritten")
		}
		if state, err := s.State("ubuntu-first"); err != nil || state != "removing" {
			t.Fatalf("cleanup authority was lost: %s %v", state, err)
		}
		fmt.Println("revoked")
	}
	if err := s.publish("ubuntu-second", "claimed", &expiry); err != nil {
		t.Fatal(err)
	}
	fmt.Println("claimed")
}

func startJournalMetadataHelper(t *testing.T, s *StateStore, mode string) (*exec.Cmd, <-chan string, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJournalMetadataProcessHelper$")
	cmd.Env = append(os.Environ(), "FIREACTIONS_JOURNAL_HELPER="+mode, "FIREACTIONS_JOURNAL_ROOT="+s.root)
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	expectJournalHelperLine(t, lines, "ready")
	return cmd, lines, input
}

func expectJournalHelperLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got := <-lines:
		if got != want {
			t.Fatalf("journal helper: got %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("journal helper timed out waiting for %q", want)
	}
}

func TestJournalMetadataLockSerializesCollidingVMsAcrossProcesses(t *testing.T) {
	s := newTestStateStore(t)
	if err := s.create(testStateRecord(s, "ubuntu", "ubuntu-second", "idle", nil)); err != nil {
		t.Fatal(err)
	}
	// All VM IDs intentionally share a metadata lock. A different process's
	// read/modify/write must wait even when its VM ID differs from the holder.
	lock, err := s.lock("ubuntu-first")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cmd, lines, input := startJournalMetadataHelper(t, s, "update")
	if _, err := io.WriteString(input, "go\n"); err != nil {
		t.Fatal(err)
	}
	expectJournalHelperLine(t, lines, "attempting")
	select {
	case got := <-lines:
		t.Fatalf("colliding metadata update bypassed held lock: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	expectJournalHelperLine(t, lines, "claimed")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if state, err := s.State("ubuntu-second"); err != nil || state != "claimed" {
		t.Fatalf("cross-process claim was not durable: %q %v", state, err)
	}
}

func TestJournalCleanupAllowsCollidingProcessUpdateWithoutLosingAuthority(t *testing.T) {
	s := newTestStateStore(t)
	for _, vmID := range []string{"ubuntu-first", "ubuntu-second"} {
		if err := s.create(testStateRecord(s, "ubuntu", vmID, "idle", nil)); err != nil {
			t.Fatal(err)
		}
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s.cleanup = func(_ context.Context, r *stateRecord, _ bool) error {
		if r.VMID == "ubuntu-first" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.destroyRecord(context.Background(), "ubuntu-first") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not leave the metadata lock")
	}
	cmd, lines, input := startJournalMetadataHelper(t, s, "cleanup-race")
	if _, err := io.WriteString(input, "go\n"); err != nil {
		t.Fatal(err)
	}
	expectJournalHelperLine(t, lines, "attempting")
	expectJournalHelperLine(t, lines, "revoked")
	expectJournalHelperLine(t, lines, "claimed")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Successful metadata work while cleanup is still blocked proves no
	// colliding VM can deadlock by retaining the lock across external cleanup.
	release <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup failed to reacquire its colliding metadata lock")
	}
	if _, err := s.State("ubuntu-first"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed VM journal remained: %v", err)
	}
	if state, err := s.State("ubuntu-second"); err != nil || state != "claimed" {
		t.Fatalf("cleanup lost another VM's cross-process claim: %q %v", state, err)
	}
	expired := time.Now().Add(-time.Second)
	if err := s.update("ubuntu-second", func(r *stateRecord) error {
		r.ExpiresAt = &expired
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.create(testStateRecord(s, "ubuntu", "ubuntu-third", "claimed", &expired)); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Reap(context.Background())
	if err != nil || len(removed) != 2 {
		t.Fatalf("reaping colliding records lost authority: %v %v", removed, err)
	}
	for _, vmID := range []string{"ubuntu-second", "ubuntu-third"} {
		if _, err := s.State(vmID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reaper retained colliding record %s: %v", vmID, err)
		}
	}
}

func cleanupSavedJournalNetwork(ctx context.Context, r *stateRecord, _ bool) error {
	return deleteOwnedNetwork(ctx, &ownedResources{cniConfig: r.CNIConfig, cniBinPaths: r.CNIBinPaths, cniCacheDir: r.CNICacheDir, netnsPath: r.NetNSPath, cniIfName: r.CNIIfName, cniArgs: r.CNIArgs}, r.VMID)
}

func waitJournalFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJournalReclamationSerializesIndependentStoresAndProcesses(t *testing.T) {
	for _, acrossProcess := range []bool{false, true} {
		name := "independent-stores"
		if acrossProcess {
			name = "independent-processes"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStateStore(t)
			s.cleanupGrace = 5 * time.Second
			dir := t.TempDir()
			entered := filepath.Join(dir, "entered")
			release := filepath.Join(dir, "release")
			calls := filepath.Join(dir, "calls")
			overlap := filepath.Join(dir, "overlap")
			resource := filepath.Join(dir, "owned-resource")
			active := filepath.Join(dir, "active")
			if err := os.WriteFile(resource, []byte("owned"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(release, 0600); err != nil {
				t.Fatal(err)
			}
			// The real DEL executable detects concurrent entry and mutates an
			// owned resource. A replay after retirement also leaves another call.
			script := "#!/bin/sh\nprintf 'DEL\\n' >> '" + calls + "'\n" +
				"mkdir '" + active + "' || { printf overlap > '" + overlap + "'; exit 1; }\n" +
				"printf entered > '" + entered + "'\nIFS= read -r release < '" + release + "'\n" +
				"rm -f '" + resource + "'\nrmdir '" + active + "'\n"
			if err := os.WriteFile(filepath.Join(dir, "serialized-network"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			r := testStateRecord(s, "ubuntu", "ubuntu-reclamation", "idle", nil)
			r.CNIConfig = []byte(`{"cniVersion":"1.0.0","name":"serialized-network","plugins":[{"type":"serialized-network"}]}`)
			r.CNIBinPaths = []string{dir}
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			s.cleanup = cleanupSavedJournalNetwork
			firstDone := make(chan error, 1)
			go func() { firstDone <- s.destroyRecord(context.Background(), r.VMID) }()
			waitJournalFile(t, entered)
			var cmd *exec.Cmd
			var lines <-chan string
			secondDone := make(chan error, 1)
			if acrossProcess {
				var input io.WriteCloser
				cmd, lines, input = startJournalMetadataHelper(t, s, "reclamation")
				if _, err := io.WriteString(input, "go\n"); err != nil {
					t.Fatal(err)
				}
				expectJournalHelperLine(t, lines, "attempting")
				expectJournalHelperLine(t, lines, "revoked")
			} else {
				second := *s
				revoked := make(chan struct{}, 1)
				second.syncDir = func(file *os.File) error {
					err := file.Sync()
					select {
					case revoked <- struct{}{}:
					default:
					}
					return err
				}
				go func() { secondDone <- second.destroyRecord(context.Background(), r.VMID) }()
				select {
				case <-revoked:
				case <-time.After(5 * time.Second):
					t.Fatal("second store did not revoke before waiting")
				}
			}
			select {
			case err := <-secondDone:
				t.Fatalf("pending cleanup returned before first finished: %v", err)
			case line := <-lines:
				t.Fatalf("pending process returned before first finished: %s", line)
			case <-time.After(100 * time.Millisecond):
			}
			if data, err := os.ReadFile(calls); err != nil || string(data) != "DEL\n" {
				t.Fatalf("same-VM reclamation overlapped: %q %v", data, err)
			}
			if _, err := os.Stat(overlap); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("network cleanup entered concurrently: %v", err)
			}
			fd, err := unix.Open(release, unix.O_WRONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = unix.Write(fd, []byte("release\n"))
			unix.Close(fd)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-firstDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("first reclamation did not finish")
			}
			if acrossProcess {
				expectJournalHelperLine(t, lines, "finished")
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
			} else {
				select {
				case err := <-secondDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("pending store replayed cleanup after retirement")
				}
			}
			if data, err := os.ReadFile(calls); err != nil || string(data) != "DEL\n" {
				t.Fatalf("retired authority replayed network cleanup: %q %v", data, err)
			}
			if _, err := os.Stat(resource); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned resource was not reclaimed: %v", err)
			}
			if _, err := s.State(r.VMID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed reclamation retained authority: %v", err)
			}
		})
	}
}

func TestJournalReclamationWaitIsBoundedAndIsolatesBeforeWaiting(t *testing.T) {
	for _, mode := range []string{"cancellation", "deadline", "cleanup-grace"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStateStore(t)
			s.cleanupGrace = 2 * time.Second
			lock, err := s.lockJournal(context.Background(), context.Background(), ".reclamation.lock")
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			expiry := time.Now().Add(-time.Second)
			r := testStateRecord(s, "ubuntu", "ubuntu-expired", "claimed", &expiry)
			process := startJournalProcess(t, r)
			resource := filepath.Join(t.TempDir(), "owned-resource")
			if err := os.WriteFile(resource, []byte("owned"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.create(r); err != nil {
				t.Fatal(err)
			}
			s.cleanup = func(context.Context, *stateRecord, bool) error { return os.Remove(resource) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				want = context.DeadlineExceeded
			} else if mode == "cleanup-grace" {
				s.cleanupGrace = 500 * time.Millisecond
				want = context.DeadlineExceeded
			}
			done := make(chan error, 1)
			started := time.Now()
			go func() { done <- s.destroyRecord(ctx, r.VMID) }()
			if !journalProcessExited(t, process, 300) {
				t.Fatal("expired VMM execution survived blocked reclamation")
			}
			if mode == "cancellation" {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("lock wait returned %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("reclamation lock exceeded cancellation/cleanup budget")
			}
			if time.Since(started) > time.Second {
				t.Fatal("reclamation lock wait exceeded bounded cleanup budget")
			}
			if _, err := os.Stat(resource); err != nil {
				t.Fatalf("waiting caller deleted resources before lock: %v", err)
			}
			if state, err := s.State(r.VMID); err != nil || state != "removing" {
				t.Fatalf("interrupted reclamation lost retry authority: %s %v", state, err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.destroyRecord(context.Background(), r.VMID); err != nil {
				t.Fatalf("interrupted lock wait could not retry: %v", err)
			}
			if _, err := os.Stat(resource); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retry failed to reclaim resource: %v", err)
			}
		})
	}
}

func TestJournalReclamationRejectsReplacedAuthorityAfterWaiting(t *testing.T) {
	s := newTestStateStore(t)
	s.cleanupGrace = 5 * time.Second
	r := testStateRecord(s, "ubuntu", "ubuntu-replaced", "idle", nil)
	if err := s.create(r); err != nil {
		t.Fatal(err)
	}
	resource := filepath.Join(t.TempDir(), "owned-resource")
	if err := os.WriteFile(resource, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	s.cleanup = func(context.Context, *stateRecord, bool) error { return os.Remove(resource) }
	lock, err := s.lockJournal(context.Background(), context.Background(), ".reclamation.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	revoked := make(chan struct{}, 1)
	s.syncDir = func(file *os.File) error {
		err := file.Sync()
		select {
		case revoked <- struct{}{}:
		default:
		}
		return err
	}
	done := make(chan error, 1)
	go func() { done <- s.destroyRecord(context.Background(), r.VMID) }()
	select {
	case <-revoked:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not revoke before waiting")
	}
	// Model a completed prior lifecycle and a new record with the same VM and
	// instance IDs. Creation time distinguishes the new allocation authority.
	if err := s.update(r.VMID, func(latest *stateRecord) error {
		latest.CreatedAt = r.CreatedAt.Add(time.Second)
		latest.State = "idle"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waiting reclaimer accepted replaced allocation authority")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting reclaimer did not settle")
	}
	if _, err := os.Stat(resource); err != nil {
		t.Fatalf("old reclaimer deleted new allocation resources: %v", err)
	}
	if state, err := s.State(r.VMID); err != nil || state != "idle" {
		t.Fatalf("old reclaimer revoked/retired new allocation: %s %v", state, err)
	}
	if err := s.destroyRecord(context.Background(), r.VMID); err != nil {
		t.Fatalf("new allocation was not independently reclaimable: %v", err)
	}
}
