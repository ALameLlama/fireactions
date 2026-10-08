package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitCgroupsEmptyRequiresUnpopulatedScopes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		events      string
		wantTimeout bool
	}{
		{name: "empty scope completes", events: "populated 0\nfrozen 0\n"},
		{name: "live descendants must be awaited", events: "populated 1\nfrozen 0\n", wantTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte(tc.events), 0600); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			close(exited)
			scope := &processScope{id: "observed", cgroup: path, exited: exited}
			cg := &cgroupScopes{scopes: map[string]string{scope.id: path}}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			err := waitCgroupsEmpty(ctx, cg, []*processScope{scope}, time.Second)
			if tc.wantTimeout {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("live scope completed before cancellation: %v", err)
				}
			} else if err != nil {
				t.Fatalf("empty scope did not complete: %v", err)
			}
		})
	}
}
