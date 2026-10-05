package v1alpha

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hostinger/fireactions/internal/executor"
	pluginv1alpha "github.com/hostinger/fireactions/proto/forgejo/plugin/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestCreateProfilePrecedenceAndValidation(t *testing.T) {
	s := New(nil, Options{Profiles: map[string]struct{}{"image": {}, "label": {}, "option": {}}})
	cases := []struct {
		name     string
		request  *pluginv1alpha.CreateRequest
		want     string
		wantCode codes.Code
	}{
		{"image wins", &pluginv1alpha.CreateRequest{Image: "image", LabelArg: "label", BackendOptions: map[string]string{"profile": "option"}}, "image", codes.OK},
		{"label wins", &pluginv1alpha.CreateRequest{LabelArg: "label", BackendOptions: map[string]string{"profile": "option"}}, "label", codes.OK},
		{"option fallback", &pluginv1alpha.CreateRequest{BackendOptions: map[string]string{"profile": "option"}}, "option", codes.OK},
		{"no implicit profile", &pluginv1alpha.CreateRequest{}, "", codes.InvalidArgument},
		{"unknown profile", &pluginv1alpha.CreateRequest{Image: "unconfigured"}, "", codes.InvalidArgument},
		{"unknown option", &pluginv1alpha.CreateRequest{Image: "image", BackendOptions: map[string]string{"other": "value"}}, "", codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.selectProfile(tc.request)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("status = %s, want %s (err %v)", status.Code(err), tc.wantCode, err)
			}
			if got != tc.want {
				t.Fatalf("profile = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnsupportedServicesFailBeforeManagerUse(t *testing.T) {
	s := New(nil, Options{Profiles: map[string]struct{}{"image": {}}})
	_, err := s.Create(context.Background(), &pluginv1alpha.CreateRequest{Image: "image", Services: []*pluginv1alpha.ServiceContainer{{Name: "db"}}})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("status = %s, want Unimplemented: %v", status.Code(err), err)
	}
}

func TestRequestedLifetimeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    *durationpb.Duration
		want  time.Duration
		valid bool
	}{
		{"absent", nil, 0, true},
		{"zero", durationpb.New(0), 0, true},
		{"positive", durationpb.New(time.Second + 1), time.Second + 1, true},
		{"negative", durationpb.New(-time.Second), 0, false},
		{"protobuf invalid", &durationpb.Duration{Seconds: 1, Nanos: -1}, 0, false},
		{"duration overflow", &durationpb.Duration{Seconds: 1 << 34}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requestedLifetime(tc.in)
			if (err == nil) != tc.valid {
				t.Fatalf("validity = %v, want %v (err %v)", err == nil, tc.valid, err)
			}
			if err == nil && got != tc.want {
				t.Fatalf("duration = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTypedExecutorErrors(t *testing.T) {
	for kind, want := range map[executor.Kind]codes.Code{
		executor.InvalidArgument:    codes.InvalidArgument,
		executor.NotFound:           codes.NotFound,
		executor.FailedPrecondition: codes.FailedPrecondition,
		executor.ResourceExhausted:  codes.ResourceExhausted,
		executor.Unavailable:        codes.Unavailable,
	} {
		got := grpcError(executor.NewError(kind, "safe message", errors.New("private cause")))
		if status.Code(got) != want {
			t.Errorf("kind %v maps to %s, want %s", kind, status.Code(got), want)
		}
		if status.Convert(got).Message() == "private cause" {
			t.Errorf("cause leaked in status message")
		}
	}
	if got := grpcError(context.Canceled); status.Code(got) != codes.Canceled {
		t.Errorf("cancellation maps to %s", status.Code(got))
	}
}

func TestCopyInReaderFramesAndMetadata(t *testing.T) {
	firstID, firstPath := "env", "/workspace"
	reader := &copyInReader{
		ctx: context.Background(),
		chunks: func() <-chan copyInResult {
			ch := make(chan copyInResult, 2)
			ch <- copyInResult{chunk: &pluginv1alpha.CopyInChunk{Data: []byte("b")}}
			ch <- copyInResult{chunk: &pluginv1alpha.CopyInChunk{DestPath: new("")}}
			return ch
		}(),
		first:        &pluginv1alpha.CopyInChunk{EnvironmentId: &firstID, DestPath: &firstPath, Data: []byte("a")},
		firstPending: true,
	}
	got, err := io.ReadAll(reader)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("later metadata status = %s, want InvalidArgument (%v)", status.Code(err), err)
	}
	if string(got) != "ab" {
		t.Fatalf("data consumed before framing error = %q, want ab", got)
	}
}

func TestCopyInReaderCloseInterruptsPendingInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &copyInReader{ctx: ctx, cancel: cancel, chunks: make(chan copyInResult)}
	done := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 1))
		done <- err
	}()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("closed input returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing archive input left a blocked reader")
	}
}
