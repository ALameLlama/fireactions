package agent

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/hostinger/fireactions"
	"github.com/hostinger/fireactions/agent/tail"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (a *Agent) Ready(ctx context.Context, req *agentv1.ReadyRequest) (*agentv1.ReadyResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.processes == nil {
		return nil, status.Error(codes.FailedPrecondition, "process manager unavailable")
	}

	settings, err := a.parseReady(req)
	if err != nil {
		return nil, err
	}

	a.processes.mu.Lock()
	defer a.processes.mu.Unlock()
	if a.processes.closing {
		return nil, status.Error(codes.FailedPrecondition, "agent is shutting down")
	}
	a.readyMu.Lock()
	defer a.readyMu.Unlock()
	if !equalReadySettings(a.readySettings, settings) && len(a.processes.scopes) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "cannot change settings while process scopes exist")
	}
	if err := a.validateProcessReadinessLocked(); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "guest process cgroups are not ready: %v", err)
	}
	if err := a.prepareDirectories(settings); err != nil {
		return nil, err
	}
	a.readySettings = settings
	return &agentv1.ReadyResponse{Version: fireactions.Version}, nil
}

func (a *Agent) Exec(req *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	if a.processes == nil {
		return status.Error(codes.FailedPrecondition, "process manager unavailable")
	}
	return a.processes.exec(stream, req)
}

func (a *Agent) GetLogs(req *agentv1.GetLogsRequest, stream agentv1.AgentService_GetLogsServer) error {
	ctx := stream.Context()

	if _, err := os.Stat(a.logFile); os.IsNotExist(err) && !req.Follow {
		return nil
	}

	config := tail.Config{
		Follow: req.Follow,
		ReOpen: req.Follow,
	}

	if req.TailLines > 0 {
		config.Location = &tail.SeekInfo{Offset: int64(-req.TailLines), Whence: io.SeekEnd}
	}

	t, err := tail.TailFile(a.logFile, config)
	if err != nil {
		return fmt.Errorf("tail file: %w", err)
	}
	defer t.Cleanup()

	for {
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case line, ok := <-t.Lines:
			if !ok {
				if t.Err() != nil {
					return fmt.Errorf("tail error: %w", t.Err())
				}
				return nil
			}
			if line.Err != nil {
				a.logger.Warn().Err(line.Err).Msg("Error reading log line")
				continue
			}
			if err := stream.Send(&agentv1.GetLogsResponse{
				Line: line.Text + "\n",
			}); err != nil {
				t.Stop()
				return err
			}
		}
	}
}
