package server

import (
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

// Config is the configuration for the Client.
type Config struct {
	SocketPath  string            `yaml:"socket_path" validate:"required"`
	SocketGroup string            `yaml:"socket_group" validate:"required"`
	StateDir    string            `yaml:"state_dir" validate:"required"`
	Containerd  *ContainerdConfig `yaml:"containerd" validate:"required"`
	Metrics     *MetricsConfig    `yaml:"metrics"`
	Guest       GuestConfig       `yaml:"guest"`
	Leases      LeaseConfig       `yaml:"leases"`
	Network     NetworkConfig     `yaml:"network"`
	Pools       []*PoolConfig     `yaml:"pools" validate:"required,min=1,dive,required"`
	LogLevel    string            `yaml:"log_level" validate:"required,oneof=debug info warn error fatal panic trace"`

	path string
}

type GuestConfig struct {
	StartupTimeout    time.Duration `yaml:"startup_timeout"`
	MaxTransferBytes  int64         `yaml:"max_transfer_bytes"`
	MaxArchiveEntries int           `yaml:"max_archive_entries"`
}

type LeaseConfig struct {
	MaxLifetime  time.Duration `yaml:"max_lifetime"`
	CleanupGrace time.Duration `yaml:"cleanup_grace"`
	ReapInterval time.Duration `yaml:"reap_interval"`
}

type NetworkConfig struct {
	ResolverPath string `yaml:"resolver_path"`
}

type ContainerdConfig struct {
	Address   string `yaml:"address" validate:"required"`
	Namespace string `yaml:"namespace" validate:"required"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled" validate:""`
	Address string `yaml:"address" validate:"required_if=enabled true,hostname_port"`
}

type FirecrackerConfig struct {
	BinaryPath       string                             `yaml:"binary_path" validate:"required"`
	KernelImagePath  string                             `yaml:"kernel_image_path" validate:"required"`
	KernelArgs       string                             `yaml:"kernel_args"`
	MachineConfig    FirecrackerMachineConfig           `yaml:"machine_config"`
	NetworkInterface *FirecrackerNetworkInterfaceConfig `yaml:"network_interface"`
	Rootfs           *FirecrackerRootfsConfig           `yaml:"rootfs"`
}

type FirecrackerMachineConfig struct {
	VcpuCount  int64 `yaml:"vcpu_count" validate:"gt=0"`
	MemSizeMib int64 `yaml:"mem_size_mib" validate:"gt=0"`
}

// FirecrackerNetworkInterfaceConfig configures the MicroVM's network interface.
// Rate limiters are optional, a nil limiter leaves that direction unlimited.
type FirecrackerNetworkInterfaceConfig struct {
	InRateLimiter  *FirecrackerRateLimiterConfig `yaml:"in_rate_limiter"`
	OutRateLimiter *FirecrackerRateLimiterConfig `yaml:"out_rate_limiter"`
}

// FirecrackerRootfsConfig configures the MicroVM's root block device. The rate
// limiter is optional, a nil limiter leaves the device unlimited.
type FirecrackerRootfsConfig struct {
	RateLimiter *FirecrackerRateLimiterConfig `yaml:"rate_limiter"`
}

// FirecrackerRateLimiterConfig defines an IO rate limiter with independent
// bytes/s and ops/s limits. A nil token bucket leaves that limit unlimited.
type FirecrackerRateLimiterConfig struct {
	Bandwidth *FirecrackerTokenBucketConfig `yaml:"bandwidth"`
	Ops       *FirecrackerTokenBucketConfig `yaml:"ops"`
}

// FirecrackerTokenBucketConfig defines a token bucket with a maximum capacity
// (Size), an optional initial burst size (OneTimeBurst) and the interval in
// milliseconds it takes to refill the bucket (RefillTime). The resulting rate
// is Size / RefillTime.
type FirecrackerTokenBucketConfig struct {
	Size         int64  `yaml:"size" validate:"required,gt=0"`
	OneTimeBurst *int64 `yaml:"one_time_burst" validate:"omitempty,gte=0"`
	RefillTime   int64  `yaml:"refill_time" validate:"required,gt=0"`
}

// toSDK converts the rate limiter configuration into its Firecracker SDK
// representation. Returns nil if the rate limiter isn't configured.
func (c *FirecrackerRateLimiterConfig) toSDK() *models.RateLimiter {
	if c == nil {
		return nil
	}

	return &models.RateLimiter{Bandwidth: c.Bandwidth.toSDK(), Ops: c.Ops.toSDK()}
}

// toSDK converts the token bucket configuration into its Firecracker SDK
// representation. Returns nil if the token bucket isn't configured.
func (c *FirecrackerTokenBucketConfig) toSDK() *models.TokenBucket {
	if c == nil {
		return nil
	}

	return &models.TokenBucket{
		Size:         firecracker.Int64(c.Size),
		RefillTime:   firecracker.Int64(c.RefillTime),
		OneTimeBurst: c.OneTimeBurst,
	}
}

// DefaultConfig creates a new Config with default values.
func DefaultConfig() *Config {
	return &Config{
		SocketPath:  "/run/fireactions/plugin.sock",
		SocketGroup: "fireactions",
		StateDir:    "/var/lib/fireactions",
		Containerd:  &ContainerdConfig{Address: "/run/containerd/containerd.sock", Namespace: "fireactions"},
		Metrics:     &MetricsConfig{Enabled: true, Address: "127.0.0.1:8081"},
		Guest: GuestConfig{
			StartupTimeout:    2 * time.Minute,
			MaxTransferBytes:  10 * 1024 * 1024 * 1024,
			MaxArchiveEntries: 100000,
		},
		Leases:   LeaseConfig{MaxLifetime: 3*time.Hour + 2*time.Minute, CleanupGrace: 2 * time.Minute, ReapInterval: 10 * time.Second},
		Network:  NetworkConfig{ResolverPath: "/etc/resolv.conf"},
		Pools:    []*PoolConfig{},
		LogLevel: "debug",
	}
}

// NewConfigFromFile creates a new Config from a file.
func NewConfig(path string) (*Config, error) {
	c := DefaultConfig()
	c.path = path

	err := c.Load()
	if err != nil {
		return nil, err
	}

	err = c.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}

	return c, nil
}

