package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hostinger/fireactions/agent"
	"github.com/hostinger/fireactions/internal/executor"
	"github.com/spf13/cobra"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "agent",
		Short:   "Starts the Fireactions agent",
		RunE:    runAgentCmd,
		Args:    cobra.NoArgs,
		GroupID: "main",
	}

	cmd.Flags().StringP("log-level", "l", "info", "Log level (debug, info, warn, error, fatal, panic, trace)")
	cmd.Flags().Uint32("port", 9001, "Guest agent VSOCK port")
	cmd.Flags().String("workspace-root", executor.WorkspaceRoot, "Root of the isolated guest workspace")
	cmd.Flags().String("default-user", executor.DefaultUser, "Existing guest identity used for execution and file ownership")
	cmd.Flags().Int64("max-transfer-bytes", executor.DefaultMaxTransferBytes, "Maximum archive framing and extracted file bytes per transfer")
	cmd.Flags().Int("max-archive-entries", executor.DefaultMaxArchiveEntries, "Maximum entries per archive")
	return cmd
}

func runAgentCmd(cmd *cobra.Command, _ []string) error {
	logLevel, _ := cmd.Flags().GetString("log-level")

	port, _ := cmd.Flags().GetUint32("port")
	workspaceRoot, _ := cmd.Flags().GetString("workspace-root")
	defaultUser, _ := cmd.Flags().GetString("default-user")
	maxTransferBytes, _ := cmd.Flags().GetInt64("max-transfer-bytes")
	maxArchiveEntries, _ := cmd.Flags().GetInt("max-archive-entries")
	if maxTransferBytes <= 0 || maxArchiveEntries <= 0 {
		return fmt.Errorf("transfer limits must be positive")
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	agentServer, err := agent.New(agent.Config{
		Port:              port,
		LogLevel:          logLevel,
		WorkspaceRoot:     workspaceRoot,
		DefaultUser:       defaultUser,
		MaxTransferBytes:  maxTransferBytes,
		MaxArchiveEntries: maxArchiveEntries,
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	return agentServer.Run(ctx)
}
