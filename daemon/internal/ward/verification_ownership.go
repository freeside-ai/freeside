package ward

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// VerificationOwnership retains task-bound launch ownership before the CLI
// starts. A command interrupted by a daemon crash remains uncertain even if
// no container is visible: its old CLI could still be creating that container.
// Cleanup may remove proven owned containers, but cannot erase that uncertainty.
type VerificationOwnership struct {
	root    string
	runtime Runtime
	mu      sync.Mutex
	live    map[string]verificationCall
}

type verificationCall struct {
	invocation domain.InvocationID
	cancel     context.CancelFunc
	done       chan struct{}
}

type verificationOwner struct {
	Version       int                 `json:"version"`
	TaskID        domain.TaskID       `json:"task_id"`
	RunID         domain.RunID        `json:"run_id"`
	InvocationID  domain.InvocationID `json:"invocation_id"`
	Owner         Label               `json:"owner"`
	CommandJoined bool                `json:"command_joined"`
}

func NewVerificationOwnership(root string, runtime Runtime) (*VerificationOwnership, error) {
	if !filepath.IsAbs(root) || runtime == nil {
		return nil, errors.New("verification ownership needs an absolute directory and runtime")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.Join(err, errors.New("verification ownership root must be a private directory"))
	}
	return &VerificationOwnership{root: root, runtime: runtime, live: make(map[string]verificationCall)}, nil
}

func verificationRunKey(id domain.RunID) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// Bind attaches immutable workflow coordinates without changing verify.Room.
// The factory must run under the engine's registered task-work context.
func (o *VerificationOwnership) Bind(room *ProjectImageRoom, task domain.TaskID, run domain.RunID, invocation domain.InvocationID) error {
	if room == nil || task == "" || run == "" || invocation == "" {
		return domain.ErrEmptyID
	}
	room.ownership = o
	room.ownerBinding = verificationOwner{Version: 1, TaskID: task, RunID: run, InvocationID: invocation}
	return nil
}

func (o *VerificationOwnership) begin(ctx context.Context, binding verificationOwner, owner Label) (context.Context, string, func(bool) error, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", nil, err
	}
	dir := filepath.Join(o.root, verificationRunKey(binding.RunID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, "stopped")); err == nil {
		return nil, "", nil, context.Canceled
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", nil, err
	}
	binding.Owner = owner
	name := verificationRunKey(domain.RunID(owner.Value))
	path := filepath.Join(dir, name+".json")
	if err := writeVerificationOwner(path, binding); err != nil {
		return nil, "", nil, err
	}
	callCtx, cancel := context.WithCancel(ctx)
	call := verificationCall{invocation: binding.InvocationID, cancel: cancel, done: make(chan struct{})}
	o.live[path] = call
	finish := func(joined bool) error {
		defer cancel()
		o.mu.Lock()
		defer o.mu.Unlock()
		defer close(call.done)
		delete(o.live, path)
		binding.CommandJoined = joined
		return writeVerificationOwner(path, binding)
	}
	return callCtx, filepath.Join(dir, name+".cid"), finish, nil
}

func writeVerificationOwner(path string, record verificationOwner) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, body, 0o600)
}

