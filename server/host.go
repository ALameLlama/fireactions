package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containernetworking/cni/libcni"
)

// CheckHost verifies the configured host prerequisites without starting a server,
// allocating a VM, or changing host state.
func CheckHost(ctx context.Context, config *Config) error {
	if config == nil {
		return fmt.Errorf("configuration is nil")
	}
	if config.Containerd == nil {
		return fmt.Errorf("containerd configuration is missing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	kvm, err := os.Stat("/dev/kvm")
	if err != nil {
		return fmt.Errorf("KVM device /dev/kvm: %w", err)
	}
	if kvm.Mode()&os.ModeDevice == 0 || kvm.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("/dev/kvm is not a character device")
	}
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("KVM device /dev/kvm is not usable: %w", err)
	}
	_ = file.Close()

	for _, pool := range config.Pools {
		if pool == nil || pool.Firecracker == nil {
			return fmt.Errorf("profile Firecracker configuration is missing")
		}
		vmm, err := executablePath(pool.Firecracker.BinaryPath)
		if err != nil {
			return fmt.Errorf("profile %q Firecracker binary: %w", pool.Name, err)
		}
		if info, err := os.Stat(vmm); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("profile %q Firecracker binary %q is not an executable regular file", pool.Name, vmm)
		}
		kernel := pool.Firecracker.KernelImagePath
		if err := checkKernelImage(kernel); err != nil {
			return fmt.Errorf("profile %q kernel image %q: %w", pool.Name, kernel, err)
		}
	}

	resolver, err := os.Open(config.Network.ResolverPath)
	if err != nil {
		return fmt.Errorf("DNS resolver %q: %w", config.Network.ResolverPath, err)
	}
	_ = resolver.Close()
	if len(config.Pools) != 0 {
		conf, err := libcni.LoadConfList("/etc/cni/net.d", "fireactions")
		if err != nil {
			return fmt.Errorf("loading Fireactions CNI network: %w", err)
		}
		if _, err := effectiveCNIConfig(conf.Bytes, config.Network.ResolverPath); err != nil {
			return fmt.Errorf("validating Fireactions CNI and DNS resolver: %w", err)
		}
		if err := checkCNIExecutables(conf.Bytes); err != nil {
			return err
		}
	}

	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := containerd.New(config.Containerd.Address,
		containerd.WithTimeout(5*time.Second),
		containerd.WithDefaultNamespace(config.Containerd.Namespace))
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	defer client.Close()
	if _, err := client.Version(connectCtx); err != nil {
		return fmt.Errorf("connecting to configured containerd: %w", err)
	}
	pluginResponse, err := client.IntrospectionService().Plugins(connectCtx, []string{"type==io.containerd.snapshotter.v1, id==devmapper"})
	if err != nil {
		return fmt.Errorf("querying containerd devmapper snapshotter: %w", err)
	}
	plugins := pluginResponse.Plugins
	found := false
	for _, plugin := range plugins {
		if plugin.ID == "devmapper" && plugin.Type == "io.containerd.snapshotter.v1" {
			found = true
			if plugin.InitErr != nil {
				return fmt.Errorf("containerd devmapper snapshotter is not ready: %s", plugin.InitErr.Message)
			}
		}
	}
	if !found {
		return fmt.Errorf("configured containerd has no ready devmapper snapshotter")
	}
	return nil
}

func checkKernelImage(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("is not readable: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened kernel image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("is not a regular file")
	}
	return nil
}

func executablePath(path string) (string, error) {
	if strings.ContainsRune(path, filepath.Separator) {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("binary path %q must be absolute or available in PATH", path)
		}
		return filepath.Clean(path), nil
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func checkCNIExecutables(raw []byte) error {
	var config struct {
		Plugins []struct {
			Type string `json:"type"`
			IPAM struct {
				Type string `json:"type"`
			} `json:"ipam"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("decoding Fireactions CNI plugin list: %w", err)
	}
	for _, plugin := range config.Plugins {
		for _, name := range []string{plugin.Type, plugin.IPAM.Type} {
			if name == "" {
				continue
			}
			if filepath.Base(name) != name || name == "." {
				return fmt.Errorf("invalid CNI plugin name %q", name)
			}
			path := filepath.Join("/opt/cni/bin", name)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return fmt.Errorf("CNI plugin %q is not an executable regular file at %s", name, path)
			}
		}
	}
	return nil
}
