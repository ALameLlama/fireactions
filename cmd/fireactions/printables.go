package main

import (
	"time"

	serverv1 "github.com/ALameLlama/fireactions/proto/server/v1"
	"github.com/docker/go-units"
)

// printablePool wraps a proto Pool for printing
type printablePool struct {
	Pools []*serverv1.Pool
}

func (p *printablePool) Cols() []string {
	return []string{"Name", "Current", "Desired", "Image", "State"}
}

func (p *printablePool) ColsMap() map[string]string {
	return map[string]string{
		"Name":    "Name",
		"Current": "Current",
		"Desired": "Desired",
		"Image":   "Image",
		"State":   "State",
	}
}

func (p *printablePool) KV() []map[string]interface{} {
	kv := make([]map[string]interface{}, 0, len(p.Pools))
	for _, pool := range p.Pools {
		state := "Active"
		if pool.State == serverv1.PoolState_POOL_STATE_PAUSED {
			state = "Paused"
		}
		kv = append(kv, map[string]interface{}{
			"Name":    pool.Name,
			"Current": pool.CurrentReplicas,
			"Desired": pool.DesiredReplicas,
			"Image":   pool.Image,
			"State":   state,
		})
	}
	return kv
}

// printableMachine wraps a slice of proto Machines for printing
type printableMachine struct {
	Machines []*serverv1.Machine
}

func (m *printableMachine) Cols() []string {
	return []string{"Pool", "ID", "ADDR", "State", "Agent Version", "Environment ID", "Created"}
}

func (m *printableMachine) ColsMap() map[string]string {
	return map[string]string{
		"Pool":           "Pool",
		"ID":             "ID",
		"ADDR":           "ADDR",
		"State":          "State",
		"Agent Version":  "Agent Version",
		"Environment ID": "Environment ID",
		"Created":        "Created",
	}
}

func (m *printableMachine) KV() []map[string]interface{} {
	kv := make([]map[string]interface{}, 0, len(m.Machines))
	for _, vm := range m.Machines {
		agentVersion := vm.AgentVersion
		if agentVersion == "" {
			agentVersion = "Unknown"
		}

		createdAt := vm.CreatedAt.AsTime()

		kv = append(kv, map[string]interface{}{
			"Pool":           vm.Pool,
			"ID":             vm.ID,
			"ADDR":           vm.Addr,
			"State":          vm.State,
			"Agent Version":  agentVersion,
			"Environment ID": vm.EnvironmentId,
			"Created":        units.HumanDuration(time.Since(createdAt)),
		})
	}
	return kv
}

// printableImage wraps a slice of proto Images for printing
type printableImage struct {
	Images []*serverv1.Image
}

func (i *printableImage) Cols() []string {
	return []string{"Name", "Size", "Created"}
}

func (i *printableImage) ColsMap() map[string]string {
	return map[string]string{
		"Name":    "Name",
		"Size":    "Size",
		"Created": "Created",
	}
}

func (i *printableImage) KV() []map[string]interface{} {
	kv := make([]map[string]interface{}, 0, len(i.Images))
	for _, img := range i.Images {
		createdAt := img.CreatedAt.AsTime()

		kv = append(kv, map[string]interface{}{
			"Name":    img.Name,
			"Size":    units.HumanSize(float64(img.GetSize())),
			"Created": units.HumanDuration(time.Since(createdAt)),
		})
	}
	return kv
}
