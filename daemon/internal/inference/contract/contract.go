// Package contract holds reusable tests for inference Driver implementations.
// Production binaries never import this package.
package contract

import (
	"context"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// Script is the behavior a driver is built to perform on every call. With
// neither flag set, the call returns Response.
type Script struct {
	Response inference.Response
	// Fail makes the call fail.
	Fail bool
	// Block holds the call in flight until its context is canceled.
	Block bool
}

// Factory lets an implementation expose its scripting and observation seams
// while the runner owns the behavioral assertions. New takes the script
// because a driver may fix its behavior when it is built, as the Claude
// driver pins its CLI binary.
type Factory struct {
	// New returns a driver scripted with script. When script.Block is set,
	// started closes once a call is in flight; otherwise it may be nil.
	New func(t *testing.T, script Script) (driver inference.Driver, started <-chan struct{})
	// Requests returns the requests the driver delivered, as far as the
	// implementation can observe them: at least each one's Fields and Model.
	Requests func(*testing.T, inference.Driver) []inference.Request
}

// cancelWatchdog bounds how long a canceled call may take to return before
// the suite reports it hung, instead of waiting out the test binary's timeout.
const cancelWatchdog = 30 * time.Second

// Run checks what the Driver interface promises: a successful call returns
// the answer and delivers the request once, a failed call returns an error
// and no response, and a canceled call returns an error. It checks no error's
// identity, which is not part of the contract (inference.Driver). The request
// uses the classifier site, so a driver that validates site, fields, model,
// and output accepts it.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	site := inference.ClassifierSite(inference.Budget{})
	fields := make(map[string]string, len(site.Fields))
	for _, field := range site.Fields {
		fields[field.Name] = "contract " + field.Name
	}
	request := inference.Request{
		SiteID: site.ID, Fields: fields,
		MaxOutput: site.MaxOutputBytes, MaxComputeUnits: site.MaxComputeUnits,
		Model: "contract-model",
	}
	const secret = inference.Secret("contract-secret")
	answer := inference.Response{
		Output:       []byte(`{"materiality":"high","confidence":"high","note":"Concrete reachable failure"}`),
		ComputeUnits: 12,
		Observed:     inference.Observed{ModelID: request.Model, OutputTokens: 12},
	}

	t.Run("response", func(t *testing.T) {
		driver, _ := factory.New(t, Script{Response: answer})
		got, err := driver.Complete(t.Context(), request, secret)
		if err != nil {
			t.Fatalf("Complete error = %v", err)
		}
		if string(got.Output) != string(answer.Output) || got.ComputeUnits != answer.ComputeUnits || got.Observed != answer.Observed {
			t.Fatalf("Complete = %+v, want %+v", got, answer)
		}
		requests := factory.Requests(t, driver)
		if len(requests) != 1 || !maps.Equal(requests[0].Fields, request.Fields) || requests[0].Model != request.Model {
			t.Fatalf("requests = %+v, want one with fields %v and model %q", requests, request.Fields, request.Model)
		}
	})

	t.Run("failure", func(t *testing.T) {
		driver, _ := factory.New(t, Script{Fail: true})
		got, err := driver.Complete(t.Context(), request, secret)
		if err == nil || !reflect.DeepEqual(got, inference.Response{}) {
			t.Fatalf("Complete = %+v, %v; want a zero response and an error", got, err)
		}
	})

	t.Run("cancellation in flight", func(t *testing.T) {
		driver, started := factory.New(t, Script{Block: true})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := complete(ctx, driver, request, secret)
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("Complete returned before the call was in flight: %v", err)
		case <-time.After(cancelWatchdog):
			t.Fatalf("call neither started nor returned within %s", cancelWatchdog)
		}
		cancel()
		requireCanceledError(t, done)
	})

	t.Run("canceled before call", func(t *testing.T) {
		driver, _ := factory.New(t, Script{Block: true})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		requireCanceledError(t, complete(ctx, driver, request, secret))
	})
}

// complete runs one call in the background and delivers its error.
func complete(ctx context.Context, driver inference.Driver, request inference.Request, secret inference.Secret) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := driver.Complete(ctx, request, secret)
		done <- err
	}()
	return done
}

// requireCanceledError fails unless a canceled call returns an error within
// cancelWatchdog.
func requireCanceledError(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled call returned no error")
		}
	case <-time.After(cancelWatchdog):
		t.Fatalf("canceled call did not return within %s", cancelWatchdog)
	}
}
