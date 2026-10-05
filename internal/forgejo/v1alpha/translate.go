package v1alpha

import (
	"context"
	"errors"

	"github.com/hostinger/fireactions/internal/executor"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func grpcError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "operation canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "operation deadline exceeded")
	}
	var launch *executor.LaunchError
	if errors.As(err, &launch) {
		return status.Error(codes.Internal, "guest process could not be started")
	}
	switch executor.KindOf(err) {
	case executor.InvalidArgument:
		return status.Error(codes.InvalidArgument, err.Error())
	case executor.NotFound:
		return status.Error(codes.NotFound, err.Error())
	case executor.FailedPrecondition:
		return status.Error(codes.FailedPrecondition, err.Error())
	case executor.Unimplemented:
		return status.Error(codes.Unimplemented, err.Error())
	case executor.ResourceExhausted:
		return status.Error(codes.ResourceExhausted, err.Error())
	case executor.Cancelled:
		return status.Error(codes.Canceled, "operation canceled")
	case executor.DeadlineExceeded:
		return status.Error(codes.DeadlineExceeded, "operation deadline exceeded")
	case executor.Unavailable:
		return status.Error(codes.Unavailable, "execution backend unavailable")
	default:
		return status.Error(codes.Internal, "execution backend failure")
	}
}
