package daemoncompose

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/cordonhost"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// cordonSuite is what cordonProofs runs on a host: the full suite and the
// pre-job probe. A suite in this process and a host launcher's client both
// offer it.
type cordonSuite interface {
	Full(context.Context) error
	ProbeBeforeJob(context.Context) error
}

var (
	_ cordonSuite = (*cordon.Suite)(nil)
	_ cordonSuite = (*cordonhost.Client)(nil)
)

// cordonProofs runs the cordoned suite on each execution host it holds and
// keeps each host's proof condition (plan §5.7, Conformance per host). It
// runs the full suite on a host when the host joins, on the doctor
// schedule, and before a job when the host's configuration digest has
// changed since its last pass; the pre-job probe runs on the job's own
// host. Each record a host's suite writes is bound to that host by the
// recorder Recorder hands out, so a pass on one host admits nothing on
// another.
//
// It sets domain.ExecutionHostProofNotCurrent on a host exactly when the
// host's newest cordoned record isn't a pass, after each full pass and each
// failed probe ends; a recheck under way doesn't set it. Only cordoned
// records count, and a host it holds no
// suite for keeps whatever conditions it has.
//
// Tier A: it binds each record to the host whose suite wrote it, and
// decides when a host's proof is run again.
type cordonProofs struct {
	store  *store.Store
	now    func() time.Time
	logger *slog.Logger

	mu    sync.Mutex
	hosts map[domain.ExecutionHostID]*hostProof
}

// hostProof is one host's suite and what the runner knows of its passes.
type hostProof struct {
	suite cordonSuite
	// digest reads the host's current configuration digest.
	digest func() domain.Digest

	// pass serializes the host's full passes, so two never overlap on a
	// host, and guards provedUnder.
	pass sync.Mutex
	// provedUnder is the configuration digest the host's last completed
	// full pass ran under, whatever its outcome; empty until one completes.
	provedUnder domain.Digest

	// settle serializes the host's condition updates apart from its
	// passes, so a probe's failure is settled without waiting out a pass,
	// and each update reads the newest record after the one before it.
	settle sync.Mutex
}

func newCordonProofs(st *store.Store, logger *slog.Logger) *cordonProofs {
	return &cordonProofs{store: st, now: func() time.Time { return time.Now().UTC() }, logger: logger, hosts: map[domain.ExecutionHostID]*hostProof{}}
}

// Recorder is the recorder a suite on host writes its records through.
func (p *cordonProofs) Recorder(host domain.ExecutionHostID) StoreConformanceRecorder {
	return StoreConformanceRecorder{Store: p.store, Host: host}
}

// Add holds suite for host, whose current configuration digest digest
// reads, and runs its full pass. The suite must record through
// Recorder(host). A failed pass leaves the host undeclared and is logged;
// Add returns only a record the store didn't take, a condition it couldn't
// write, or ctx's end.
func (p *cordonProofs) Add(
	ctx context.Context, host domain.ExecutionHostID, suite cordonSuite, digest func() domain.Digest,
) error {
	h := &hostProof{suite: suite, digest: digest}
	p.mu.Lock()
	if _, held := p.hosts[host]; held {
		p.mu.Unlock()
		return fmt.Errorf("cordon proofs: host %q already has a suite", host)
	}
	p.hosts[host] = h
	p.mu.Unlock()
	return p.full(ctx, host, h)
}

// RunScheduled runs the full pass on every host it holds, one host at a
// time in id order, as the scheduled doctor pass does. One host's failed
// pass doesn't stop the others; it returns, joined, what Add would.
func (p *cordonProofs) RunScheduled(ctx context.Context) error {
	p.mu.Lock()
	ids := slices.SortedFunc(maps.Keys(p.hosts), cmp.Compare[domain.ExecutionHostID])
	hosts := make([]*hostProof, len(ids))
	for i, id := range ids {
		hosts[i] = p.hosts[id]
	}
	p.mu.Unlock()
	var errs []error
	for i, id := range ids {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		errs = append(errs, p.full(ctx, id, hosts[i]))
	}
	return errors.Join(errs...)
}

// PreJob proves host before a job runs there: it reruns the full pass when
// the host's configuration digest differs from the one its last pass ran
// under, then runs the pre-job probe. A failed probe refuses the job and
// settles the host's proof condition.
func (p *cordonProofs) PreJob(ctx context.Context, host domain.ExecutionHostID) error {
	h, err := p.host(host)
	if err != nil {
		return err
	}
	// The digest is compared and the pass run under one hold of the pass
	// lock, so jobs that see the same change rerun it once.
	h.pass.Lock()
	if h.provedUnder != h.digest() {
		err = p.passLocked(ctx, host, h)
	}
	h.pass.Unlock()
	if err != nil {
		return err
	}
	probeErr := h.suite.ProbeBeforeJob(ctx)
	if probeErr == nil {
		return nil
	}
	return errors.Join(probeErr, p.settle(ctx, host, h, false))
}

