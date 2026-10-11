package wardstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

const (
	readSentinelToken = "sk-ant-oat-READ-SENTINEL-0123456789abcdefghijklmnopqrstuv" //nolint:gosec // G101: test fixture
	readIdentity      = domain.AuthIdentityID("claude-main")
	readVolume        = "claude-main-auth"
)

// credentialReadRig is a probe rig's store and enrollments with a credential
// reader whose volume reads are test-owned.
type credentialReadRig struct {
	*probeRig
	volumes map[string]func() (ward.SetupTokenRead, error)
	read    []string
}

func newCredentialReadRig(t *testing.T) *credentialReadRig {
	t.Helper()
	return &credentialReadRig{
		probeRig: newProbeRig(t),
		volumes:  map[string]func() (ward.SetupTokenRead, error){},
	}
}

func (r *credentialReadRig) reader() wardstore.CredentialReader {
	return wardstore.CredentialReader{
		Store: r.st,
		ReadSetupToken: func(_ context.Context, volume string) (ward.SetupTokenRead, error) {
			r.read = append(r.read, volume)
			answer, ok := r.volumes[volume]
			if !ok {
				r.t.Fatalf("unexpected credential read of %q", volume)
			}
			return answer()
		},
		Now: func() time.Time { return r.clock },
	}
}

func (r *credentialReadRig) enrollment(id domain.ClientEnrollmentID) domain.ClientEnrollment {
	r.t.Helper()
	ctx := context.Background()
	var enrollment domain.ClientEnrollment
	if err := r.st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		enrollment, err = tx.GetClientEnrollment(ctx, id)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return enrollment
}

// stored answers a volume read with the sentinel token under the given
// digests.
func stored(t *testing.T, tokenDigest, treeDigest domain.Digest) func() (ward.SetupTokenRead, error) {
	t.Helper()
	token, err := ward.NewSetupToken([]byte(readSentinelToken))
	if err != nil {
		t.Fatal(err)
	}
	return func() (ward.SetupTokenRead, error) {
		return ward.SetupTokenRead{Token: token, TokenDigest: tokenDigest, TreeDigest: treeDigest}, nil
	}
}

// assertRefused checks a refusal carries its cause, hands back no token, and
// names nothing the store held.
func assertRefused(t *testing.T, token ward.SetupToken, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("read = %v, want %v", err, want)
	}
	if len(token.Reveal()) != 0 {
		t.Error("a refused read handed back a token")
	}
	if err != nil && strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), readSentinelToken) {
		t.Error("a refusal carries the token")
	}
}

// TestCredentialReaderReturnsTheRecordedStore: a generation recorded under
// either digest convention (the token hash from auth add, the tree digest
// from auth adopt) hands back the token its volume holds, with and without
// the hold.
func TestCredentialReaderReturnsTheRecordedStore(t *testing.T) {
	tokenHash, tree := probeDigest("token bytes"), probeDigest("volume tree")
	for name, recorded := range map[string]domain.Digest{"auth add": tokenHash, "auth adopt": tree} {
		t.Run(name, func(t *testing.T) {
			r := newCredentialReadRig(t)
			enrollment := r.enrollment(r.enrollClaude(readIdentity, recorded))
			r.volumes[readVolume] = stored(t, tokenHash, tree)
			reader := r.reader()
			for how, read := range map[string]func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error){
				"held": reader.Read, "unheld": reader.ReadUnheld,
			} {
				token, err := read(context.Background(), enrollment.ID, 1)
				if err != nil || string(token.Reveal()) != readSentinelToken {
					t.Errorf("%s read = %v, want the stored token", how, err)
				}
			}
			if len(r.read) != 2 || r.read[0] != readVolume || r.read[1] != readVolume {
				t.Errorf("volumes read = %v, want the generation's volume once per read", r.read)
			}
		})
	}
}

