package main

import (
	"testing"

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