// Settle brings host's proof condition in line with its newest cordoned
// record, for a caller that saw a check fail outside the runner, such as
// the probe a launch that requires proof runs again.
func (p *cordonProofs) Settle(ctx context.Context, host domain.ExecutionHostID) error {
	h, err := p.host(host)
	if err != nil {
		return err
	}
	return p.settle(ctx, host, h, false)
}

func (p *cordonProofs) host(host domain.ExecutionHostID) (*hostProof, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hosts[host]
	if !ok {
		return nil, fmt.Errorf("cordon proofs: no suite for host %q", host)
	}
	return h, nil
}

// full runs host's full pass and settles its proof condition. A failed
// pass is logged and returns nil; it returns a record the store didn't
// take, a condition it couldn't write, or ctx's end.
func (p *cordonProofs) full(ctx context.Context, host domain.ExecutionHostID, h *hostProof) error {
	h.pass.Lock()
	defer h.pass.Unlock()
	return p.passLocked(ctx, host, h)
}

// passLocked is full's body, run with h.pass held.
func (p *cordonProofs) passLocked(ctx context.Context, host domain.ExecutionHostID, h *hostProof) error {
	digest := h.digest()
	err := h.suite.Full(ctx)
	settleErr := p.settle(ctx, host, h, true)
	switch {
	case err == nil:
	case errors.Is(err, cordon.ErrConformanceRecord):
		return errors.Join(err, settleErr)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	// The pass completed under digest, whatever its outcome, so a job
	// reruns it only when the configuration changes again.
	h.provedUnder = digest
	if settleErr != nil {
		return settleErr
	}
	if err == nil {
		// The digest is what `freesided doctor -cordon-configuration-digest`
		// takes, so the log is where an operator finds it.
		p.log(slog.LevelInfo, "the cordoned class is proven", "host", host, "configuration_digest", digest)
		return nil
	}
	p.log(slog.LevelWarn, "the cordoned class declares nothing on this host; unattended admission refuses it "+
		"until a pass succeeds", "host", host, "configuration_digest", digest, "reason", err.Error())
	return nil
}

// settle sets host's proof condition when its newest cordoned record isn't
// a pass and clears it when it is. It reads first and writes only when the
// condition changes, because every Write moves the server revision.
//
// Outside a pass (endOfPass false), a superseded newest record means a
// recheck is under way, and the pass settles the host when it ends, so
// settle leaves the condition as it is; a probe refused because a pass
// began records nothing, which leaves the marker newest. Every pass
// settles at its end, a canceled one included, since it runs without ctx's
// cancel, as the suite records its own steps; a canceled pass's marker
// stays newest and sets the condition until the next pass.
func (p *cordonProofs) settle(ctx context.Context, host domain.ExecutionHostID, h *hostProof, endOfPass bool) error {
	ctx = context.WithoutCancel(ctx)
	h.settle.Lock()
	defer h.settle.Unlock()
	var (
		newest      domain.ConformanceOutcome
		found, held bool
	)
	if err := p.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		newest, found, held, err = proofCondition(ctx, tx, host)
		return err
	}); err != nil {
		return fmt.Errorf("read host %q's proof: %w", host, err)
	}
	if !endOfPass && found && newest == domain.ConformanceSuperseded {
		return nil
	}
	notCurrent := !found || newest != domain.ConformancePassed
	if notCurrent == held {
		return nil
	}
	if err := p.store.Write(ctx, func(tx *store.WriteTx) error {
		if notCurrent {
			_, err := tx.SetExecutionHostCondition(ctx, host, domain.ExecutionHostProofNotCurrent, p.now())
			return err
		}
		_, err := tx.ClearExecutionHostCondition(ctx, host, domain.ExecutionHostProofNotCurrent)
		return err
	}); err != nil {
		return fmt.Errorf("settle host %q's proof condition: %w", host, err)
	}
	return nil
}

// proofCondition reads the outcome of host's newest cordoned record, if it
// has one, and whether the host carries the proof condition now.
func proofCondition(
	ctx context.Context, tx *store.ReadTx, host domain.ExecutionHostID,
) (newest domain.ConformanceOutcome, found, held bool, err error) {
	record, found, err := tx.LatestBackendConformance(ctx, host, domain.BackendCordonedHostProcess)
	if err != nil {
		return "", false, false, err
	}
	h, err := tx.ExecutionHost(ctx, host)
	if err != nil {
		return "", false, false, err
	}
	held = slices.ContainsFunc(h.Conditions, func(o domain.HostObservedCondition) bool {
		return o.Condition == domain.ExecutionHostProofNotCurrent
	})
	return record.Outcome, found, held, nil
}

func (p *cordonProofs) log(level slog.Level, msg string, args ...any) {
	if p.logger != nil {
		p.logger.Log(context.Background(), level, msg, args...)
	}
}
