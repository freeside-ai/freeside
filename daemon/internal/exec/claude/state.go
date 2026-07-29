package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// phase is one invocation's durable lifecycle position.
type phase string

const (
	// phaseSeeding: the intent is committed but the gate has not been called,
	// so no ward object and no journal record exist yet. Nothing external has
	// happened, which makes this window plainly rerun-safe.
	phaseSeeding phase = "seeding"
	// phaseRunning: the gate is mid-handoff, so its journal record is open
	// and ward's own recovery is what adopts or loses the run.
	phaseRunning phase = "running"
	// phaseExported: the gate returned a released export and closed its
	// journal record. Ward recovery refuses a closed record by design, so
	// this driver carries the released facts itself and finishes the
	// pipeline from them rather than asking the gate again.
	phaseExported phase = "exported"
	// phaseCommitted: a terminal StageResult is durable and collectable.
	phaseCommitted phase = "committed"
	// phaseLost: recovery proved every runtime object absent with no export.
	// Collect reports ErrNoResult, and the engine reruns from the admission.
	phaseLost phase = "lost"
)

// intent is the driver's durable per-invocation record. It pins every value
// a replayed pipeline must reproduce byte-identically (RecordedAt for the
// export row, CommitDate for the importer's commit), because a crash between
// the export write and the result commit replays the pipeline and the
// write-once store rows must converge rather than collide.
type intent struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        string              `json:"run_id"`
	Phase        phase               `json:"phase"`
	Spec         exec.StartSpec      `json:"spec"`
	// Seed is the daemon-owned checkout the gate seeded from; kept so
	// recovery rebuilds the identical handoff spec.
	Seed   string `json:"seed"`
	Prompt string `json:"prompt"`
	// Inputs are the three immutable bodies used to render Prompt. Their
	// digests are re-checked against Spec on every reconstruction, so the
	// exported state file cannot substitute a prompt or policy while the
	// daemon is down.
	Inputs durableInputs `json:"inputs"`
	// Instructions is the materialized vendor-instruction role as ward
	// received it. Recovery rebuilds the handoff from this record rather than
	// re-reading the host file: a host CLAUDE.md edited while the daemon was
	// down must not silently re-target an in-flight run, and the admission
	// snapshot carries only the digest, not the bytes the gate re-hashes.
	Instructions ward.VendorInstructions `json:"instructions"`
	// Export carries what the gate released, recorded the moment Handoff
	// returns. The gate closes its journal at that point and refuses to
	// recover a closed record, so without this the whole import-and-record
	// window would be unrecoverable.
	Export     *releasedExport   `json:"export,omitempty"`
	RecordedAt time.Time         `json:"recorded_at"`
	CommitDate time.Time         `json:"commit_date"`
	Result     *exec.StageResult `json:"result,omitempty"`
}

type durableInputs struct {
	Specification []byte `json:"specification"`
	PromptPackage []byte `json:"prompt_package"`
	Policy        []byte `json:"policy"`
}

func durableInputsFrom(inputs exec.StageInputs) durableInputs {
	return durableInputs{
		Specification: inputs.Specification().Bytes(),
		PromptPackage: inputs.PromptPackage().Bytes(),
		Policy:        inputs.Policy().Bytes(),
	}
}

func (i intent) validate() error {
	switch {
	case i.InvocationID == "":
		return fmt.Errorf("driver intent invocation_id: %w", domain.ErrEmptyID)
	case i.RunID == "":
		return fmt.Errorf("driver intent run_id: %w", domain.ErrEmptyID)
	case !i.Phase.valid():
		return fmt.Errorf("driver intent phase %q is invalid", i.Phase)
	case i.Phase == phaseExported && i.Export == nil:
		return fmt.Errorf("driver intent %q is exported without its released facts", i.InvocationID)
	case i.RecordedAt.IsZero() || i.CommitDate.IsZero():
		return fmt.Errorf("driver intent %q: pinned instants are required", i.InvocationID)
	case i.Phase == phaseCommitted && i.Result == nil:
		return fmt.Errorf("driver intent %q is committed without a result", i.InvocationID)
	case i.Phase != phaseCommitted && i.Result != nil:
		return fmt.Errorf("driver intent %q carries a result in phase %q", i.InvocationID, i.Phase)
	}
	if i.Result != nil {
		return i.Result.Validate()
	}
	return nil
}

// valid reports whether p is a registered phase. Being a validity
// predicate it uses default; the behaviour-dispatch switches over phase omit
// it so a new member must be handled.
func (p phase) valid() bool {
	switch p {
	case phaseSeeding, phaseRunning, phaseExported, phaseCommitted, phaseLost:
		return true
	default:
		return false
	}
}

