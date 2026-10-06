package main

import (
	"fmt"

	"github.com/hostinger/fireactions/server"
	"github.com/spf13/cobra"
)

func newReapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "reap",
		Short:   "Reaps expired or abandoned Firecracker environments",
		Args:    cobra.NoArgs,
		GroupID: "main",
		RunE:    func(cmd *cobra.Command, _ []string) error { return runReapCmd(cmd) },
	}
	cmd.Flags().SortFlags = false
	cmd.Flags().String("config", "/etc/fireactions/config.yaml", "Sets the configuration file path.")
	return cmd
}

func runReapCmd(cmd *cobra.Command) error {
	configFile, err := cmd.Flags().GetString("config")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if configFile == "" {
		return fmt.Errorf("config is required")
	}
	config, err := server.NewConfig(configFile)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := server.Reap(cmd.Context(), config); err != nil {
		return fmt.Errorf("reaping environments: %w", err)
	}
	return nil
}
