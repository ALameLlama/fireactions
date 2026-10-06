package main

import (
	"fmt"

	"github.com/hostinger/fireactions/server"
	"github.com/spf13/cobra"
)

func newValidateCmd() *cobra.Command {
	var host bool
	cmd := &cobra.Command{
		Use:     "validate <config-file>",
		Short:   "Validates the server configuration file",
		Args:    cobra.ExactArgs(1),
		GroupID: "main",
		RunE:    func(cmd *cobra.Command, args []string) error { return runValidateCmd(cmd, args, host) },
	}
	cmd.Flags().BoolVar(&host, "host", false, "also check configured host prerequisites without changing host state")
	return cmd

}

func runValidateCmd(cmd *cobra.Command, args []string, host bool) error {
	configFile := args[0]

	config, err := server.NewConfig(configFile)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	if host {
		if err := server.CheckHost(cmd.Context(), config); err != nil {
			return fmt.Errorf("host validation failed: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Configuration file %s and host prerequisites are valid\n", configFile)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Configuration file %s is valid\n", configFile)
	return nil
}
