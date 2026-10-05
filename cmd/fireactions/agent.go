package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hostinger/fireactions/agent"
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
	return cmd
}

func runAgentCmd(cmd *cobra.Command, _ []string) error {
	logLevel, _ := cmd.Flags().GetString("log-level")

	port, _ := cmd.Flags().GetUint32("port")

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	agentServer, err := agent.New(agent.Config{
		Port:     port,
		LogLevel: logLevel,
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	return agentServer.Run(ctx)
}
