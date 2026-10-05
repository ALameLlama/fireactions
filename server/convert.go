package server

import (
	"context"

	"github.com/containerd/containerd"
	serverv1 "github.com/hostinger/fireactions/proto/server/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// convertPoolToProto converts a Pool to its protobuf representation.
func convertPoolToProto(ctx context.Context, pool *Pool) *serverv1.Pool {
	state := serverv1.PoolState_POOL_STATE_ACTIVE
	if !pool.IsActive() {
		state = serverv1.PoolState_POOL_STATE_PAUSED
	}

	return &serverv1.Pool{
		Name:            pool.config.Name,
		Replicas:        int32(pool.GetReplicas()),
		CurrentReplicas: int32(pool.GetCurrentSize()),
		DesiredReplicas: int32(pool.GetReplicas()),
		Image:           pool.config.Image,
		State:           state,
	}
}

func convertMachineToProto(ctx context.Context, machine *Machine) *serverv1.Machine {
	metadata := machine.Metadata()
	return &serverv1.Machine{
		ID:            machine.Name,
		Pool:          machine.Pool,
		Addr:          machine.GetAddr(),
		CreatedAt:     timestamppb.New(machine.CreatedAt),
		State:         metadata.State,
		AgentVersion:  metadata.AgentVersion,
		EnvironmentId: metadata.EnvironmentID,
	}
}

// convertImageToProto converts a containerd Image to its protobuf representation.
func convertImageToProto(ctx context.Context, img containerd.Image) *serverv1.Image {
	size, _ := img.Size(ctx)
	createdAt := img.Metadata().CreatedAt

	i := &serverv1.Image{
		Name:      img.Name(),
		Size:      size,
		CreatedAt: timestamppb.New(createdAt),
	}

	return i
}