// StopRun fences future room launches, joins live calls, and retries exact
// owned cleanup from private records. Missing records are not a legacy-runtime
// absence proof; the caller must establish that this inventory covers the run.
func (o *VerificationOwnership) StopRun(ctx context.Context, task domain.TaskID, run domain.RunID) error {
	dir := filepath.Join(o.root, verificationRunKey(run))
	o.mu.Lock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		o.mu.Unlock()
		return err
	}
	if err := atomicfile.WriteFile(filepath.Join(dir, "stopped"), []byte(run), 0o600); err != nil {
		o.mu.Unlock()
		return err
	}
	var calls []verificationCall
	for path, call := range o.live {
		if filepath.Dir(path) == dir {
			call.cancel()
			calls = append(calls, call)
		}
	}
	o.mu.Unlock()
	for _, call := range calls {
		select {
		case <-call.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	names, err := verificationRecordNames(dir)
	if err != nil {
		return err
	}
	return o.proveRecordsAbsent(ctx, dir, names, task, run, "", true)
}

// ErrVerificationCommandUnjoined marks a proof that failed because a
// recorded command has no quiescence proof: a daemon crash, or a runner that
// could not prove its process group gone, interrupted the call before it
// joined. The engine reads it to tell the operator which release applies.
var ErrVerificationCommandUnjoined = errors.New("host command has no quiescence proof")

// ProveInvocationAbsent retries owned cleanup for every recorded container
// call of one invocation, across all of its rooms and passes, and proves each
// such container absent and each such command joined. It re-checks the
// private ownership records, never the caller's view of which rooms ran, so a
// container an earlier pass left behind still blocks the proof. Unlike
// StopRun it writes no stopped fence and cancels nothing: a later room of the
// run still launches. A live call of the invocation is refused rather than
// reaped, and a record it cannot read or check fails the proof whatever
// invocation it names, since it may be this one's. A run with no records for
// the invocation has nothing left to prove. Every failure to prove is marked
// ErrVerificationAbsenceUnproven, and an unjoined command's also
// ErrVerificationCommandUnjoined.
func (o *VerificationOwnership) ProveInvocationAbsent(
	ctx context.Context, task domain.TaskID, run domain.RunID, invocation domain.InvocationID,
) error {
	return o.proveInvocationAbsent(ctx, task, run, invocation, true)
}

// ProveInvocationContainersAbsent is ProveInvocationAbsent without the
// quiescence condition: a record whose command never joined passes once its
// containers are proved absent. It still refuses a live call, retries owned
// cleanup, and fails on a record it cannot read. Only the operator-confirmed
// release (freesided release-verification-memory -command-gone) calls it:
// the operator has confirmed the old CLI is gone, which is the one fact the
// daemon cannot prove for a call a crash interrupted. Every automatic release
// keeps ProveInvocationAbsent's strict rule.
func (o *VerificationOwnership) ProveInvocationContainersAbsent(
	ctx context.Context, task domain.TaskID, run domain.RunID, invocation domain.InvocationID,
) error {
	return o.proveInvocationAbsent(ctx, task, run, invocation, false)
}

func (o *VerificationOwnership) proveInvocationAbsent(
	ctx context.Context, task domain.TaskID, run domain.RunID, invocation domain.InvocationID, requireJoined bool,
) error {
	if task == "" || run == "" || invocation == "" {
		return domain.ErrEmptyID
	}
	dir := filepath.Join(o.root, verificationRunKey(run))
	names, err := o.quiescentRecordNames(dir, invocation)
	if err != nil {
		return errors.Join(ErrVerificationAbsenceUnproven, err)
	}
	proof := o.proveRecordsAbsent(ctx, dir, names, task, run, invocation, requireJoined)
	// A call of the invocation that began while the proof ran is one the
	// snapshot missed.
	after, err := o.quiescentRecordNames(dir, invocation)
	for _, name := range after {
		if err != nil {
			break
		}
		if slices.Contains(names, name) {
			continue
		}
		var record verificationOwner
		if record, err = readVerificationRecord(dir, name, task, run); err == nil && record.InvocationID == invocation {
			err = errors.New("verification call began during the absence proof")
		}
	}
	if proof = errors.Join(proof, err); proof != nil {
		return errors.Join(ErrVerificationAbsenceUnproven, proof)
	}
	return nil
}

// quiescentRecordNames snapshots the run's records under the lock begin
// writes them under, refusing while a call of the invocation is live: its
// container is still running, and owned cleanup would delete it.
func (o *VerificationOwnership) quiescentRecordNames(dir string, invocation domain.InvocationID) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for path, call := range o.live {
		if filepath.Dir(path) == dir && call.invocation == invocation {
			return nil, fmt.Errorf("verification %s has a live call", invocation)
		}
	}
	names, err := verificationRecordNames(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return names, err
}

func verificationRecordNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// proveRecordsAbsent retries exact owned cleanup for each named record and
// proves its containers absent and, when requireJoined, its command joined.
// An empty invocation selects every record; otherwise a consistent record of
// another invocation is skipped, while an inconsistent one still fails the
// proof.
func (o *VerificationOwnership) proveRecordsAbsent(
	ctx context.Context, dir string, names []string,
	task domain.TaskID, run domain.RunID, invocation domain.InvocationID, requireJoined bool,
) error {
	var pending error
	room := &ProjectImageRoom{runtime: o.runtime}
	for _, name := range names {
		record, err := readVerificationRecord(dir, name, task, run)
		if err != nil {
			pending = errors.Join(pending, err)
			continue
		}
		if invocation != "" && record.InvocationID != invocation {
			continue
		}
		cidPath := filepath.Join(dir, strings.TrimSuffix(name, ".json")+".cid")
		id, idErr := readVerificationContainerID(cidPath)
		if idErr != nil && !errors.Is(idErr, os.ErrNotExist) {
			pending = errors.Join(pending, idErr)
		}
		cleanup := errors.Join(
			room.cleanupOwnedContainers(ctx, id, record.Owner),
			room.verifyOwnedContainersAbsent(ctx, id, record.Owner))
		if cleanup != nil {
			// Name what the operator has to remove: the container the call
			// recorded, or its owner label when a crash came before the record.
			if id != "" {
				cleanup = fmt.Errorf("verification %s container %s: %w", record.InvocationID, id, cleanup)
			} else {
				cleanup = fmt.Errorf("verification %s containers labeled %s=%s: %w",
					record.InvocationID, record.Owner.Key, record.Owner.Value, cleanup)
			}
		}
		pending = errors.Join(pending, cleanup)
		if requireJoined && !record.CommandJoined {
			pending = errors.Join(pending, fmt.Errorf("verification %s %w", record.InvocationID, ErrVerificationCommandUnjoined))
		}
	}
	return pending
}

// readVerificationRecord reads one record of the run's directory and checks
// it names this task and run and the owner label its file name binds.
func readVerificationRecord(dir, name string, task domain.TaskID, run domain.RunID) (verificationOwner, error) {
	body, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- directory entry beneath the hashed run's private ownership directory.
	var record verificationOwner
	if err == nil {
		err = json.Unmarshal(body, &record)
	}
	if err != nil || record.Version != 1 || record.TaskID != task || record.RunID != run || record.InvocationID == "" ||
		record.Owner.Key != ownershipLabelKey || record.Owner.Value == "" ||
		name != verificationRunKey(domain.RunID(record.Owner.Value))+".json" {
		return verificationOwner{}, errors.Join(err, errors.New("verification ownership record is inconsistent"))
	}
	return record, nil
}

func (r *ProjectImageRoom) verifyOwnedContainersAbsent(ctx context.Context, id string, owner Label) error {
	var proof error
	if id != "" {
		proof = (runtimeOps{rt: r.runtime}).verifyContainerAbsent(ctx, id, objectClaim{attempted: true}, owner, CheckTeardown)
		if got, err := r.runtime.Inspect(ctx, id); err == nil {
			if got.ID != id || !got.LabelsObserved || slices.Contains(got.Labels, owner) {
				proof = errors.Join(proof, errors.New("verification container survived cleanup or has unprovable ownership"))
			}
		}
	}
	containers, err := r.runtime.ListContainers(ctx)
	if err != nil {
		return errors.Join(proof, err)
	}
	for _, candidate := range containers {
		labels, observed := candidate.Labels, candidate.LabelsObserved
		if !observed {
			got, err := r.runtime.Inspect(ctx, candidate.ID)
			if err == nil && got.ID == candidate.ID && got.LabelsObserved {
				labels, observed = got.Labels, true
			}
		}
		if !observed || slices.Contains(labels, owner) {
			proof = errors.Join(proof, errors.New("verification container absence is unproven"))
		}
	}
	return proof
}