// TestCredentialReaderRefuses: every read that cannot prove it took the
// asked-for generation's recorded store out from under a live hold returns
// its cause and no token.
func TestCredentialReaderRefuses(t *testing.T) {
	tokenHash, tree := probeDigest("token bytes"), probeDigest("volume tree")
	ctx := context.Background()

	t.Run("not a Claude Code setup-token enrollment", func(t *testing.T) {
		r := newCredentialReadRig(t)
		codex := r.enrollCodex("codex-main", "/codex/auth.json", tokenHash)
		token, err := r.reader().Read(ctx, codex, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadUnsupported)
		token, err = r.reader().ReadUnheld(ctx, codex, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadUnsupported)
		if len(r.read) != 0 {
			t.Errorf("volumes read = %v, want none", r.read)
		}
	})

	t.Run("an enrollment the store does not hold", func(t *testing.T) {
		r := newCredentialReadRig(t)
		r.enrollClaude(readIdentity, tokenHash)
		for _, read := range []func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error){
			r.reader().Read, r.reader().ReadUnheld,
		} {
			token, err := read(ctx, "claude-other/claude_code", 1)
			if err == nil || len(token.Reveal()) != 0 {
				t.Fatalf("read of an unknown enrollment = %v with a token", err)
			}
		}
		if len(r.read) != 0 {
			t.Errorf("volumes read = %v, want none", r.read)
		}
	})

	t.Run("live mutation lease", func(t *testing.T) {
		r := newCredentialReadRig(t)
		enrollment := r.enrollment(r.enrollClaude(readIdentity, tokenHash))
		r.volumes[readVolume] = stored(t, tokenHash, tree)
		lease, err := r.adapters.Leaser.Acquire(ctx, readIdentity, "refresh-in-flight", r.clock, r.clock.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		token, err := r.reader().Read(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadMutationLeaseLive)
		if len(r.read) != 0 {
			t.Errorf("volumes read = %v, want none under a live mutation lease", r.read)
		}
		// The read neither took over nor disturbed the mutator's lease.
		current, err := r.adapters.Leaser.Get(ctx, readIdentity)
		if err != nil || current.Holder != lease.Holder || current.Fence != lease.Fence || !current.HeldAt(r.clock) {
			t.Errorf("mutation lease after the read = %+v, %v; want the mutator's own", current, err)
		}
	})

	t.Run("another generation is current", func(t *testing.T) {
		r := newCredentialReadRig(t)
		id := r.enrollClaude(readIdentity, tokenHash)
		enrollment := r.enrollment(id)
		r.volumes[readVolume] = stored(t, tokenHash, tree)
		if err := r.appendGeneration(readIdentity, id, tokenHash, r.clock); err != nil {
			t.Fatal(err)
		}
		r.clock = r.clock.Add(time.Hour)
		token, err := r.reader().Read(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadGenerationChanged)
		token, err = r.reader().ReadUnheld(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadGenerationChanged)
		if len(r.read) != 0 {
			t.Errorf("volumes read = %v, want none for a superseded generation", r.read)
		}
		if token, err := r.reader().Read(ctx, enrollment.ID, 2); err != nil || string(token.Reveal()) != readSentinelToken {
			t.Errorf("read of the current generation = %v, want its token", err)
		}
	})

	t.Run("generation changed during the read", func(t *testing.T) {
		r := newCredentialReadRig(t)
		id := r.enrollClaude(readIdentity, tokenHash)
		enrollment := r.enrollment(id)
		answer := stored(t, tokenHash, tree)
		r.volumes[readVolume] = func() (ward.SetupTokenRead, error) {
			// A writer whose clock runs ahead of the reader's sees the hold
			// as over and appends, while the reader's clock still shows it
			// live.
			ahead := r.clock.Add(2 * wardstore.DefaultCredentialReadHold)
			if err := r.appendGeneration(readIdentity, id, tokenHash, ahead); err != nil {
				t.Fatal(err)
			}
			return answer()
		}
		token, err := r.reader().Read(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadGenerationChanged)
	})

	t.Run("generation changed during an unheld read", func(t *testing.T) {
		r := newCredentialReadRig(t)
		id := r.enrollClaude(readIdentity, tokenHash)
		enrollment := r.enrollment(id)
		answer := stored(t, tokenHash, tree)
		r.volumes[readVolume] = func() (ward.SetupTokenRead, error) {
			if err := r.appendGeneration(readIdentity, id, tokenHash, r.clock); err != nil {
				t.Fatal(err)
			}
			return answer()
		}
		token, err := r.reader().ReadUnheld(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadGenerationChanged)
	})

	t.Run("read hold ended during the read", func(t *testing.T) {
		r := newCredentialReadRig(t)
		enrollment := r.enrollment(r.enrollClaude(readIdentity, tokenHash))
		answer := stored(t, tokenHash, tree)
		r.volumes[readVolume] = func() (ward.SetupTokenRead, error) {
			r.clock = r.clock.Add(wardstore.DefaultCredentialReadHold)
			return answer()
		}
		token, err := r.reader().Read(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadHoldEnded)
	})

	t.Run("store matches neither recorded digest", func(t *testing.T) {
		r := newCredentialReadRig(t)
		enrollment := r.enrollment(r.enrollClaude(readIdentity, probeDigest("recorded")))
		r.volumes[readVolume] = stored(t, tokenHash, tree)
		token, err := r.reader().Read(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadDigestMismatch)
		token, err = r.reader().ReadUnheld(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, wardstore.ErrCredentialReadDigestMismatch)
		// The read is not the integrity probe: it marks nothing.
		if marks := r.marks(enrollment.ID, 1); len(marks) != 0 {
			t.Errorf("a credential read recorded marks: %+v", marks)
		}
	})

	t.Run("volume read failed", func(t *testing.T) {
		r := newCredentialReadRig(t)
		enrollment := r.enrollment(r.enrollClaude(readIdentity, tokenHash))
		r.volumes[readVolume] = func() (ward.SetupTokenRead, error) {
			return ward.SetupTokenRead{}, ward.ErrCredentialStoreAbsent
		}
		token, err := r.reader().Read(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, ward.ErrCredentialStoreAbsent)
		token, err = r.reader().ReadUnheld(ctx, enrollment.ID, 1)
		assertRefused(t, token, err, ward.ErrCredentialStoreAbsent)
	})
}