// releasedExport is the durable form of one gate release: enough to finish
// the pipeline after a restart without calling the gate again.
type releasedExport struct {
	Dir               string                  `json:"dir"`
	Manifest          export.Manifest         `json:"manifest"`
	Evidence          export.EvidenceManifest `json:"evidence"`
	EvidencePresent   bool                    `json:"evidence_present"`
	CommitPlanPresent bool                    `json:"commit_plan_present"`
	ObservedBaseSHA   string                  `json:"observed_base_sha"`
}

func (r releasedExport) outcome() exportOutcome {
	return exportOutcome{
		dir: r.Dir, manifest: r.Manifest, evidence: r.Evidence,
		evidencePresent: r.EvidencePresent, commitPlanPresent: r.CommitPlanPresent,
		observedBaseSHA: r.ObservedBaseSHA,
	}
}

func releasedFrom(out exportOutcome) *releasedExport {
	return &releasedExport{
		Dir: out.dir, Manifest: out.manifest, Evidence: out.evidence,
		EvidencePresent: out.evidencePresent, CommitPlanPresent: out.commitPlanPresent,
		ObservedBaseSHA: out.observedBaseSHA,
	}
}

// intentPath is one invocation's state file. The name is the invocation id
// hashed into the run id, so an id carrying path separators cannot escape
// the state directory.
func (d *Driver) intentPath(id domain.InvocationID) string {
	return filepath.Join(d.dir, RunIDFor(id)+".json")
}

// loadIntent reads one durable intent. Absence returns ErrUnknownInvocation,
// which is the engine's reconciliation vocabulary for "this driver never
// started it".
func (d *Driver) loadIntent(ctx context.Context, id domain.InvocationID) (intent, error) {
	return d.loadIntentRegated(ctx, id, true)
}

// loadIntentAdmission reconstructs an intent against its immutable admission
// without re-applying mutable start policy. It is reserved for stopping an
// already-admitted writer and committing a terminal fact: policy drift must
// prevent new or recovered work, but it must not make existing work
// uncancelable or its terminal outcome permanently unrecordable.
func (d *Driver) loadIntentAdmission(
	ctx context.Context, id domain.InvocationID,
) (intent, error) {
	return d.loadIntentRegated(ctx, id, false)
}

func (d *Driver) loadIntentRegated(
	ctx context.Context, id domain.InvocationID, applyCurrentPolicy bool,
) (intent, error) {
	body, err := os.ReadFile(d.intentPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return intent{}, fmt.Errorf("invocation %s: %w", id, exec.ErrUnknownInvocation)
	}
	if err != nil {
		return intent{}, fmt.Errorf("read driver intent %s: %w", id, err)
	}
	in, err := decodeIntent(body)
	if err != nil {
		return intent{}, fmt.Errorf("decode driver intent %s: %w", id, err)
	}
	// A decoded record is a reconstruction boundary, not trusted state: the
	// daemon convention is to re-run the policy gate on read, and the
	// alternative here is a tampered or corrupted file retargeting a
	// recovered run's image, base, identity, or runtime objects.
	if in.InvocationID != id {
		return intent{}, fmt.Errorf("driver intent file for %s names %s: %w",
			id, in.InvocationID, domain.ErrParentKeyMismatch)
	}
	in, _, err = d.restoreDurableOutcome(ctx, in)
	if err != nil {
		return intent{}, err
	}
	if applyCurrentPolicy {
		err = d.regate(ctx, in, false)
	} else {
		err = d.regateAdmission(ctx, in)
	}
	if err != nil {
		return intent{}, err
	}
	return in, nil
}

