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
	cancel context.CancelFunc
	done   chan struct{}
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
	call := verificationCall{cancel: cancel, done: make(chan struct{})}
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var pending error
	room := &ProjectImageRoom{runtime: o.runtime}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		body, err := os.ReadFile(path) // #nosec G304 -- directory entry beneath the hashed run's private ownership directory.
		var record verificationOwner
		if err == nil {
			err = json.Unmarshal(body, &record)
		}
		if err != nil || record.Version != 1 || record.TaskID != task || record.RunID != run || record.InvocationID == "" ||
			record.Owner.Key != ownershipLabelKey || record.Owner.Value == "" ||
			entry.Name() != verificationRunKey(domain.RunID(record.Owner.Value))+".json" {
			pending = errors.Join(pending, err, errors.New("verification ownership record is inconsistent"))
			continue
		}
		cidPath := strings.TrimSuffix(path, ".json") + ".cid"
		id, idErr := readVerificationContainerID(cidPath)
		if idErr != nil && !errors.Is(idErr, os.ErrNotExist) {
			pending = errors.Join(pending, idErr)
		}
		pending = errors.Join(pending, room.cleanupOwnedContainers(ctx, id, record.Owner))
		pending = errors.Join(pending, room.verifyOwnedContainersAbsent(ctx, id, record.Owner))
		if !record.CommandJoined {
			pending = errors.Join(pending, fmt.Errorf("verification %s host command has no quiescence proof", record.InvocationID))
		}
	}
	return pending
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
