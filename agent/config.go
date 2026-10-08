package agent

import (
	"path/filepath"

	"github.com/go-playground/validator/v10"
)

const (
	DefaultWorkspaceRoot     = "/workspace"
	DefaultGuestUser         = "ci"
	DefaultMaxTransferBytes  = int64(10 << 30)
	DefaultMaxArchiveEntries = 100000
)

type Config struct {
	Port              uint32 `validate:"required"`
	LogLevel          string `validate:"required,oneof=debug info warn error fatal panic trace"`
	WorkspaceRoot     string `validate:"required"`
	DefaultUser       string `validate:"required"`
	MaxTransferBytes  int64  `validate:"required,min=1"`
	MaxArchiveEntries int    `validate:"required,min=1"`
}

func (c Config) withDefaults() Config {
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = DefaultWorkspaceRoot
	}
	if c.DefaultUser == "" {
		c.DefaultUser = DefaultGuestUser
	}
	if c.MaxTransferBytes == 0 {
		c.MaxTransferBytes = DefaultMaxTransferBytes
	}
	if c.MaxArchiveEntries == 0 {
		c.MaxArchiveEntries = DefaultMaxArchiveEntries
	}
	if c.WorkspaceRoot != "" {
		if absolute, err := filepath.Abs(c.WorkspaceRoot); err == nil {
			c.WorkspaceRoot = absolute
		}
	}
	return c
}

func (c Config) Validate() error {
	return validator.New().Struct(c.withDefaults())
}
