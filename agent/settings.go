package agent

import (
	"errors"
	"slices"

	"github.com/hostinger/fireactions/internal/guestfs"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (a *Agent) applyReady(req *agentv1.ReadyRequest) error {
	settings, err := a.parseReady(req)
	if err != nil {
		return err
	}
	if err := a.prepareDirectories(settings); err != nil {
		return err
	}
	a.publishReady(settings)
	return nil
}

func (a *Agent) parseReady(req *agentv1.ReadyRequest) (readySettings, error) {
	userSpec := req.GetDefaultUser()
	if userSpec == "" {
		userSpec = a.cfg.DefaultUser
	}
	identity, err := resolveIdentity(userSpec)
	if err != nil {
		return readySettings{}, status.Error(codes.InvalidArgument, "unknown or invalid guest identity")
	}

	maxBytes := req.GetMaxTransferBytes()
	if maxBytes == 0 {
		maxBytes = a.cfg.MaxTransferBytes
	}
	maxEntries := int(req.GetMaxArchiveEntries())
	if maxEntries == 0 {
		maxEntries = a.cfg.MaxArchiveEntries
	}
	if maxBytes < 1 || maxEntries < 1 {
		return readySettings{}, status.Error(codes.InvalidArgument, "transfer limits must be positive")
	}

	directories := req.GetDirectories()
	if len(directories) == 0 {
		directories = []string{"/workspace"}
	}
	cleanDirectories := make([]string, 0, len(directories))
	seen := make(map[string]struct{}, len(directories))
	for _, directory := range directories {
		clean, resolveErr := a.workspacePath(directory)
		if resolveErr != nil {
			return readySettings{}, status.Error(codes.InvalidArgument, "invalid workspace directory")
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		cleanDirectories = append(cleanDirectories, clean)
	}
	slices.Sort(cleanDirectories)

	return readySettings{
		identity:          cloneIdentity(identity),
		directories:       cleanDirectories,
		maxTransferBytes:  maxBytes,
		maxArchiveEntries: maxEntries,
		ready:             true,
	}, nil
}

func (a *Agent) prepareDirectories(settings readySettings) error {
	for _, directory := range settings.directories {
		if err := a.fs.EnsureDirectory(directory, settings.identity.UID, settings.identity.GID, 0755); err != nil {
			if errors.Is(err, guestfs.ErrInvalidPath) {
				return status.Error(codes.InvalidArgument, "invalid workspace directory")
			}
			return status.Error(codes.Internal, "could not prepare workspace directories")
		}
	}
	return nil
}

func (a *Agent) publishReady(settings readySettings) {
	a.readyMu.Lock()
	if !equalReadySettings(a.readySettings, settings) {
		a.readySettings = settings
	}
	a.readyMu.Unlock()
}

func cloneIdentity(identity Identity) Identity {
	identity.Groups = append([]uint32(nil), identity.Groups...)
	return identity
}

func (a *Agent) workspacePath(name string) (string, error) {
	return a.fs.ResolvePath(name)
}

func (a *Agent) currentIdentity() Identity {
	a.readyMu.RLock()
	defer a.readyMu.RUnlock()
	return cloneIdentity(a.readySettings.identity)
}

func (a *Agent) transferLimits() (int64, int) {
	a.readyMu.RLock()
	defer a.readyMu.RUnlock()
	if a.readySettings.ready {
		return a.readySettings.maxTransferBytes, a.readySettings.maxArchiveEntries
	}
	return a.cfg.MaxTransferBytes, a.cfg.MaxArchiveEntries
}

func (a *Agent) isReady() bool {
	a.readyMu.RLock()
	defer a.readyMu.RUnlock()
	return a.readySettings.ready
}

func equalReadySettings(left, right readySettings) bool {
	return left.ready && right.ready && left.identity.UID == right.identity.UID &&
		left.identity.GID == right.identity.GID && left.identity.Username == right.identity.Username &&
		left.identity.Home == right.identity.Home && left.identity.Shell == right.identity.Shell &&
		slices.Equal(left.identity.Groups, right.identity.Groups) &&
		slices.Equal(left.directories, right.directories) &&
		left.maxTransferBytes == right.maxTransferBytes &&
		left.maxArchiveEntries == right.maxArchiveEntries
}
