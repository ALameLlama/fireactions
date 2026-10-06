package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestReapCommandRejectsArguments(t *testing.T) {
	cmd := newReapCmd()
	cmd.SetArgs([]string{"unexpected"})
	assert.Error(t, cmd.Execute())
}

func TestReapCommandReportsInvalidConfig(t *testing.T) {
	cmd := newReapCmd()
	cmd.SetArgs([]string{"--config", "/path/that/does/not/exist"})
	assert.ErrorContains(t, cmd.Execute(), "config:")
}

func TestMachineEndpointsDefaultToUnixSocket(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{"ps": newPsCmd(), "logs": newLogsCmd()} {
		endpoint, err := cmd.Flags().GetString("endpoint")
		assert.NoError(t, err, name)
		assert.Equal(t, "unix:///run/fireactions/plugin.sock", endpoint, name)
	}
}
