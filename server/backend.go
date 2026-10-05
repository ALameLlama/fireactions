package server

import (
	"context"
	"fmt"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
)

var _ executor.Backend = (*Server)(nil)

// Acquire claims a ready idle Firecracker VM when available, or provisions a
// dedicated claimed VM when the pool is active and has no warm replica.
func (s *Server) Acquire(ctx context.Context, profile string, absoluteExpiry time.Time) (executor.VM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool, err := s.findPool(profile)
	if err != nil {
		return nil, executor.NewError(executor.InvalidArgument, fmt.Sprintf("unknown image profile %q", profile), err)
	}
	started := time.Now()
	defer func() {
		metricVMAcquisition.WithLabelValues(pool.config.Name).Observe(time.Since(started).Seconds())
	}()
	return pool.acquire(ctx, absoluteExpiry)
}
