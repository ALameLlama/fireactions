package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/leases"
	"github.com/rs/zerolog"
)

type blockedPullLeases struct {
	leases.Manager
	entered chan struct{}
	release chan struct{}
	err     error
}

func (l *blockedPullLeases) Create(ctx context.Context, _ ...leases.Opt) (leases.Lease, error) {
	l.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return leases.Lease{}, ctx.Err()
	case <-l.release:
		return leases.Lease{}, l.err
	}
}

func TestConcurrentImagePullCancellationIsIndependent(t *testing.T) {
	for _, canceled := range []string{"waiter", "initiator"} {
		t.Run(canceled, func(t *testing.T) {
			pullErr := errors.New("pull reached containerd")
			service := &blockedPullLeases{
				entered: make(chan struct{}, 2),
				release: make(chan struct{}),
				err:     pullErr,
			}
			client, err := containerd.New("", containerd.WithServices(containerd.WithLeasesService(service)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			logger := zerolog.Nop()
			manager := newImageManager(&logger, client)
			t.Setenv("DOCKER_CONFIG", t.TempDir())

			firstCtx, cancelFirst := context.WithCancel(context.Background())
			defer cancelFirst()
			secondCtx, cancelSecond := context.WithCancel(context.Background())
			defer cancelSecond()
			first := make(chan error, 1)
			second := make(chan error, 1)
			released := false
			defer func() {
				if !released {
					close(service.release)
				}
			}()
			go func() {
				_, err := manager.ensureImage(firstCtx, "example.invalid/guest:latest", "Always")
				first <- err
			}()
			select {
			case <-service.entered:
			case err := <-first:
				t.Fatalf("first pull did not reach containerd: %v", err)
			case <-time.After(time.Second):
				t.Fatal("first pull did not reach containerd")
			}
			go func() {
				_, err := manager.ensureImage(secondCtx, "example.invalid/guest:latest", "Always")
				second <- err
			}()

			canceledResult, survivingResult := second, first
			survivingCtx := firstCtx
			if canceled == "initiator" {
				select {
				case <-service.entered:
				case err := <-second:
					t.Fatalf("second pull did not reach containerd: %v", err)
				case <-time.After(time.Second):
					t.Fatal("second pull waited for another caller")
				}
				cancelFirst()
				canceledResult, survivingResult = first, second
				survivingCtx = secondCtx
			} else {
				cancelSecond()
			}
			select {
			case err := <-canceledResult:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled pull returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled pull waited for another caller")
			}
			if err := survivingCtx.Err(); err != nil {
				t.Fatalf("other pull was canceled: %v", err)
			}
			close(service.release)
			released = true
			select {
			case err := <-survivingResult:
				if !errors.Is(err, pullErr) {
					t.Fatalf("other pull inherited cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("other pull did not finish")
			}
		})
	}
}
