package server

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigParsesDurationStrings(t *testing.T) {
	data, err := os.ReadFile("testdata/config1.yaml")
	require.NoError(t, err)
	content := string(data) + "\nguest:\n  startup_timeout: 90s\nleases:\n  max_lifetime: 4h\n  cleanup_grace: 3m\n  reap_interval: 15s\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	config, err := NewConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, config.Guest.StartupTimeout)
	assert.Equal(t, 4*time.Hour, config.Leases.MaxLifetime)
	assert.Equal(t, 3*time.Minute, config.Leases.CleanupGrace)
	assert.Equal(t, 15*time.Second, config.Leases.ReapInterval)
}

func TestConfigRejectsUnsafePathsAndDurations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"relative socket", func(c *Config) { c.SocketPath = "plugin.sock" }},
		{"shared runtime socket directory", func(c *Config) { c.SocketPath = "/run/plugin.sock" }},
		{"shared legacy runtime socket directory", func(c *Config) { c.SocketPath = "/var/run/plugin.sock" }},
		{"shared temporary socket directory", func(c *Config) { c.SocketPath = "/tmp/plugin.sock" }},
		{"shared persistent temporary socket directory", func(c *Config) { c.SocketPath = "/var/tmp/plugin.sock" }},
		{"unclean state path", func(c *Config) { c.StateDir = "/var/lib/../fireactions" }},
		{"relative resolver", func(c *Config) { c.Network.ResolverPath = "resolv.conf" }},
		{"invalid socket group", func(c *Config) { c.SocketGroup = "bad group" }},
		{"non-loopback metrics", func(c *Config) { c.Metrics.Address = "0.0.0.0:8081" }},
		{"zero startup", func(c *Config) { c.Guest.StartupTimeout = 0 }},
		{"negative transfer", func(c *Config) { c.Guest.MaxTransferBytes = -1 }},
		{"zero archive limit", func(c *Config) { c.Guest.MaxArchiveEntries = 0 }},
		{"zero lifetime", func(c *Config) { c.Leases.MaxLifetime = 0 }},
		{"zero cleanup", func(c *Config) { c.Leases.CleanupGrace = 0 }},
		{"zero reap", func(c *Config) { c.Leases.ReapInterval = 0 }},
		{"reap longer than grace", func(c *Config) { c.Leases.ReapInterval = 3 * time.Minute }},
		{"startup longer than lifetime", func(c *Config) { c.Guest.StartupTimeout = 4 * time.Hour }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, err := NewConfig("testdata/config1.yaml")
			require.NoError(t, err)
			tc.mutate(config)
			assert.Error(t, config.Validate())
		})
	}
}

func TestConfigArchiveEntryWireRange(t *testing.T) {
	data, err := os.ReadFile("testdata/config1.yaml")
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		entries int64
		wantErr bool
	}{
		{"int32 maximum", math.MaxInt32, false},
		{"above int32 maximum", int64(math.MaxInt32) + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := string(data) + "\nguest:\n  max_archive_entries: " + strconv.FormatInt(tc.entries, 10) + "\n"
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0600))
			config, err := NewConfig(path)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.entries, int64(config.Guest.MaxArchiveEntries))
		})
	}
}

func TestNewConfigInvalidRateLimitFixtures(t *testing.T) {
	cases := map[string]string{
		"testdata/config2.yaml": "Size",
		"testdata/config3.yaml": "RefillTime",
	}
	for path, field := range cases {
		t.Run(path, func(t *testing.T) {
			_, err := NewConfig(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "validate:")
			assert.Contains(t, err.Error(), field)
		})
	}
}

