package fake_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/contract"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

func TestDriverContract(t *testing.T) {
	contract.Run(t, contract.Factory{
		New: func(*testing.T) inference.Driver { return fake.New() },
		Script: func(t *testing.T, driver inference.Driver, response inference.Response, err error) {
			t.Helper()
			driver.(*fake.Driver).Script("contract", fake.Script{Response: response, Err: err})
		},
		Block: func(t *testing.T, driver inference.Driver) {
			t.Helper()
			driver.(*fake.Driver).Script("contract", fake.Script{Wait: make(chan struct{})})
		},
		Requests: func(t *testing.T, driver inference.Driver) []inference.Request {
			t.Helper()
			return driver.(*fake.Driver).Requests()
		},
	})
}
