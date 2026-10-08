// Package executor owns disposable environments independently of host and guest
// wire protocols.
package executor

import (
	"context"
	"io"
	"time"
)

const (
	WorkspaceRoot                  = "/workspace"
	DefaultPath                    = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	DefaultUser                    = "ci"
	DefaultMaxTransferBytes  int64 = 10 * 1024 * 1024 * 1024
	DefaultMaxArchiveEntries       = 100000
)

type CreateSpec struct {
	Profile  string
	Lifetime time.Duration
}

type Layout struct {
	Root, Act, ToolCache, Temp, OS, Arch, DefaultPath string
}

// DefaultLayout returns the fixed guest layout for a supported architecture.
func DefaultLayout(architecture string) (Layout, error) {
	var arch string
	switch architecture {
	case "amd64", "X64":
		arch = "X64"
	case "arm64", "ARM64":
		arch = "ARM64"
	default:
		return Layout{}, NewError(InvalidArgument, "unsupported guest architecture", nil)
	}
	return Layout{
		Root:        WorkspaceRoot,
		Act:         WorkspaceRoot + "/.fireactions/act",
		ToolCache:   WorkspaceRoot + "/.fireactions/toolcache",
		Temp:        WorkspaceRoot + "/.fireactions/tmp",
		OS:          "Linux",
		Arch:        arch,
		DefaultPath: DefaultPath,
	}, nil
}

type ExecSpec struct {
	Command       []string
	Env           map[string]string
	User, Workdir string
	// Workspace is a trusted declaration of the runner checkout to prepare.
	// It is distinct from Workdir, which is passed unchanged to the guest.
	Workspace string
}

type ExecResult struct {
	ExitCode int32
}

type ReadySpec struct {
	Directories       []string
	DefaultUser       string
	MaxTransferBytes  int64
	MaxArchiveEntries int
}

type Guest interface {
	Ready(context.Context, ReadySpec) (string, error)
	CopyIn(context.Context, string, io.Reader) error
	Exec(context.Context, ExecSpec, io.Writer, io.Writer) (ExecResult, error)
	CopyOut(context.Context, string, io.Writer) error
	// An empty handle kills all guest execution scopes.
	Kill(context.Context, string) error
	Close() error
}

type VMInfo struct {
	Layout      Layout
	ImageEnv    map[string]string
	DefaultUser string
}

type VM interface {
	ID() string
	Info() VMInfo
	Guest() Guest
	Destroy(context.Context) error
}

type Backend interface {
	// Acquire must persist the hard expiry before publishing VM ownership.
	Acquire(context.Context, string, time.Time) (VM, error)
}

type EnvironmentInfo struct {
	ID     string
	Layout Layout
}

// Options configures environment ownership. Zero durations select defaults:
// MaxLifetime is 3h2m, CleanupTimeout is 30s, and CleanupGrace is 2m.
// CleanupTimeout must not exceed 30s; attempts are also bounded by CleanupGrace.
// CleanupGrace bounds cleanup windows, never extends execution lifetime.
// ReadySpec supplies trusted profile-specific guest limits and identity. The
// manager always supplies the canonical layout directories; zero-valued fields
// in the returned spec inherit defaults from the VM and manager.
type Options struct {
	MaxLifetime    time.Duration
	CleanupTimeout time.Duration
	CleanupGrace   time.Duration
	ReadySpec      func(profile string) (ReadySpec, error)
	Observer       *Observer
}

// Operation identifies a public environment operation reported to an Observer.
type Operation string

const (
	OperationCreate  Operation = "create"
	OperationStart   Operation = "start"
	OperationCopyIn  Operation = "copy_in"
	OperationExec    Operation = "exec"
	OperationCopyOut Operation = "copy_out"
	OperationRemove  Operation = "remove"
)

// Outcome is the result category reported for a public operation.
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeFailure   Outcome = "failure"
	OutcomeCancelled Outcome = "cancelled"
)

// Observer receives protocol-independent ownership and operation events.
// Nil callbacks are ignored. Callbacks should be fast and must be safe for
// concurrent calls.
type Observer struct {
	Operation      func(profile string, operation Operation, outcome Outcome)
	ActiveEntries  func(profile string, delta int)
	CleanupFailure func(profile string)
	// TTLExpiration reports a deadline-caused removal transition once, not retries.
	TTLExpiration func(profile string)
}
