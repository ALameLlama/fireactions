package server

import (
	"context"
	"errors"
	"testing"
)

func TestCheckHostHonorsCanceledContextBeforeHostAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckHost(ctx, DefaultConfig()); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckHost error = %v, want context.Canceled", err)
	}
}
