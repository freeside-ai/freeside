package domain

// clonePtr returns a pointer to a copy of *p, or nil when p is nil. The
// constructors use it to detach a validated value from caller-owned pointers,
// so the value cannot change out from under its validation when the caller
// later reuses or mutates the variable it passed in.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
