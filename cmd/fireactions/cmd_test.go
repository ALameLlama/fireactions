package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestRootCommandRegistersOnlyRetainedCommands(t *testing.T) {
	cmd := NewRootCommand()
	names := make(map[string]struct{})
	for _, child := range cmd.Commands() {
		names[child.Name()] = struct{}{}
	}
	for _, name := range []string{"server", "agent", "validate", "pools", "ps", "logs", "image", "version"} {
		assert.Contains(t, names, name)
	}
	assert.NotContains(t, names, "login")
}

func TestMachineEndpointsDefaultToUnixSocket(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{"ps": newPsCmd(), "logs": newLogsCmd()} {
		endpoint, err := cmd.Flags().GetString("endpoint")
		assert.NoError(t, err, name)
		assert.Equal(t, "unix:///run/fireactions/plugin.sock", endpoint, name)
	}
}