// TestCredentialReaderReadsOnlyUnderTheReadHold: while the volume is read
// its identity's read hold is live, so no mutation can start; once the read
// ends, on success or refusal, the hold is released; and the read never
// holds the mutation lease.
func TestCredentialReaderReadsOnlyUnderTheReadHold(t *testing.T) {
	tokenHash, tree := probeDigest("token bytes"), probeDigest("volume tree")
	ctx := context.Background()
	for name, recorded := range map[string]domain.Digest{"success": tokenHash, "refusal": probeDigest("recorded")} {
		t.Run(name, func(t *testing.T) {
			r := newCredentialReadRig(t)
			enrollment := r.enrollment(r.enrollClaude(readIdentity, recorded))
			acquireMutation := func() error {
				_, err := r.adapters.Leaser.Acquire(ctx, readIdentity, "mutator", r.clock, r.clock.Add(time.Minute))
				return err
			}
			answer := stored(t, tokenHash, tree)
			r.volumes[readVolume] = func() (ward.SetupTokenRead, error) {
				var held *store.ReadHeldError
				if err := acquireMutation(); !errors.As(err, &held) {
					t.Errorf("a mutation started during the read: %v", err)
				}
				if lease, err := r.adapters.Leaser.Get(ctx, readIdentity); err != nil || lease.HeldAt(r.clock) {
					t.Errorf("mutation lease during the read = %+v, %v; want none live", lease, err)
				}
				return answer()
			}
			_, _ = r.reader().Read(ctx, enrollment.ID, 1)
			if len(r.read) != 1 {
				t.Fatalf("volumes read = %v, want exactly one", r.read)
			}
			if err := acquireMutation(); err != nil {
				t.Errorf("a mutation cannot start after the read, so the hold was not released: %v", err)
			}
		})
	}
}

