package ward

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrWriterFailed marks a nonzero writer status read from a marker carrying
// this run's nonce. The status is durable in the journal before this error is
// returned.
var ErrWriterFailed = errors.New("writer process failed")

const maxWriterOutcomeProofBytes = 256

func (b *Backend) observeWriterOutcome(
	ctx context.Context,
	hs HandoffSpec,
	names handoffNames,
	st *runState,
) (int, error) {
	spec := buildWriterOutcomeObserverSpec(b.cfg, hs, names, st.ownershipLabel)
	var status int
	// proofChecked marks that checkProof accepted the status, so only the
	// delete and the absence proof can have failed since; those alone take
	// writerObservationError's wrapping, as every earlier failure either
	// precedes the status or already carries ErrWriterFailed.
	proofChecked := false
	err := b.runtimeOps.runHelperRole(ctx, helperRole{
		spec: spec, label: "writer-outcome observer", claim: &st.writerObserver, owner: st.ownershipLabel,
		check: CheckWriterTermination, class: hs.Class, timeout: b.cfg.SeedTimeout,
		verify: func(rep InspectReport) error {
			return verifySeedRoleAllowlist(
				rep, spec, names.Workspace, b.cfg.WorkspaceTarget, CheckWriterTermination,
			)
		},
		readProof: func(ctx context.Context) ([]byte, error) {
			read, err := b.readWriterOutcomeProof(
				ctx, hs, names.WriterObserver, st.ownershipLabel.Value, st,
			)
			if err == nil {
				status = read
			}
			return nil, err
		},
		// The journal write stays ahead of the delete: the failure status is
		// durable before the observer that read it is gone.
		checkProof: func([]byte) error {
			if status != 0 {
				if b.cfg.Journal != nil {
					st.preserveForRecovery, st.leaveJournalOpen = true, true
					if err := b.cfg.Journal.MarkWriterFailed(ctx, hs.RunID, status); err != nil {
						return errors.Join(ErrWriterFailed, err)
					}
					st.preserveForRecovery, st.leaveJournalOpen = false, false
				}
				st.writerFailureStatus = &status
			}
			proofChecked = true
			return nil
		},
	})
	if err != nil {
		if proofChecked {
			return 0, writerObservationError(status, err)
		}
		return 0, err
	}
	return status, nil
}

func writerObservationError(status int, err error) error {
	if status != 0 {
		return errors.Join(ErrWriterFailed, err)
	}
	return err
}

func (b *Backend) readWriterOutcomeProof(
	ctx context.Context,
	hs HandoffSpec,
	id, nonce string,
	st *runState,
) (int, error) {
	dir, err := os.MkdirTemp("", "freeside-handoff-"+hs.RunID+"-writer-")
	if err != nil {
		return 0, failw(CheckWriterTermination, err, "create writer-outcome proof directory: %v", err)
	}
	st.writerArchiveDir = dir
	defer func() {
		_ = os.RemoveAll(dir)
		st.writerArchiveDir = ""
	}()
	tarPath := filepath.Join(dir, "observer.tar")
	if err := b.materializeRootFS(ctx, id, tarPath, CheckWriterTermination); err != nil {
		return 0, err
	}
	f, err := os.Open(tarPath) //nolint:gosec // gate-owned fresh temp path
	if err != nil {
		return 0, failw(CheckWriterTermination, err, "open writer-outcome proof: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only temp handle
	data, found, err := extractArchiveRegularFile(
		f, writerOutcomeProofPath, maxWriterOutcomeProofBytes,
	)
	if err != nil {
		return 0, failw(CheckWriterTermination, err, "read writer-outcome proof: %v", err)
	}
	if !found {
		return 0, failf(CheckWriterTermination, "writer produced no outcome marker")
	}
	status, err := verifyWriterOutcomeProof(data, nonce)
	if err != nil {
		return 0, err
	}
	if status != 0 {
		if err := b.captureFailureEvidence(ctx, hs, st, f, status); err != nil {
			return 0, errors.Join(ErrWriterFailed, err)
		}
	}
	return status, nil
}

// verifyWriterOutcomeProof authenticates the marker's freshness and shape,
// not its authorship. The writer shares a PID namespace with the launcher, so
// the nonce is readable from the launcher's own cmdline and cannot stop the
// writer composing a well-formed line. What stops it placing one is the
// marker's directory: root-owned, writable by nobody else, and inside a
// root-owned sticky evidence directory, so an unprivileged writer can neither
// write into it, rename it, nor unlink it. A launcher may leave the directory
// searchable while it is empty, and closes it to 0700 before it writes
// anything there. Giving up the ownership, the write bits, or the sticky
// parent removes the control, whatever this function proves about the bytes.
func verifyWriterOutcomeProof(data []byte, nonce string) (int, error) {
	if len(data) == 0 || data[len(data)-1] != '\n' ||
		strings.Count(string(data), "\n") != 1 {
		return 0, failf(CheckWriterTermination, "writer outcome marker is malformed")
	}
	line := strings.TrimSuffix(string(data), "\n")
	gotNonce, statusText, ok := strings.Cut(line, " ")
	if !ok || gotNonce != nonce || statusText == "" ||
		strings.ContainsAny(statusText, " \t\r") {
		return 0, failf(CheckWriterTermination, "writer outcome marker is malformed or stale")
	}
	status, err := strconv.Atoi(statusText)
	if err != nil || status < 0 || status > 255 ||
		strconv.Itoa(status) != statusText {
		return 0, failf(CheckWriterTermination, "writer outcome status is invalid")
	}
	return status, nil
}

// writerFailure is a nonzero writer status. It matches ErrWriterFailed and
// keeps the status, so the conformance suite can name the writer step that
// status reports.
type writerFailure struct {
	status int
}

func (e *writerFailure) Error() string {
	return fmt.Sprintf("%v (status %d)", ErrWriterFailed, e.status)
}

func (e *writerFailure) Unwrap() error { return ErrWriterFailed }

func writerFailureError(status int) error {
	return &writerFailure{status: status}
}
