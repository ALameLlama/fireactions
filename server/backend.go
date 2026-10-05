package server

import (
	"context"
	"fmt"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
)

var _ executor.Backend = (*Server)(nil)

// Acquire creates a dedicated Firecracker VM for the configured image profile.
// Stage 5 deliberately does not claim idle replicas: every environment gets a
// fresh VM and ownership remains with its pool until cleanup completes.
func (s *Server) Acquire(ctx context.Context, profile string, absoluteExpiry time.Time) (executor.VM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool, err := s.findPool(profile)
	if err != nil {
		return nil, executor.NewError(executor.InvalidArgument, fmt.Sprintf("unknown image profile %q", profile), err)
	}
	if !pool.IsActive() {
		return nil, executor.NewError(executor.FailedPrecondition, fmt.Sprintf("image profile %q is paused", profile), nil)
	}
	return pool.acquire(ctx, absoluteExpiry)
}