// TestCredentialReaderUnheldTakesNoHold: the unheld read writes nothing, so
// it works on the read-only store preflight opens.
func TestCredentialReaderUnheldTakesNoHold(t *testing.T) {
	r := newCredentialReadRig(t)
	tokenHash := probeDigest("token bytes")
	enrollment := r.enrollment(r.enrollClaude(readIdentity, tokenHash))
	ctx := context.Background()
	answer := stored(t, tokenHash, probeDigest("volume tree"))
	r.volumes[readVolume] = func() (ward.SetupTokenRead, error) {
		if _, err := r.adapters.Leaser.Acquire(ctx, readIdentity, "mutator", r.clock, r.clock.Add(time.Minute)); err != nil {
			t.Errorf("an unheld read blocked a mutation, so it took a hold: %v", err)
		}
		return answer()
	}
	if _, err := r.reader().ReadUnheld(ctx, enrollment.ID, 1); err != nil {
		t.Errorf("unheld read = %v", err)
	}
}

// TestCredentialReaderCancelsAReadThatOutlivesTheHold: the read's context
// ends with the hold window, so a volume read that hangs is cancelled instead
// of reading a store nothing holds.
func TestCredentialReaderCancelsAReadThatOutlivesTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newCredentialReadRig(t)
		enrollment := r.enrollment(r.enrollClaude(readIdentity, probeDigest("token bytes")))
		reader := r.reader()
		reader.HoldDuration = 30 * time.Second
		// The bubble's clock starts before the fixture's lease history, so
		// the reader's clock is the fixture instant plus bubble time elapsed.
		start := time.Now()
		reader.Now = func() time.Time { return r.clock.Add(time.Since(start)) }
		reader.ReadSetupToken = func(ctx context.Context, _ string) (ward.SetupTokenRead, error) {
			<-ctx.Done()
			return ward.SetupTokenRead{}, ctx.Err()
		}
		token, err := reader.Read(context.Background(), enrollment.ID, 1)
		assertRefused(t, token, err, context.DeadlineExceeded)
		if waited := time.Since(start); waited != 30*time.Second {
			t.Errorf("the read ran %s, want it cancelled at the 30s hold window", waited)
		}
	})
}

func TestCredentialReaderRequiresItsDependencies(t *testing.T) {
	r := newCredentialReadRig(t)
	enrollment := r.enrollment(r.enrollClaude(readIdentity, probeDigest("token bytes")))
	r.volumes[readVolume] = stored(t, probeDigest("token bytes"), "")
	ctx := context.Background()
	for name, mutate := range map[string]func(*wardstore.CredentialReader){
		"store":         func(c *wardstore.CredentialReader) { c.Store = nil },
		"volume reader": func(c *wardstore.CredentialReader) { c.ReadSetupToken = nil },
		"clock":         func(c *wardstore.CredentialReader) { c.Now = nil },
		"negative hold": func(c *wardstore.CredentialReader) { c.HoldDuration = -time.Second },
	} {
		reader := r.reader()
		mutate(&reader)
		if _, err := reader.Read(ctx, enrollment.ID, 1); err == nil {
			t.Errorf("a reader with no usable %s read a credential", name)
		}
		// The unheld read needs no clock and no hold.
		if _, err := reader.ReadUnheld(ctx, enrollment.ID, 1); (err == nil) != (name == "clock" || name == "negative hold") {
			t.Errorf("an unheld reader with no usable %s = %v", name, err)
		}
	}
}
