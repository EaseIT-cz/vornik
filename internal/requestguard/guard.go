// Package requestguard binds an assembled physical request to a daemon-owned decision.
package requestguard

import (
	"context"
	"errors"
)

type key struct{}

// Check validates the immutable action decision without holding locks over network I/O.
type Check func(context.Context) error

// Refusal contains no credentials, provider response or account identity.
type Refusal struct{ AfterRejected bool }

func (*Refusal) Error() string {
	return "the connection changed since this proposal; run the workflow again"
}

// With installs a daemon-owned physical-request check in a private context value.
func With(ctx context.Context, check Check) context.Context {
	return context.WithValue(ctx, key{}, check)
}

// Present reports whether the context carries a callable physical-request check.
func Present(ctx context.Context) bool { f, ok := ctx.Value(key{}).(Check); return ok && f != nil }

// Validate checks an assembled request and returns a bounded refusal on failure.
func Validate(ctx context.Context) error {
	if f, ok := ctx.Value(key{}).(Check); ok && f != nil {
		if err := f(ctx); err != nil {
			return &Refusal{}
		}
	}
	return nil
}

// IsRefusal reports whether an error carries a physical-request refusal.
func IsRefusal(err error) bool { var r *Refusal; return errors.As(err, &r) }

// AfterRejection marks a refusal after an actual vendor-rejected request.
func AfterRejection(err error) error {
	if IsRefusal(err) {
		return &Refusal{AfterRejected: true}
	}
	return err
}
