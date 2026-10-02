package ward

import (
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// EnrollmentBootstrap is one first enrollment of a harness client (plan §5.4):
// the record to write and the lease binding its bootstrap mutation takes. The
// persistence port records the enrollment and takes the bound lease in one
// transaction, so the fence that authors the store names exactly the
// enrollment it creates, and the verified result appends generation one.
type EnrollmentBootstrap struct {
	Enrollment domain.ClientEnrollment
	// Binding names the enrollment, generation zero, the store locator, and
	// the digest of the bytes about to be written. The store checks the digest
	// only past bootstrap, so here it is provenance.
	Binding domain.LeaseGenerationBinding
	// EnableIdentity asks the store to enable an existing identity in the
	// same transaction that records the enrollment, so no failure leaves it
	// enrolled and disabled. Only adoption sets it: a flag-era identity can be
	// stored disabled by the old harness seed step. `auth add` leaves it
	// unset and keeps the stored bit. The store does not trust the request:
	// it refuses the bootstrap when the identity already holds an enrollment
	// with a generation.
	EnableIdentity bool
}

// ErrEnrollmentExists refuses a first enrollment whose id already names a
// recorded enrollment that holds a store generation, or a different
// enrollment. Replacing an enrolled store is re-enroll, never a second add.
var ErrEnrollmentExists = errors.New(
	"client enrollment already exists; replacing its store is re-enroll")
