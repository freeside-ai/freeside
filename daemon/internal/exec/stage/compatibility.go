package stage

import "errors"

// These sentinels preserve the existing Claude driver's error identities and
// text while its durable implementation moves into this package. Provider-
// specific adapters re-export the identities their callers already consume.

// ErrUnsupportedStart marks an admitted start this driver refuses to run.
var ErrUnsupportedStart = errors.New("claude driver cannot run this start")

// ErrDriverClosed marks a start refused because daemon shutdown has begun.
var ErrDriverClosed = errors.New("claude driver is closed")

// ErrSeedRetryable marks an operational exact-base failure that is retryable.
var ErrSeedRetryable = errors.New("claude driver seed is retryable")

// ErrSeedRefused marks a definitive exact-base or credential refusal.
var ErrSeedRefused = errors.New("claude driver seed was refused")

// ErrRecoveryRetryable marks a recovery failure that preserved durable state.
var ErrRecoveryRetryable = errors.New("claude driver recovery is retryable")
