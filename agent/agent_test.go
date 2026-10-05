package agent

import (
	"path/filepath"
	"testing"
)

func TestCloseReleasesLogger(t *testing.T) {
	a, err := New(Config{Port: 9001, LogLevel: "info"}, func(a *Agent) {
		a.logFile = filepath.Join(t.TempDir(), "agent.log")
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	writer := a.logFileWriter
	if err := a.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := writer.WriteString("after close"); err == nil {
		t.Fatal("log file remains writable after Close")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
	}
}