// restoreDurableOutcome closes the record-outcome-before-save crash window.
// The write-once store record is consulted after immutable admission
// authentication but before regate can apply mutable current policy to the
// stale preterminal phase.
func (d *Driver) restoreDurableOutcome(
	ctx context.Context, in intent,
) (intent, bool, error) {
	if in.Phase == phaseCommitted || in.Phase == phaseLost {
		return in, false, nil
	}
	if err := d.authority.AuthenticateAdmission(ctx, in.InvocationID, in.Spec); err != nil {
		return intent{}, false, fmt.Errorf("authenticate outcome recovery %s: %w",
			in.InvocationID, err)
	}
	stored, found, err := d.outcomes.LookupExecutionOutcomeRecord(ctx, in.InvocationID)
	if err != nil {
		return intent{}, false, fmt.Errorf("%w: lookup outcome recovery %s: %w",
			ErrRecoveryRetryable, in.InvocationID, err)
	}
	if !found {
		return in, false, nil
	}
	if err := stored.Validate(); err != nil {
		return intent{}, false, fmt.Errorf("%w: outcome recovery %s: %w",
			ErrUnsupportedStart, in.InvocationID, err)
	}
	if stored.InvocationID != in.InvocationID || stored.AdmissionID != in.Spec.AdmissionID {
		return intent{}, false, fmt.Errorf(
			"%w: outcome recovery %s disagrees with its immutable admission",
			ErrUnsupportedStart, in.InvocationID,
		)
	}
	in.Export = nil
	switch stored.Status {
	case domain.ExecutionOutcomeFailed:
		in.Phase = phaseCommitted
		in.Result = &exec.StageResult{
			InvocationID: in.InvocationID,
			Status:       exec.StatusFailed,
			Summary:      stored.Summary,
		}
	case domain.ExecutionOutcomeCanceled:
		in.Phase = phaseCommitted
		in.Result = &exec.StageResult{
			InvocationID: in.InvocationID,
			Status:       exec.StatusCanceled,
			Summary:      stored.Summary,
		}
	case domain.ExecutionOutcomeLost:
		in.Phase = phaseLost
		in.Result = nil
	}
	if err := in.validate(); err != nil {
		return intent{}, false, fmt.Errorf("%w: outcome recovery %s: %w",
			ErrUnsupportedStart, in.InvocationID, err)
	}
	return in, true, nil
}

