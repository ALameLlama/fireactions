package executor

import (
	"context"
	"errors"
)

// Kind is a protocol-independent error classification.
type Kind uint8

const (
	Unknown Kind = iota
	InvalidArgument
	NotFound
	FailedPrecondition
	Unimplemented
	ResourceExhausted
	Cancelled
	DeadlineExceeded
	Unavailable
	Internal
)

// LaunchError is a completed guest response reporting that no process could be
// started. It is distinct from an interrupted or lost execution stream.
type LaunchError struct {
	Message string
}

func (e *LaunchError) Error() string {
	return e.Message
}

// Error preserves an underlying cause without exposing it in a client-facing
// message. In particular, transport errors need not disclose guest commands or
// environment values.
type Error struct {
	Kind    Kind
	Message string
	Cause   error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func NewError(kind Kind, message string, cause error) error {
	return &Error{Kind: kind, Message: message, Cause: cause}
}

func KindOf(err error) Kind {
	if err == nil {
		return Unknown
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return Cancelled
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Kind
	}
	return Internal
}
