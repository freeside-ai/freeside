// Package contract holds reusable tests for inference Driver implementations.
// Production binaries never import this package.
package contract

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// Factory lets an implementation expose its scripting and observation seams
// while the runner owns the behavioral assertions.
type Factory struct {
	New      func(*testing.T) inference.Driver
	Script   func(*testing.T, inference.Driver, inference.Response, error)
	Block    func(*testing.T, inference.Driver)
	Requests func(*testing.T, inference.Driver) []inference.Request
}

// Run checks exact request delivery, response fidelity, and error fidelity.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	request := inference.Request{
		SiteID: "contract", InputDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Fields: map[string]string{"subject": "value"}, MaxOutput: 128,
	}
	t.Run("response", func(t *testing.T) {
		driver := factory.New(t)
		want := inference.Response{Output: []byte(`{"answer":"ok"}`), ComputeUnits: 3}
		factory.Script(t, driver, want, nil)
		got, err := driver.Complete(context.Background(), request, "secret")
		if err != nil || string(got.Output) != string(want.Output) || got.ComputeUnits != want.ComputeUnits {
			t.Fatalf("Complete = %#v, %v", got, err)
		}
		requests := factory.Requests(t, driver)
		if len(requests) != 1 || requests[0].SiteID != request.SiteID || requests[0].InputDigest != request.InputDigest {
			t.Fatalf("requests = %#v", requests)
		}
	})
	t.Run("error", func(t *testing.T) {
		driver := factory.New(t)
		want := context.Canceled
		factory.Script(t, driver, inference.Response{}, want)
		if _, err := driver.Complete(context.Background(), request, "secret"); !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		driver := factory.New(t)
		factory.Block(t, driver)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := driver.Complete(ctx, request, "secret"); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled error = %v, want %v", err, context.Canceled)
		}
	})
}