// decodeIntent strictly reconstructs one private state record. The file is a
// recovery authority, not a permissive interchange format: accepting unknown,
// duplicate, or trailing fields would let a stale or future schema be
// interpreted under today's authorization checks.
func decodeIntent(body []byte) (intent, error) {
	if err := ward.RejectDuplicateJSONKeys(body); err != nil {
		return intent{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var in intent
	if err := decoder.Decode(&in); err != nil {
		return intent{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return intent{}, errors.New("trailing JSON value")
	}
	if err := in.validate(); err != nil {
		return intent{}, err
	}
	return in, nil
}

// regate re-checks the immutable admission and containment facts carried by a
// reconstructed record. Preterminal work additionally requires current
// conformance so it cannot run under a class this driver would now refuse;
// terminal history authenticates its durable export or outcome without being
// made unreadable by a later conformance lapse or configuration change.
func (d *Driver) regate(ctx context.Context, i intent, forceCurrent bool) error {
	return d.regateWithCurrentPolicy(ctx, i, forceCurrent, true)
}

func (d *Driver) regateAdmission(ctx context.Context, i intent) error {
	return d.regateWithCurrentPolicy(ctx, i, false, false)
}

func (d *Driver) regateWithCurrentPolicy(
	ctx context.Context, i intent, forceCurrent, applyCurrentPolicy bool,
) error {
	switch {
	case i.RunID != RunIDFor(i.InvocationID):
		return fmt.Errorf("%w: intent %s names run %q, derivation gives %q",
			ErrUnsupportedStart, i.InvocationID, i.RunID, RunIDFor(i.InvocationID))
	case i.Spec.CredentialMode != domain.CredentialSubscriptionContained:
		return fmt.Errorf("%w: intent %s carries credential mode %q",
			ErrUnsupportedStart, i.InvocationID, i.Spec.CredentialMode)
	case i.Spec.EgressProfile != domain.EgressProviderOnly:
		return fmt.Errorf("%w: intent %s carries egress profile %q",
			ErrUnsupportedStart, i.InvocationID, i.Spec.EgressProfile)
	case i.Spec.AuthIdentityID == "":
		return fmt.Errorf("%w: intent %s names no auth identity",
			ErrUnsupportedStart, i.InvocationID)
	case i.Spec.Workspace != WorkspaceFor(i.InvocationID):
		return fmt.Errorf("%w: intent %s names workspace %q, derivation gives %q",
			ErrUnsupportedStart, i.InvocationID, i.Spec.Workspace, WorkspaceFor(i.InvocationID))
	case i.Seed != filepath.Join(d.seedRoot, RunIDFor(i.InvocationID)):
		return fmt.Errorf("%w: intent %s names seed %q, derivation gives %q",
			ErrUnsupportedStart, i.InvocationID, i.Seed,
			filepath.Join(d.seedRoot, RunIDFor(i.InvocationID)))
	}
	if err := i.Spec.Base.Validate(); err != nil {
		return fmt.Errorf("intent %s base: %w", i.InvocationID, err)
	}
	if err := d.authority.AuthenticateAdmission(ctx, i.InvocationID, i.Spec); err != nil {
		return fmt.Errorf("authenticate intent %s: %w", i.InvocationID, err)
	}
	if i.Export != nil {
		if err := d.authenticateReleasedExport(ctx, i, i.Export.Dir); err != nil {
			return classifyReleasedExportAuthentication(err)
		}
	}
	if i.Spec.StageInputs == nil {
		return fmt.Errorf("%w: intent %s carries no stage-input snapshot",
			ErrUnsupportedStart, i.InvocationID)
	}
	snapshot := i.Spec.StageInputs
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("intent %s stage inputs: %w", i.InvocationID, err)
	}
	if snapshot.InputDigest != i.Spec.InputDigest ||
		snapshot.SpecificationDigest != i.Spec.SpecDigest ||
		snapshot.PolicyDigest != i.Spec.PolicyDigest {
		return fmt.Errorf("intent %s stage inputs disagree with start spec: %w",
			i.InvocationID, domain.ErrParentKeyMismatch)
	}
	checks := []struct {
		name   string
		body   []byte
		digest domain.Digest
	}{
		{"specification", i.Inputs.Specification, snapshot.SpecificationDigest},
		{"prompt package", i.Inputs.PromptPackage, snapshot.PromptPackageDigest},
		{"policy", i.Inputs.Policy, snapshot.PolicyDigest},
	}
	for _, check := range checks {
		sum := sha256.Sum256(check.body)
		got := domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
		if got != check.digest {
			return fmt.Errorf("%w: intent %s %s hashes to %s, admission names %s",
				ErrUnsupportedStart, i.InvocationID, check.name, got, check.digest)
		}
	}
	vendor := snapshot.VendorInstructions
	if vendor == nil || vendor.Vendor != i.Instructions.Vendor {
		return fmt.Errorf("%w: intent %s vendor instructions disagree with admission",
			ErrUnsupportedStart, i.InvocationID)
	}
	switch {
	case vendor.Digest == nil:
		if i.Instructions.Present || i.Instructions.Digest != "" ||
			len(i.Instructions.Body) != 0 {
			return fmt.Errorf("%w: intent %s materialized absent vendor instructions",
				ErrUnsupportedStart, i.InvocationID)
		}
	case !i.Instructions.Present || i.Instructions.Digest != *vendor.Digest:
		return fmt.Errorf("%w: intent %s vendor-instruction digest disagrees with admission",
			ErrUnsupportedStart, i.InvocationID)
	default:
		sum := sha256.Sum256(i.Instructions.Body)
		got := domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
		if got != *vendor.Digest {
			return fmt.Errorf("%w: intent %s vendor instructions hash to %s, admission names %s",
				ErrUnsupportedStart, i.InvocationID, got, *vendor.Digest)
		}
	}
	wantPrompt, promptErr := renderPromptParts(i.Inputs)
	if promptErr != nil {
		if i.Phase != phaseCommitted || i.Result == nil ||
			i.Result.Status != exec.StatusFailed || i.Prompt != "" ||
			i.Result.Summary != truncateSummary(promptErr.Error()) {
			return fmt.Errorf("%w: intent %s does not carry the authenticated refusal",
				ErrUnsupportedStart, i.InvocationID)
		}
		// A new deterministic refusal is authenticated by the current
		// admission here, then its outcome is recorded before saveIntent.
		// Reconstruction must find that independent terminal authority.
		if forceCurrent {
			return nil
		}
		return d.authenticateOutcome(ctx, i, *i.Result)
	}
	if i.Prompt != wantPrompt {
		return fmt.Errorf("%w: intent %s prompt disagrees with admitted inputs",
			ErrUnsupportedStart, i.InvocationID)
	}
	requireCurrent := forceCurrent && applyCurrentPolicy
	switch i.Phase {
	case phaseCommitted:
		record, found, err := d.exports.LookupExecutionExportRecord(ctx, i.InvocationID)
		if err != nil {
			return fmt.Errorf("authenticate committed result %s: %w", i.InvocationID, err)
		}
		if i.Result.Status == exec.StatusCompleted {
			if !found || i.Export == nil {
				return fmt.Errorf("%w: completed intent %s has no durable export",
					ErrUnsupportedStart, i.InvocationID)
			}
			artifacts, err := validateReleasedExportRecord(
				d.exportRoot, i, i.Export.outcome(), record,
			)
			if err != nil {
				return err
			}
			if i.Result.HeadSHA != record.HeadSHA ||
				!slices.Equal(i.Result.Artifacts, artifacts) {
				return fmt.Errorf("%w: completed intent %s disagrees with durable export",
					ErrUnsupportedStart, i.InvocationID)
			}
		} else {
			if found {
				return fmt.Errorf("%w: non-completed intent %s has a durable export",
					ErrUnsupportedStart, i.InvocationID)
			}
			if err := d.authenticateOutcome(ctx, i, *i.Result); err != nil {
				return err
			}
		}
	case phaseLost:
		_, found, err := d.exports.LookupExecutionExportRecord(ctx, i.InvocationID)
		if err != nil {
			return fmt.Errorf("authenticate lost result %s: %w", i.InvocationID, err)
		}
		if found {
			return fmt.Errorf("%w: lost intent %s has a durable export",
				ErrUnsupportedStart, i.InvocationID)
		}
		if err := d.authenticateLost(ctx, i); err != nil {
			return err
		}
	case phaseExported:
		record, found, err := d.exports.LookupExecutionExportRecord(ctx, i.InvocationID)
		if err != nil {
			return fmt.Errorf("%w: authenticate exported result %s: %w",
				ErrRecoveryRetryable, i.InvocationID, err)
		}
		if found {
			if _, err := validateReleasedExportRecord(
				d.exportRoot, i, i.Export.outcome(), record,
			); err != nil {
				return fmt.Errorf("%w: %w", errExportAuthorityConflict, err)
			}
		} else if applyCurrentPolicy {
			requireCurrent = true
		}
	case phaseSeeding, phaseRunning:
		requireCurrent = applyCurrentPolicy
	}
	if requireCurrent {
		if err := d.authority.AuthenticateStart(ctx, i.InvocationID, i.Spec); err != nil {
			return fmt.Errorf("authenticate current intent %s: %w", i.InvocationID, err)
		}
	}
	return nil
}

// saveIntent writes one intent durably: a temp file in the same directory,
// fsynced, then renamed, so a crash mid-write leaves the previous record
// rather than a truncated one.
func (d *Driver) saveIntent(in intent) error {
	if err := in.validate(); err != nil {
		return err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode driver intent %s: %w", in.InvocationID, err)
	}
	tmp, err := os.CreateTemp(d.dir, RunIDFor(in.InvocationID)+".*.tmp")
	if err != nil {
		return fmt.Errorf("stage driver intent %s: %w", in.InvocationID, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write driver intent %s: %w", in.InvocationID, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync driver intent %s: %w", in.InvocationID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close driver intent %s: %w", in.InvocationID, err)
	}
	if err := os.Rename(name, d.intentPath(in.InvocationID)); err != nil {
		return fmt.Errorf("commit driver intent %s: %w", in.InvocationID, err)
	}
	// Syncing the file persists its contents; only syncing the directory
	// persists the entry that names it. Without this a power loss after
	// Start returns can drop the record while the outbox row is already
	// dispatched, leaving an invocation nothing can adopt and nothing can
	// rerun.
	dir, err := os.Open(d.dir)
	if err != nil {
		return fmt.Errorf("open driver state dir: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync driver state dir: %w", err)
	}
	return nil
}

// listIntents returns every durable intent, for restart reconciliation.
func (d *Driver) listIntents(ctx context.Context) ([]intent, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, fmt.Errorf("list driver intents: %w", err)
	}
	out := make([]intent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect driver intent %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: driver intent %s is not a regular file",
				ErrUnsupportedStart, entry.Name())
		}
		body, err := os.ReadFile(filepath.Join(d.dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read driver intent %s: %w", entry.Name(), err)
		}
		in, err := decodeIntent(body)
		if err != nil {
			return nil, fmt.Errorf("decode driver intent %s: %w", entry.Name(), err)
		}
		if entry.Name() != filepath.Base(d.intentPath(in.InvocationID)) {
			return nil, fmt.Errorf(
				"%w: driver intent %s names invocation %s whose canonical file is %s",
				ErrUnsupportedStart, entry.Name(), in.InvocationID,
				filepath.Base(d.intentPath(in.InvocationID)))
		}
		// The same fail-closed reconstruction gate loadIntent runs. Recovery
		// hands this record's RunID straight to the ward gate, so an
		// enumeration path that skipped the gate would let a corrupted or
		// tampered file retarget recovery at another run's objects.
		var restored bool
		in, restored, err = d.restoreDurableOutcome(ctx, in)
		if err != nil {
			return nil, err
		}
		if err := d.regate(ctx, in, false); err != nil {
			return nil, err
		}
		if restored {
			if err := d.saveIntent(in); err != nil {
				return nil, fmt.Errorf("%w: persist recovered outcome %s: %w",
					ErrRecoveryRetryable, in.InvocationID, err)
			}
		}
		out = append(out, in)
	}
	return out, nil
}