// LoadFromFile loads the configuration from a file.
func (c *Config) Load() error {
	file, err := os.OpenFile(c.path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}

	defer func() {
		_ = file.Close()
	}()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(c); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("configuration must contain exactly one YAML document")
	}
	for _, pool := range c.Pools {
		if pool != nil && pool.DefaultUser == "" {
			pool.DefaultUser = "ci"
		}
	}
	return nil
}

var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
var socketGroupPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var userNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Validate validates the configuration.
func (c *Config) Validate() error {
	if err := validator.New().Struct(c); err != nil {
		return err
	}
	for name, path := range map[string]string{
		"socket_path": c.SocketPath, "state_dir": c.StateDir, "network.resolver_path": c.Network.ResolverPath,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "\x00") {
			return fmt.Errorf("%s must be an absolute, clean path", name)
		}
	}
	if c.SocketPath == "/" || c.StateDir == "/" || c.Network.ResolverPath == "/" {
		return fmt.Errorf("socket_path, state_dir, and network.resolver_path must name specific paths")
	}
	if len(c.SocketPath) > 107 || filepath.Dir(c.SocketPath) == "/" {
		return fmt.Errorf("socket_path is not a usable Unix socket path")
	}
	if sharedSocketDirectory(filepath.Dir(c.SocketPath)) {
		return fmt.Errorf("socket_path must use a dedicated directory")
	}
	if !socketGroupPattern.MatchString(c.SocketGroup) {
		return fmt.Errorf("socket_group must be a valid Unix group name")
	}
	if c.Metrics != nil && c.Metrics.Enabled {
		host, _, err := net.SplitHostPort(c.Metrics.Address)
		if err != nil {
			return fmt.Errorf("metrics.address must be a loopback host and port")
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("metrics.address must be loopback-only")
		}
	}
	if c.Guest.StartupTimeout <= 0 || c.Guest.MaxTransferBytes <= 0 || c.Guest.MaxArchiveEntries <= 0 {
		return fmt.Errorf("guest limits and startup_timeout must be positive")
	}
	if c.Guest.MaxArchiveEntries > math.MaxInt32 {
		return fmt.Errorf("guest.max_archive_entries must not exceed %d", math.MaxInt32)
	}
	if c.Leases.MaxLifetime <= 0 || c.Leases.CleanupGrace <= 0 || c.Leases.ReapInterval <= 0 {
		return fmt.Errorf("lease durations must be positive")
	}
	if c.Leases.ReapInterval > c.Leases.CleanupGrace || c.Guest.StartupTimeout > c.Leases.MaxLifetime {
		return fmt.Errorf("reap_interval must not exceed cleanup_grace and startup_timeout must not exceed max_lifetime")
	}

	names := make(map[string]struct{}, len(c.Pools))
	for _, pool := range c.Pools {
		if pool.Replicas > math.MaxInt32 {
			return fmt.Errorf("profile %q replicas must fit int32", pool.Name)
		}
		if !profileNamePattern.MatchString(pool.Name) {
			return fmt.Errorf("invalid profile name %q: use only letters, digits, underscores and hyphens", pool.Name)
		}
		if _, exists := names[pool.Name]; exists {
			return fmt.Errorf("duplicate profile name %q", pool.Name)
		}
		if !userNamePattern.MatchString(pool.DefaultUser) {
			return fmt.Errorf("profile %q default_user must be a valid guest user name", pool.Name)
		}
		names[pool.Name] = struct{}{}
	}
	return nil
}
