package guest

import (
	"context"
	"errors"

	"github.com/hostinger/fireactions/internal/executor"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// translateError classifies private RPC failures without exposing wire types or
// echoing command/environment data into caller-visible diagnostics.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	var kind executor.Kind
	switch status.Code(err) {
	case codes.InvalidArgument:
		kind = executor.InvalidArgument
	case codes.NotFound:
		kind = executor.NotFound
	case codes.FailedPrecondition:
		kind = executor.FailedPrecondition
	case codes.ResourceExhausted:
		kind = executor.ResourceExhausted
	case codes.Unimplemented:
		kind = executor.Unimplemented
	case codes.Unavailable:
		kind = executor.Unavailable
	default:
		kind = executor.Internal
	}
	return &executor.Error{Kind: kind, Message: status.Convert(err).Message(), Cause: err}
}