func TestConfigRequiredProfileSettings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing profiles", func(c *Config) { c.Pools = nil }},
		{"nil profile", func(c *Config) { c.Pools[0] = nil }},
		{"missing name", func(c *Config) { c.Pools[0].Name = "" }},
		{"unsafe name", func(c *Config) { c.Pools[0].Name = "../escape" }},
		{"dot segment name", func(c *Config) { c.Pools[0].Name = ".." }},
		{"empty dotted segment", func(c *Config) { c.Pools[0].Name = "ubuntu..04" }},
		{"duplicate name", func(c *Config) { c.Pools[1].Name = c.Pools[0].Name }},
		{"missing image", func(c *Config) { c.Pools[0].Image = "" }},
		{"replicas overflow int32", func(c *Config) {
			replicas := int64(math.MaxInt32) + 1
			c.Pools[0].Replicas = int(replicas)
		}},
		{"missing pull policy", func(c *Config) { c.Pools[0].ImagePullPolicy = "" }},
		{"unknown pull policy", func(c *Config) { c.Pools[0].ImagePullPolicy = "sometimes" }},
		{"missing binary", func(c *Config) { c.Pools[0].Firecracker.BinaryPath = "" }},
		{"missing kernel", func(c *Config) { c.Pools[0].Firecracker.KernelImagePath = "" }},
		{"zero CPUs", func(c *Config) { c.Pools[0].Firecracker.MachineConfig.VcpuCount = 0 }},
		{"negative CPUs", func(c *Config) { c.Pools[0].Firecracker.MachineConfig.VcpuCount = -1 }},
		{"zero memory", func(c *Config) { c.Pools[0].Firecracker.MachineConfig.MemSizeMib = 0 }},
		{"negative memory", func(c *Config) { c.Pools[0].Firecracker.MachineConfig.MemSizeMib = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config, err := NewConfig("testdata/config1.yaml")
			require.NoError(t, err)
			tc.mutate(config)
			assert.Error(t, config.Validate())
		})
	}
}

func TestConfigAcceptsSafeProfileNamesAndPullPolicies(t *testing.T) {
	for _, policy := range []string{"Always", "Never", "IfNotPresent"} {
		t.Run(policy, func(t *testing.T) {
			config, err := NewConfig("testdata/config1.yaml")
			require.NoError(t, err)
			config.Pools[0].Name = "ubuntu-24.04"
			config.Pools[0].ImagePullPolicy = policy
			assert.NoError(t, config.Validate())
		})
	}
}

func TestConfigRateLimitValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*FirecrackerTokenBucketConfig)
	}{
		{"zero size", func(b *FirecrackerTokenBucketConfig) { b.Size = 0 }},
		{"negative size", func(b *FirecrackerTokenBucketConfig) { b.Size = -1 }},
		{"zero refill", func(b *FirecrackerTokenBucketConfig) { b.RefillTime = 0 }},
		{"negative refill", func(b *FirecrackerTokenBucketConfig) { b.RefillTime = -1 }},
		{"negative burst", func(b *FirecrackerTokenBucketConfig) { v := int64(-1); b.OneTimeBurst = &v }},
	}
	for _, target := range []string{"network in", "network out", "rootfs"} {
		for _, tc := range cases {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				config, err := NewConfig("testdata/config1.yaml")
				require.NoError(t, err)
				fc := config.Pools[0].Firecracker
				bucket := fc.Rootfs.RateLimiter.Bandwidth
				switch target {
				case "network in":
					bucket = fc.NetworkInterface.InRateLimiter.Bandwidth
				case "network out":
					bucket = fc.NetworkInterface.OutRateLimiter.Bandwidth
				}
				tc.mutate(bucket)
				assert.Error(t, config.Validate())
			})
		}
	}
	config, err := NewConfig("testdata/config1.yaml")
	require.NoError(t, err)
	zero := int64(0)
	config.Pools[0].Firecracker.Rootfs.RateLimiter.Bandwidth.OneTimeBurst = &zero
	assert.NoError(t, config.Validate(), "an explicitly zero initial burst is valid")
}

func TestNewConfigRejectsUnknownFields(t *testing.T) {
	data, err := os.ReadFile("testdata/config1.yaml")
	require.NoError(t, err)
	valid := string(data)
	cases := map[string]string{
		"obsolete bind_address": valid + "\nbind_address: 127.0.0.1:8080\n",
		"github":                valid + "\ngithub:\n  app_id: 1\n",
		"basic auth enabled":    valid + "\nbasic_auth_enabled: true\n",
		"basic auth users":      valid + "\nbasic_auth_users:\n  user: password\n",
		"unknown top level":     valid + "\ndebug: false\n",
		"runner":                strings.Replace(valid, "  replicas: 1", "  replicas: 1\n  runner:\n    name: obsolete", 1),
		"shutdown on exit":      strings.Replace(valid, "  replicas: 1", "  replicas: 1\n  shutdown_on_exit: true", 1),
		"metadata":              strings.Replace(valid, "  firecracker:", "  firecracker:\n    metadata:\n      key: value", 1),
		"unknown nested field":  strings.Replace(valid, "      vcpu_count: 2", "      vcpu_count: 2\n      unknown: true", 1),
		"unknown guest field":   valid + "\nguest:\n  unrecognized: true\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0600))
			_, err := NewConfig(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not found")
		})
	}
}
