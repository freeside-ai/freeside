package cordonhost

import (
	"log/slog"
	"sync"
)

// awakeHolder keeps the host from idle sleep while any launch or recovery
// is in flight: the first hold takes a power assertion and the last
// release lets it go. A host that sleeps mid-run stalls the run, so a
// failure to take the assertion is logged rather than refusing the work,
// which would strand a recovery for want of it.
type awakeHolder struct {
	logger *slog.Logger
	// prevent takes the assertion; nil means preventIdleSleep. Only this
	// package's tests replace it.
	prevent func() (func() error, error)

	mu      sync.Mutex
	holds   int
	release func() error
}

func (a *awakeHolder) hold() func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holds++
	if a.holds == 1 {
		prevent := a.prevent
		if prevent == nil {
			prevent = preventIdleSleep
		}
		release, err := prevent()
		if err != nil {
			a.logger.Warn("cordon host: keep the host awake", "error", err)
		}
		a.release = release
	}
	var once sync.Once
	return func() { once.Do(a.drop) }
}

func (a *awakeHolder) drop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holds--
	if a.holds > 0 || a.release == nil {
		return
	}
	if err := a.release(); err != nil {
		a.logger.Warn("cordon host: release the power assertion", "error", err)
	}
	a.release = nil
}
