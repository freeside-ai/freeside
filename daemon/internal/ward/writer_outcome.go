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
	st.writerObserver.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return 0, failf(CheckWriterTermination, "create writer-outcome observer: %v", err)
	}
	st.writerObserver.owned = true
	rep, err := b.rt.Inspect(ctx, names.WriterObserver)
	if err != nil {
		return 0, failf(CheckWriterTermination, "inspect writer-outcome observer: %v", err)
	}
	if err := verifySeedRoleAllowlist(
		rep, spec, names.Workspace, b.cfg.WorkspaceTarget, CheckWriterTermination,
	); err != nil {
		return 0, err
	}
	st.writerObserver.fingerprint, err = ownedFingerprint(
		rep.CreationDate, rep.Labels, rep.LabelsObserved, st.ownershipLabel,
	)
	if err != nil {
		return 0, failf(CheckWriterTermination, "writer-outcome observer: %v", err)
	}
	if err := b.rt.StartContainer(ctx, names.WriterObserver); err != nil {
		return 0, failf(CheckWriterTermination, "start writer-outcome observer: %v", err)
	}
	if err := b.waitStopped(
		ctx,
		names.WriterObserver,
		st.writerObserver,
		st.ownershipLabel,
		b.cfg.SeedTimeout,
	); err != nil {
		return 0, failf(CheckWriterTermination, "writer-outcome observer: %v", err)
	}
	status, err := b.readWriterOutcomeProof(
		ctx, hs.RunID, names.WriterObserver, st.ownershipLabel.Value, st,
	)
	if err != nil {
		return 0, err
	}
	if err := b.rt.DeleteContainer(ctx, names.WriterObserver); err != nil {
		return 0, failf(CheckWriterTermination, "delete writer-outcome observer: %v", err)
	}
	if err := b.verifyContainerAbsent(
		ctx,
		names.WriterObserver,
		st.writerObserver,
		st.ownershipLabel,
		CheckWriterTermination,
	); err != nil {
		return 0, err
	}
	st.writerObserver = objectClaim{}
	return status, nil
}

func (b *Backend) readWriterOutcomeProof(
	ctx context.Context,
	runID, id, nonce string,
	st *runState,
) (int, error) {
	dir, err := os.MkdirTemp("", "freeside-handoff-"+runID+"-writer-")
	if err != nil {
		return 0, failf(CheckWriterTermination, "create writer-outcome proof directory: %v", err)
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
		return 0, failf(CheckWriterTermination, "open writer-outcome proof: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only temp handle
	data, found, err := extractArchiveRegularFile(
		f, writerOutcomeProofPath, maxWriterOutcomeProofBytes,
	)
	if err != nil {
		return 0, failf(CheckWriterTermination, "read writer-outcome proof: %v", err)
	}
	if !found {
		return 0, failf(CheckWriterTermination, "writer produced no outcome marker")
	}
	return verifyWriterOutcomeProof(data, nonce)
}

// verifyWriterOutcomeProof authenticates the marker's freshness and shape,
// not its authorship. The writer shares a PID namespace with the launcher, so
// the nonce is readable from the launcher's own cmdline and cannot stop the
// writer composing a well-formed line. What stops it placing one is the
// marker's directory: root-owned 0700 inside a sticky evidence directory an
// unprivileged writer can neither write, rename, nor unlink. Relaxing those
// modes removes the control, whatever this function proves about the bytes.
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

func writerFailureError(status int) error {
	return fmt.Errorf("%w (status %d)", ErrWriterFailed, status)
}
