package cordon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// CheckRequest is one check launch: one recipe command run in a fresh
// worktree under the writer's rule file, with no agent and no credential
// (docs/plan.md §5.7, Cordoned Verification). It has no token field, and
// its run never exports.
type CheckRequest struct {
	// RunID names the launch's directory under Config.RunsRoot, as a
	// writer's does; each check is one command in a run of its own.
	RunID      string
	Repository string
	BaseRef    string
	BaseSHA    string
	// Argv is the command, which the daemon takes from the trusted recipe.
	// A bare name in Argv[0] resolves through the launch's PATH.
	Argv []string
	// MaxOutputBytes bounds the output a result carries: the last
	// MaxOutputBytes bytes.
	MaxOutputBytes int64
	// RequireProof runs the attached suite's pre-job probe first, as a
	// writer's LaunchRequest.RequireProof does.
	RequireProof bool
}

// CheckResult is how a check ended, once no process from it is alive.
type CheckResult struct {
	// ExitCode is the command's exit status, -1 when a signal ended it or
	// recovery ended the run lost.
	ExitCode int
	EndedBy  EndedBy
	// Output is the end of the command's stdout and stderr, in one
	// stream. The bytes are the command's, so they are untrusted.
	Output []byte
	// Truncated is set when the command wrote more than MaxOutputBytes.
	Truncated bool
	// Reason says why a lost run is lost.
	Reason string
}

// CheckRecoverRequest names the check to recover after a restart.
type CheckRecoverRequest struct {
	RunID          string
	MaxOutputBytes int64
}

// Check runs one check launch and returns once no process from it is
// alive, as Launch does: it seeds the worktree as a writer's is seeded and
// starts the launch's supervisor, which runs the command under the rule
// file with the worktree as its working directory, the launch's
// environment, stdin on /dev/null, and no descriptor 3. Every ending runs
// the run-end proof, and a result comes back only once the end record
// exists; a check whose end can't be proven returns a *BlockedError.
// Canceling ctx cancels the run and keeps waiting for its end; the stall
// limit ends a silent command as it ends a silent agent.
func (b *Backend) Check(ctx context.Context, req CheckRequest) (CheckResult, error) {
	if err := req.validate(); err != nil {
		return CheckResult{}, err
	}
	if req.RequireProof {
		if err := b.probeBeforeJob(ctx); err != nil {
			return CheckResult{}, err
		}
	}
	l, err := b.claimLaunch(req.RunID)
	if err != nil {
		return CheckResult{}, err
	}
	if err := b.seed(ctx, nil, req.Repository, req.BaseRef, req.BaseSHA, l); err != nil {
		return CheckResult{}, err
	}
	res, err := b.supervise(ctx, l, supervisorRequest{
		RunID: req.RunID, Kind: kindCheck,
		Repository: req.Repository, BaseRef: req.BaseRef, BaseSHA: req.BaseSHA,
		Program: supervisedProcess{
			Argv: confinedArgv(b.cfg, l, req.Argv[0], req.Argv[1:]...),
			Dir:  l.worktree,
			Env:  launchEnv(b.cfg, l),
		},
	}, nil)
	if err != nil {
		return CheckResult{}, err
	}
	return checkResultFor(l, endRecord{EndedBy: res.EndedBy, ExitCode: res.ExitCode}, "", req.MaxOutputBytes)
}

func (r CheckRequest) validate() error {
	if !plainRunID(r.RunID) {
		return fmt.Errorf("cordon: run id %q must be one plain path component", r.RunID)
	}
	if r.Repository == "" || r.BaseRef == "" || r.BaseSHA == "" {
		return errors.New("cordon: a check needs its repository, base ref, and base commit")
	}
	if len(r.Argv) == 0 || r.Argv[0] == "" {
		return errors.New("cordon: a check needs a command")
	}
	for _, arg := range r.Argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("cordon: a check's command holds a NUL byte")
		}
	}
	if r.MaxOutputBytes <= 0 {
		return errors.New("cordon: a check needs a positive output bound")
	}
	return nil
}

// RecoverCheck finds how a check ended after a restart, waiting for it
// while its supervisor is alive, through the states Recover handles: a run
// no supervisor claimed ends lost, a run whose supervisor died ends lost
// once the run-end proof passes, and a recorded end is returned as
// recorded, a cancellation marker outranking a clean exit. It never
// exports, and it refuses a run whose supervisor request is another
// kind's. Canceling ctx stops the wait and leaves the run running.
func (b *Backend) RecoverCheck(ctx context.Context, req CheckRecoverRequest) (CheckResult, error) {
	if err := b.attached(); err != nil {
		return CheckResult{}, err
	}
	if req.MaxOutputBytes <= 0 {
		return CheckResult{}, errors.New("cordon: a check needs a positive output bound")
	}
	l, err := b.existingRun(ctx, req.RunID)
	if err != nil {
		return CheckResult{}, err
	}
	// A daemon that died before it wrote the request left a run nothing
	// started, which ends lost below.
	kind, err := readRequestKind(l, req.RunID)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return CheckResult{}, err
	case kind != kindCheck:
		return CheckResult{}, fmt.Errorf("cordon: %s is a %s launch, not a check", req.RunID, kind)
	}
	end, reason, err := b.awaitEnd(ctx, l, req.RunID)
	if err != nil {
		return CheckResult{}, err
	}
	return checkResultFor(l, end, reason, req.MaxOutputBytes)
}

// checkResultFor is a check's result from its end and its output.
func checkResultFor(l layout, end endRecord, reason string, maxBytes int64) (CheckResult, error) {
	res := CheckResult{ExitCode: end.ExitCode, EndedBy: end.EndedBy}
	if end.EndedBy == EndedByLost {
		res.Reason = lostReason(reason)
	}
	var err error
	res.Output, res.Truncated, err = readTail(l.transcript, maxBytes)
	return res, err
}

// readTail returns the last maxBytes bytes of the file at path, which is
// ctl/transcript, and whether the file is longer. The launch can't reach
// ctl/, so the path is the supervisor's file; a run that ended before its
// supervisor created it has no output.
func readTail(path string, maxBytes int64) ([]byte, bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the transcript in the launch's own control directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cordon: open the check's output: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("cordon: inspect the check's output: %w", err)
	}
	truncated := info.Size() > maxBytes
	if truncated {
		if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
			return nil, false, fmt.Errorf("cordon: seek the check's output: %w", err)
		}
	}
	body, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, false, fmt.Errorf("cordon: read the check's output: %w", err)
	}
	return body, truncated, nil
}
