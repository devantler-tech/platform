package main

import (
	"context"
	"errors"
)

// A Service annotation expresses desired state. Gate receiver operations on the
// provider's effective disabled state and retain the same provider identity.
func withProviderBaseline(ctx context.Context, b binding, observe func(context.Context, binding, bool) (providerIdentity, error), operation func() error) error {
	return withProviderMode(ctx, b, false, observe, operation)
}

func withProviderMode(ctx context.Context, b binding, wantProxy bool, observe func(context.Context, binding, bool) (providerIdentity, error), operation func() error) error {
	if observe == nil || operation == nil {
		return errors.New("provider-capability")
	}
	before, err := observe(ctx, b, wantProxy)
	if err != nil {
		return err
	}
	if err = operation(); err != nil {
		return err
	}
	after, err := observe(ctx, b, wantProxy)
	if err != nil {
		return err
	}
	if after != before {
		return errors.New("provider-changed")
	}
	return nil
}
