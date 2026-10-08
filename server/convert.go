package server

import (
	"context"

	"github.com/ALameLlama/fireactions/internal/executor"
	serverv1 "github.com/ALameLlama/fireactions/proto/server/v1"
	"github.com/containerd/containerd"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// convertPoolToProto converts a Pool to its protobuf representation.
func convertPoolToProto(ctx context.Context, pool *Pool) *serverv1.Pool {
	state := serverv1.PoolState_POOL_STATE_ACTIVE
	if !pool.IsActive() {
		state = serverv1.PoolState_POOL_STATE_PAUSED
	}

	replicas := int32(pool.GetReplicas())
	return &serverv1.Pool{
		Name:            pool.config.Name,
		Replicas:        replicas,
		CurrentReplicas: int32(pool.GetCurrentSize()),
		DesiredReplicas: replicas,
		Image:           pool.config.Image,
		State:           state,
	}
}

func convertMachineToProto(ctx context.Context, machine *Machine, managers ...*executor.Manager) *serverv1.Machine {
	metadata := machine.Metadata()
	if len(managers) != 0 && managers[0] != nil {
		if id, removing := managers[0].EnvironmentForVM(machine.Name); id != "" {
			metadata.EnvironmentID = id
			if removing {
				metadata.State = "removing"
			}
		}
	}
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
