package fake_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/contract"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

func TestDriverContract(t *testing.T) {
	contract.Run(t, contract.Factory{
		New: func(t *testing.T, script contract.Script) (inference.Driver, <-chan struct{}) {
			t.Helper()
			step := fake.Script{Response: script.Response}
			if script.Fail {
				step = fake.Script{Err: errors.New("scripted failure")}
			}
			if script.Block {
				// The fake's wait selects on the call's context, so the
				// call counts as in flight from the start.
				step = fake.Script{Wait: make(chan struct{})}
			}
			driver := fake.New()
			driver.Script(inference.ClassifierSiteID, step)
			started := make(chan struct{})
			close(started)
			return driver, started
		},
		Requests: func(t *testing.T, driver inference.Driver) []inference.Request {
			t.Helper()
			return driver.(*fake.Driver).Requests()
		},
	})
}
