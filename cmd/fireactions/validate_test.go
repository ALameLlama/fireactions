package main

import (
	"bytes"
	"os"
	"path/filepath"
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

func TestValidateCommand_HostModeRequiresConfiguration(t *testing.T) {
	cmd := newValidateCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--host"})
	assert.Error(t, cmd.Execute())
}

func TestValidateCommand_HostModeRejectsExtraArguments(t *testing.T) {
	cmd := newValidateCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--host", "config.yaml", "unexpected"})
	assert.Error(t, cmd.Execute())
}

func TestValidateCommand_PrintSocketGroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		yaml  string
		group string
	}{
		{name: "default", yaml: "", group: "fireactions"},
		{name: "unquoted", yaml: "socket_group: ci-runners", group: "ci-runners"},
		{name: "single quoted", yaml: "socket_group: 'ci-runners' # runner access", group: "ci-runners"},
		{name: "double quoted escape", yaml: `socket_group: "ci\u002drunners"`, group: "ci-runners"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := writeValidateConfig(t, tc.yaml)
			cmd := NewRootCommand()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"validate", "--print-socket-group", config})
			require.NoError(t, cmd.Execute())
			assert.Equal(t, tc.group+"\n", stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func TestValidateCommand_PrintSocketGroupRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{name: "invalid group", yaml: "socket_group: 'bad group'"},
		{name: "unrelated invalid value", yaml: "socket_group: ci-runners\nlog_level: invalid"},
		{name: "unknown field", yaml: "socket_group: ci-runners\nunknown_option: true"},
		{name: "invalid YAML", yaml: "socket_group: [unterminated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := writeValidateConfig(t, tc.yaml)
			cmd := NewRootCommand()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"validate", "--print-socket-group", config})
			require.Error(t, cmd.Execute())
			assert.Empty(t, stdout.String(), "invalid configuration must not supply an installer group")
		})
	}
}

func writeValidateConfig(t *testing.T, overrides string) string {
	t.Helper()
	config := overrides + `
pools:
  - name: test
    image: localhost/fireactions-guest:test
    image_pull_policy: Never
    firecracker:
      binary_path: /usr/local/bin/firecracker
      kernel_image_path: /var/lib/fireactions/kernels/6.1/vmlinux
      machine_config:
        vcpu_count: 1
        mem_size_mib: 128
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(config), 0600))
	return path
}
