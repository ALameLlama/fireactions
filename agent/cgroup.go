package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type cgroupScopes struct {
	root   string
	mu     sync.Mutex
	scopes map[string]string
}

func newCgroupScopes() (*cgroupScopes, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, fmt.Errorf("read cgroup membership: %w", err)
	}
	var rel string
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(line, "0::")
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("unified cgroup v2 membership is unavailable")
	}
	rel = strings.TrimPrefix(rel, "/")
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("invalid cgroup v2 membership")
	}
	root := filepath.Join("/sys/fs/cgroup", clean)
	if err := validateCgroup(root); err != nil {
		return nil, err
	}
	scopes := &cgroupScopes{root: root, scopes: make(map[string]string)}
	probe, _, err := scopes.create("readiness-probe")
	if err != nil {
		return nil, fmt.Errorf("cgroup v2 delegation is unavailable: %w", err)
	}
	_ = probe.Close()
	if err := scopes.kill("readiness-probe"); err != nil {
		return nil, err
	}
	if err := scopes.remove("readiness-probe"); err != nil {
		return nil, fmt.Errorf("remove cgroup readiness probe: %w", err)
	}
	return scopes, nil
}

func validateCgroup(root string) error {
	for _, name := range []string{"cgroup.procs", "cgroup.kill"} {
		f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("cgroup v2 %s is unavailable: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close cgroup %s: %w", name, err)
		}
	}
	return nil
}

func (s *cgroupScopes) create(id string) (*os.File, string, error) {
	path := filepath.Join(s.root, "fireactions-"+id)
	if err := os.Mkdir(path, 0750); err != nil {
		return nil, "", fmt.Errorf("create process cgroup: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, "", fmt.Errorf("open process cgroup: %w", err)
	}
	if err := validateCgroup(path); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, "", err
	}
	s.mu.Lock()
	s.scopes[id] = path
	s.mu.Unlock()
	return f, path, nil
}
func (s *cgroupScopes) lookup(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.scopes[id]
	return p, ok
}
func (s *cgroupScopes) remove(id string) error {
	p, ok := s.lookup(id)
	if !ok {
		return nil
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	delete(s.scopes, id)
	s.mu.Unlock()
	return nil
}
func (s *cgroupScopes) kill(id string) error {
	p, ok := s.lookup(id)
	if !ok {
		return nil
	}
	if err := writeCgroup(filepath.Join(p, "cgroup.kill"), []byte("1")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kill process cgroup: %w", err)
	}
	return nil
}
func (s *cgroupScopes) killAll() error {
	s.mu.Lock()
	paths := make([]string, 0, len(s.scopes))
	for _, p := range s.scopes {
		paths = append(paths, p)
	}
	s.mu.Unlock()
	var errs []error
	for _, p := range paths {
		if err := writeCgroup(filepath.Join(p, "cgroup.kill"), []byte("1")); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func (s *cgroupScopes) waitEmpty(id string, timeout time.Duration) error {
	p, ok := s.lookup(id)
	if !ok {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(p, "cgroup.events"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "populated 0") {
			return nil
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return errors.New("process cgroup remained populated after kill")
		}
	}
}
func writeCgroup(path string, value []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(value)
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}
func signalProcessGroup(pid int, sig unix.Signal) error {
	err := unix.Kill(-pid, sig)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}
