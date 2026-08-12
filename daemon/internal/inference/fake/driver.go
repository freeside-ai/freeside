// Package fake provides a deterministic scripted inference driver.
package fake

import (
	"context"
	"errors"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// Script is one deterministic response selected by site id and call count.
type Script struct {
	Response inference.Response
	Err      error
	Wait     <-chan struct{}
}

// Driver replays scripts in registration order without clocks or goroutines.
type Driver struct {
	mu       sync.Mutex
	scripts  map[string][]Script
	requests []inference.Request
}

func New() *Driver { return &Driver{scripts: make(map[string][]Script)} }

func (d *Driver) Script(site string, scripts ...Script) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scripts[site] = append([]Script(nil), scripts...)
}

func (d *Driver) Complete(ctx context.Context, request inference.Request, _ inference.Secret) (inference.Response, error) {
	d.mu.Lock()
	d.requests = append(d.requests, request)
	scripts := d.scripts[request.SiteID]
	if len(scripts) == 0 {
		d.mu.Unlock()
		return inference.Response{}, errors.New("unscripted inference call")
	}
	script := scripts[0]
	d.scripts[request.SiteID] = scripts[1:]
	d.mu.Unlock()
	if script.Wait != nil {
		select {
		case <-ctx.Done():
			return inference.Response{}, ctx.Err()
		case <-script.Wait:
		}
	}
	return script.Response, script.Err
}

func (d *Driver) Requests() []inference.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]inference.Request, len(d.requests))
	copy(out, d.requests)
	return out
}
