package main

import (
	"github.com/hostinger/fireactions"
	serverv1 "github.com/hostinger/fireactions/proto/server/v1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// newClient creates a new gRPC client for the given endpoint.
// It returns the client and a cleanup function that should be called when done.
func newClient(endpoint string) (serverv1.ServerServiceClient, func(), error) {
	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, err
	}
	client := serverv1.NewServerServiceClient(conn)
	cleanup := func() { _ = conn.Close() }
	return client, cleanup, nil
}

// NewRootCommand returns a new root-level command.
func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "fireactions",
		Short:         "Run disposable Firecracker execution environments for Forgejo.",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       fireactions.Version,
	}

	cmd.SetVersionTemplate(fireactions.GetVersion())
	cmd.PersistentFlags().SortFlags = false
	cmd.Flags().SortFlags = false

	cmd.AddGroup(&cobra.Group{ID: "main", Title: "Main application commands:"})
	cmd.AddCommand(newReapCmd())
	cmd.AddCommand(newServerCmd())
	cmd.AddCommand(newAgentCmd())
	cmd.AddCommand(newValidateCmd())

	cmd.AddGroup(&cobra.Group{ID: "pool", Title: "Pool management commands:"})
	cmd.AddCommand(newPoolsCmd())

	cmd.AddGroup(&cobra.Group{ID: "machine", Title: "VM diagnostics commands:"})
	cmd.AddCommand(newPsCmd())
	cmd.AddCommand(newLogsCmd())

	cmd.AddGroup(&cobra.Group{ID: "image", Title: "Image management commands:"})
	cmd.AddCommand(newImageCmd())

	cmd.AddCommand(newVersionCmd())

	return cmd
}

func init() {
	cobra.EnableCommandSorting = false
}
