// Package wardname is the one definition of the runtime object names ward
// creates inside the production rig: the name prefixes, the run-ID shapes, and
// the role suffixes each shape may carry.
//
// It gates a destructive path. The rig lease (internal/daemonlock) renders its
// container, volume, and network name patterns from RigShapes, and rig
// cleanup and stale recovery act only on names those patterns admit. Ward
// builds the same names from the constants here, and a ward test fails when
// ward can produce a name RigShapes doesn't admit, or RigShapes admits a role
// no ward enumerator produces. So a new ward role reaches the rig only by
// being registered here.
//
// The package is a leaf: daemonlock can't import ward, because ward's
// package-internal tests import daemonlock.
//
// Outside this definition on purpose: the enrollment credential observer
// (freeside-enroll-credential-), which never binds to the rig; the audit
// marker, a path inside a container; and host temporary directories. The
// shapes record the run IDs their producers mint (exec/claude.RunIDFor, the
// conformance driver, and the review gate) but don't bind those producers.
package wardname
