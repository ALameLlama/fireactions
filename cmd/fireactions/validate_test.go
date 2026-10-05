package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCommand_InvalidConfig(t *testing.T) {
	// Create a temporary invalid config file
	tmpFile, err := os.CreateTemp("", "invalid-config-*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	invalidConfig := `
socket_path: /run/fireactions/plugin.sock
socket_group: fireactions
state_dir: /var/lib/fireactions
log_level: invalid_level
pools: []
`
	_, err = tmpFile.WriteString(invalidConfig)
	require.NoError(t, err)
	tmpFile.Close()

	cmd := newValidateCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.SetArgs([]string{tmpFile.Name()})

	err = cmd.Execute()
	assert.Error(t, err)
}

func TestValidateCommand_MissingFile(t *testing.T) {
	cmd := newValidateCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.SetArgs([]string{"/nonexistent/config.yaml"})

	err := cmd.Execute()
	assert.Error(t, err)
}

func TestValidateCommand_NoArgs(t *testing.T) {
	cmd := newValidateCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	cmd.SetArgs([]string{})

	err := cmd.Execute()
	assert.Error(t, err)
}
