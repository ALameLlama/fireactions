package main

import (
	"fmt"

	"github.com/ALameLlama/fireactions/server"
	"github.com/spf13/cobra"
)

func newValidateCmd() *cobra.Command {
	var host, printSocketGroup bool
	cmd := &cobra.Command{
		Use:     "validate <config-file>",
		Short:   "Validates the server configuration file",
		Args:    cobra.ExactArgs(1),
		GroupID: "main",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidateCmd(cmd, args, host, printSocketGroup)
		},
	}
	cmd.Flags().BoolVar(&host, "host", false, "also check configured host prerequisites without changing host state")
	cmd.Flags().BoolVar(&printSocketGroup, "print-socket-group", false, "print only the socket group after configuration validation (for installers)")
	return cmd

}

func runValidateCmd(cmd *cobra.Command, args []string, host, printSocketGroup bool) error {
	configFile := args[0]

	config, err := server.NewConfig(configFile)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	if host {
		if err := server.CheckHost(cmd.Context(), config); err != nil {
			return fmt.Errorf("host validation failed: %w", err)
		}
	}
	if printSocketGroup {
		fmt.Fprintln(cmd.OutOrStdout(), config.SocketGroup)
		return nil
	}
	if host {
		fmt.Fprintf(cmd.OutOrStdout(), "Configuration file %s and host prerequisites are valid\n", configFile)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Configuration file %s is valid\n", configFile)
	return nil
}
