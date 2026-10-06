package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// decided returns a declined or deferred disposition for an external finding
// bound to a stored adjudication that covers the round's record finding and
// routes that external finding accordingly.
func (f externalCycleFixture) decided(
	t *testing.T, finding domain.Finding, disposition domain.ReviewDisposition,
) domain.ExternalFindingDisposition {
	t.Helper()
	route := domain.RouteDecline
	if disposition == domain.ReviewDispositionDeferred {
		route = domain.RouteDefer
	}
	artifact := f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
		f.internal.ID: domain.RouteRemediate, finding.ID: route,
	})
	if err := f.putAdjudication(t, artifact); err != nil {
		t.Fatalf("put adjudication: %v", err)
	}
	return domain.ExternalFindingDisposition{
		FindingID: finding.ID, RunID: f.run.ID, Round: externalCycleRound,
		Disposition: disposition, Reason: "decided by adjudication",
		AdjudicationDigest: &artifact.Digest, CreatedAt: reentryAt.Add(2 * time.Hour),
	}
}

func (f externalCycleFixture) fixed(finding domain.Finding, remediation domain.InvocationID) domain.ExternalFindingDisposition {
	return domain.ExternalFindingDisposition{
		FindingID: finding.ID, RunID: f.run.ID, Round: externalCycleRound,
		Disposition: domain.ReviewDispositionFixed, Reason: "remediated in a later round",
		RemediationInvocationID: &remediation, CreatedAt: reentryAt.Add(2 * time.Hour),
	}
}

// otherRun adds a second run to the fixture's project.
func (f externalCycleFixture) otherRun(t *testing.T) domain.RunID {
	t.Helper()
	ctx := context.Background()
	other := domain.Run{ID: "run-other", ProjectID: f.run.ProjectID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	if err := f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, other) }); err != nil {
		t.Fatal(err)
	}
	return other.ID
}

func (f externalCycleFixture) put(d domain.ExternalFindingDisposition) error {
	ctx := context.Background()
	return f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutExternalFindingDisposition(ctx, d) })
}

func (f externalCycleFixture) get(d domain.ExternalFindingDisposition) (domain.ExternalFindingDisposition, error) {
	ctx := context.Background()
	var got domain.ExternalFindingDisposition
	err := f.st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetExternalFindingDisposition(ctx, d.FindingID, d.Round)
		return err
	})
	return got, err
}

func (f externalCycleFixture) list() ([]domain.ExternalFindingDisposition, error) {
	ctx := context.Background()
	var got []domain.ExternalFindingDisposition
	err := f.st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.ListExternalFindingDispositions(ctx, f.run.ID)
		return err
	})
	return got, err
}

// insertRaw writes the row with direct SQL, past the Go gate. Only the
// schema's own trigger and constraints can refuse it.
func (f externalCycleFixture) insertRaw(t *testing.T, d domain.ExternalFindingDisposition) error {
	t.Helper()
	body, err := encode(d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.st.db.ExecContext(context.Background(), putExternalFindingDispositionSQL,
		d.FindingID, d.RunID, d.Round, d.Disposition, d.Reason, d.Remediation(),
		formatTime(d.CreatedAt), reviewBodyDigest(body), body)
	return err
}

func TestExternalFindingDispositionMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0090_")
	if got := rawVersion(t, db); got != 89 {
		t.Fatalf("prior schema version = %d, want 89", got)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	assertAtHead(t, db)
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_finding_dispositions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("new disposition table contains %d rows", count)
	}
}

// TestExternalFindingDispositionAccepted: each outcome of an admitted
// external finding is written in the cycle's first round, replays, and reads
// back the same, also after the owner edits the allowlist, because the
// cycle keeps reading under the profile its authority names. The finding
// stays quarantined from review dispositions throughout.
func TestExternalFindingDispositionAccepted(t *testing.T) {
	t.Parallel()
	for name, build := range map[string]func(*testing.T, externalCycleFixture) domain.ExternalFindingDisposition{
		"fixed": func(_ *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
			return f.fixed(f.finding, f.fixedBy.InvocationID)
		},
		"declined": func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
			return f.decided(t, f.finding, domain.ReviewDispositionDeclined)
		},
		"deferred": func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
			return f.decided(t, f.second, domain.ReviewDispositionDeferred)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := seedExternalCycle(t)
			want := build(t, f)
			if err := f.put(want); err != nil {
				t.Fatalf("put: %v", err)
			}
			if err := f.put(want); err != nil {
				t.Fatalf("replay: %v", err)
			}
			changed := want
			changed.Reason = "another reason"
			if err := f.put(changed); !errors.Is(err, ErrImmutableConflict) {
				t.Fatalf("conflicting replay = %v, want ErrImmutableConflict", err)
			}
			check := func(when string) {
				t.Helper()
				got, err := f.get(want)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: get = %#v, %v; want %#v", when, got, err, want)
				}
				listed, err := f.list()
				if err != nil || !reflect.DeepEqual(listed, []domain.ExternalFindingDisposition{want}) {
					t.Fatalf("%s: list = %#v, %v; want only %#v", when, listed, err, want)
				}
			}
			check("after write")
			activateProfile(t, f.st, f.binding.Repo, f.binding.RepositoryID, reentryAt.Add(time.Hour),
				reviewerEntry(900, "maintainer"))
			check("after the allowlist drops the reviewer")

			// The quarantine holds: no review disposition names the finding,
			// and the review-disposition door still refuses it.
			if err := f.st.Write(ctx, func(tx *WriteTx) error {
				return tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
					FindingID: want.FindingID, RunID: want.RunID, Round: want.Round,
					Disposition: want.Disposition, Reason: want.Reason,
					AdjudicationDigest: want.Adjudication(), RemediationInvocationID: want.Remediation(),
					CreatedAt: want.CreatedAt,
				})
			}); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("review disposition for an external finding = %v, want ErrParentKeyMismatch", err)
			}
			if err := f.st.Read(ctx, func(tx *ReadTx) error {
				got, err := tx.ListFindingDispositions(ctx, f.run.ID)
				if err != nil || len(got) != 0 {
					t.Fatalf("review dispositions = %#v, %v; want none", got, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestExternalFindingDispositionRefused: every disposition outside an
// admitted finding's own cycle round is refused at the write door, and a row
// that names one, placed with direct SQL, fails every read. trigger says
// whether the schema itself refuses the direct insert; where it does, the
// read is proven with the trigger dropped, as if the row predated it.
func TestExternalFindingDispositionRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		build   func(*testing.T, externalCycleFixture) domain.ExternalFindingDisposition
		want    error
		trigger bool
	}{
		"unlisted reviewer": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				finding := externalFindingBy(t, f.run.ID, reentryHead1, 900, "maintainer", "PRRT_unlisted", reentryAt)
				putExternalFinding(t, f.st, finding)
				return f.fixed(finding, f.fixedBy.InvocationID)
			},
			want: domain.ErrExternalReviewNotAdmitted,
		},
		"listed reviewer on another head": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				finding := externalFindingBy(t, f.run.ID, reentryHead2, externalReviewerID, externalReviewerLogin,
					"PRRT_other_head", reentryAt)
				putExternalFinding(t, f.st, finding)
				return f.fixed(finding, f.fixedBy.InvocationID)
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"finding of another run": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				finding := externalFindingBy(t, f.otherRun(t), reentryHead1, externalReviewerID, externalReviewerLogin,
					"PRRT_other_run", reentryAt)
				putExternalFinding(t, f.st, finding)
				return f.fixed(finding, f.fixedBy.InvocationID)
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"finding of a run with the round but no cycle of its own": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				other := f.otherRun(t)
				finding := externalFindingBy(t, other, reentryHead1, externalReviewerID, externalReviewerLogin,
					"PRRT_other_run", reentryAt)
				putExternalFinding(t, f.st, finding)
				// The other run matches the fixture's cycle round for round,
				// base, and head, and has its own later round to name.
				putReentryReview(t, f.st, other, externalCycleRound, reentryBase1, reentryHead1)
				remediation := putReentryReview(t, f.st, other, 3, reentryBase1, reentryHead2)
				d := f.fixed(finding, remediation.InvocationID)
				d.RunID = other
				return d
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"finding of the review record": {
			build: func(_ *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				return f.fixed(f.internal, f.fixedBy.InvocationID)
			},
			want: domain.ErrExternalFindingInconsistent, trigger: true,
		},
		"round before the cycle": {
			build: func(_ *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				d := f.fixed(f.finding, f.fixedBy.InvocationID)
				d.Round = 1
				return d
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"round after the cycle's first": {
			build: func(_ *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				d := f.fixed(f.finding, f.otherBase.InvocationID)
				d.Round = 3
				return d
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"round on the cycle's head with no cycle": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				d := f.decided(t, f.finding, domain.ReviewDispositionDeclined)
				d.Round = 5
				return d
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"fixed by a round on another base": {
			build: func(_ *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				return f.fixed(f.finding, f.otherBase.InvocationID)
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"fixed by a round on the same head": {
			build: func(_ *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				return f.fixed(f.finding, f.sameHead.InvocationID)
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"row of a run other than the finding's": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				other := f.otherRun(t)
				// The other run has the round, so only the finding's run
				// stands between the row and the cycle it borrows.
				putReentryReview(t, f.st, other, externalCycleRound, reentryBase1, reentryHead1)
				d := f.fixed(f.finding, f.fixedBy.InvocationID)
				d.RunID = other
				return d
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"fixed by a round of another run": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				remediation := putReentryReview(t, f.st, f.otherRun(t), 3, reentryBase1, reentryHead2)
				return f.fixed(f.finding, remediation.InvocationID)
			},
			want: domain.ErrParentKeyMismatch, trigger: true,
		},
		"declined by an adjudication that omits the finding": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				d := f.decided(t, f.second, domain.ReviewDispositionDeclined)
				d.FindingID = f.finding.ID
				return d
			},
			want: domain.ErrInvalidDispositionAdjudication,
		},
		"declined by an adjudication that defers the finding": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				d := f.decided(t, f.finding, domain.ReviewDispositionDeferred)
				d.Disposition = domain.ReviewDispositionDeclined
				return d
			},
			want: domain.ErrInvalidDispositionAdjudication,
		},
		"deferred by an adjudication of another round": {
			build: func(t *testing.T, f externalCycleFixture) domain.ExternalFindingDisposition {
				later := f.adjudication(t, 3, map[domain.FindingID]domain.AdjudicationRoute{
					f.laterInternal.ID: domain.RouteRemediate,
				})
				if err := f.putAdjudication(t, later); err != nil {
					t.Fatalf("put adjudication: %v", err)
				}
				return domain.ExternalFindingDisposition{
					FindingID: f.finding.ID, RunID: f.run.ID, Round: externalCycleRound,
					Disposition: domain.ReviewDispositionDeferred, Reason: "decided elsewhere",
					AdjudicationDigest: &later.Digest, CreatedAt: reentryAt.Add(2 * time.Hour),
				}
			},
			want: domain.ErrParentKeyMismatch,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := seedExternalCycle(t)
			d := tc.build(t, f)
			if err := f.put(d); !errors.Is(err, tc.want) {
				t.Fatalf("put = %v, want %v", err, tc.want)
			}
			if got, err := f.list(); err != nil || len(got) != 0 {
				t.Fatalf("refused write left %#v, %v", got, err)
			}

			err := f.insertRaw(t, d)
			if refused := err != nil; refused != tc.trigger {
				t.Fatalf("direct insert = %v, want refused by the trigger: %t", err, tc.trigger)
			}
			if tc.trigger {
				if _, err := f.st.db.ExecContext(ctx,
					`DROP TRIGGER external_finding_disposition_requires_admitted_cycle`); err != nil {
					t.Fatal(err)
				}
				if err := f.insertRaw(t, d); err != nil {
					t.Fatalf("direct insert without the trigger: %v", err)
				}
			}
			if _, err := f.get(d); !errors.Is(err, tc.want) {
				t.Fatalf("get of the stored row = %v, want %v", err, tc.want)
			}
			if _, err := f.list(); !errors.Is(err, tc.want) {
				t.Fatalf("list over the stored row = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestExternalFindingDispositionRefusesEarlierRemediation: a round before the
// cycle cannot have remediated what the cycle answers, even on the cycle's
// base and another head. The cycle starts at round 3 here so that such a
// round can exist: the round a cycle follows is always on the cycle's head.
func TestExternalFindingDispositionRefusesEarlierRemediation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := externalCycleFixture{externalReviewFixture: seedExternalReview(t, externalReviewOptions{priorRound: 2})}
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	earlier := putCycleReview(t, f.st, f.run.ID, 1, reentryBase1, reentryHead2)
	putCycleReview(t, f.st, f.run.ID, f.authority.ReviewRound, reentryBase1, reentryHead1)
	later := putCycleReview(t, f.st, f.run.ID, f.authority.ReviewRound+1, reentryBase1, reentryHead2)

	d := f.fixed(f.finding, earlier.InvocationID)
	d.Round = f.authority.ReviewRound
	if err := f.put(d); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
	}
	if err := f.insertRaw(t, d); err == nil {
		t.Fatal("direct SQL inserted a disposition fixed by an earlier round")
	}
	if _, err := f.st.db.ExecContext(ctx,
		`DROP TRIGGER external_finding_disposition_requires_admitted_cycle`); err != nil {
		t.Fatal(err)
	}
	if err := f.insertRaw(t, d); err != nil {
		t.Fatalf("direct insert without the trigger: %v", err)
	}
	if _, err := f.get(d); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("get of the stored row = %v, want ErrParentKeyMismatch", err)
	}
	if _, err := f.st.db.ExecContext(ctx, `DELETE FROM external_finding_dispositions`); err != nil {
		t.Fatal(err)
	}
	// The same cycle does accept the round after it.
	d.RemediationInvocationID = &later.InvocationID
	if err := f.put(d); err != nil {
		t.Fatalf("put fixed by the later round: %v", err)
	}
}

// TestExternalFindingDispositionRefusesRoundReviewedElsewhere: the cycle's
// first round must have reviewed the base and head its authority names. On
// another head, a later round on the finding's own head would otherwise pass
// as its remediation; on another base, the remediation would be compared
// against a base the cycle never reviewed.
func TestExternalFindingDispositionRefusesRoundReviewedElsewhere(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		base, head  string
		remediation func(externalCycleFixture) domain.InvocationID
	}{
		"another head": {reentryBase1, reentryHead2, func(f externalCycleFixture) domain.InvocationID {
			return f.sameHead.InvocationID
		}},
		"another base": {reentryBase2, reentryHead1, func(f externalCycleFixture) domain.InvocationID {
			return f.otherBase.InvocationID
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalCycleReviewedOn(t, tc.base, tc.head)
			d := f.fixed(f.finding, tc.remediation(f))
			if err := f.put(d); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
			if err := f.insertRaw(t, d); err == nil {
				t.Fatal("direct SQL inserted a disposition for a round reviewed off the authority's coordinates")
			}
		})
	}
}

// TestExternalFindingDispositionTriggerAdmitsCycleRound: the schema admits
// the row the Go door admits, by direct SQL too, which is what proves the
// trigger reads the authority's BLOB payload as JSON text.
func TestExternalFindingDispositionTriggerAdmitsCycleRound(t *testing.T) {
	t.Parallel()
	f := seedExternalCycle(t)
	want := f.fixed(f.finding, f.fixedBy.InvocationID)
	if err := f.insertRaw(t, want); err != nil {
		t.Fatalf("direct insert of an admitted cycle-round disposition: %v", err)
	}
	if got, err := f.get(want); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("get = %#v, %v; want %#v", got, err, want)
	}
}

// TestExternalFindingDispositionTriggerRefusesAlteredAuthority: the schema
// refuses the row the Go door admits once the round's authority stops being
// a dispatched external_review authority on the base and head the round
// reviewed, though finding and review record still agree.
func TestExternalFindingDispositionTriggerRefusesAlteredAuthority(t *testing.T) {
	t.Parallel()
	update := func(query string, value any) func(*testing.T, externalCycleFixture) {
		return func(t *testing.T, f externalCycleFixture) {
			if _, err := f.st.db.ExecContext(context.Background(), query, value, f.authority.Key()); err != nil {
				t.Fatal(err)
			}
		}
	}
	rewrite := func(from, to string) func(*testing.T, externalCycleFixture) {
		return func(t *testing.T, f externalCycleFixture) { f.rewriteAuthority(t, from, to) }
	}
	for name, alter := range map[string]func(*testing.T, externalCycleFixture){
		"another head": rewrite(`"head_sha":"`+reentryHead1+`"`, `"head_sha":"`+reentryHead2+`"`),
		"another base": rewrite(`"base_sha":"`+reentryBase1+`"`, `"base_sha":"`+reentryBase2+`"`),
		"another origin": rewrite(`"origin":"`+string(domain.PublicationSuccessorExternalReview)+`"`,
			`"origin":"`+string(domain.PublicationSuccessorRemediation)+`"`),
		"another run":    rewrite(`"run_id":"`, `"run_id":"other-`),
		"not dispatched": update(`UPDATE outbox SET status = ? WHERE idempotency_key = ?`, "pending"),
		"another kind":   update(`UPDATE outbox SET kind = ? WHERE idempotency_key = ?`, "publication_feedback"),
		// A payload that is not JSON is skipped, not raised on: the refusal
		// is still the trigger's own.
		"payload not JSON": update(`UPDATE outbox SET payload = ? WHERE idempotency_key = ?`, []byte("not json")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalCycle(t)
			alter(t, f)
			err := f.insertRaw(t, f.fixed(f.finding, f.fixedBy.InvocationID))
			if err == nil || !strings.Contains(err.Error(), "does not belong to an external review cycle round") {
				t.Fatalf("direct insert under an altered authority = %v, want the trigger's refusal", err)
			}
		})
	}
}

// TestExternalFindingDispositionRefusesOffChainAuthority: the authority's own
// gate admits any superseded ready item, so a row forged beside the real
// successor of one passes it. Only the run's authenticated chain tells them
// apart, and no disposition is written while a forged row is on it.
func TestExternalFindingDispositionRefusesOffChainAuthority(t *testing.T) {
	t.Parallel()
	for name, forge := range offChainAuthorities {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalCycle(t)
			forge(t, f)
			if err := f.put(f.fixed(f.finding, f.fixedBy.InvocationID)); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestExternalFindingDispositionReadRegatesStoredState: an accepted row
// stops reading when the state it rests on is altered under it.
func TestExternalFindingDispositionReadRegatesStoredState(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		alter func(*testing.T, externalCycleFixture)
		want  error
	}{
		"authority moved to another round": {
			alter: func(t *testing.T, f externalCycleFixture) {
				f.rewriteAuthority(t, `"review_round":2`, `"review_round":3`)
			},
			want: domain.ErrParentKeyMismatch,
		},
		"a forged successor beside the authority": {
			alter: offChainAuthorities["a forged successor beside the authority"],
			want:  domain.ErrParentKeyMismatch,
		},
		"a forged successor after the authority": {
			alter: offChainAuthorities["a forged successor after the authority"],
			want:  domain.ErrParentKeyMismatch,
		},
		"authority names a profile that does not admit the reviewer": {
			alter: func(t *testing.T, f externalCycleFixture) {
				f.rewriteAuthority(t, string(f.admitting.ProfileDigest), string(f.unlisted.ProfileDigest))
			},
			want: domain.ErrExternalReviewNotAdmitted,
		},
		"authority no longer dispatched": {
			alter: func(t *testing.T, f externalCycleFixture) {
				if _, err := f.st.db.ExecContext(context.Background(),
					`UPDATE outbox SET status = 'pending' WHERE idempotency_key = ?`, f.authority.Key()); err != nil {
					t.Fatal(err)
				}
			},
			want: domain.ErrParentKeyMismatch,
		},
		"finding no longer external": {
			alter: func(t *testing.T, f externalCycleFixture) {
				if _, err := f.st.db.ExecContext(context.Background(),
					`UPDATE findings SET body = json_remove(body, '$.external') WHERE id = ?`, f.second.ID); err != nil {
					t.Fatal(err)
				}
			},
			want: domain.ErrExternalFindingInconsistent,
		},
		// An external finding's ID is the address of its provenance, so the
		// finding's own read refuses a moved head before the cycle check can.
		"finding moved to another head": {
			alter: func(t *testing.T, f externalCycleFixture) {
				if _, err := f.st.db.ExecContext(context.Background(),
					`UPDATE findings SET body = json_set(body, '$.external.head_sha', ?) WHERE id = ?`,
					reentryHead2, f.second.ID); err != nil {
					t.Fatal(err)
				}
			},
			want: domain.ErrExternalFindingInconsistent,
		},
		"copied disposition column": {
			alter: func(t *testing.T, f externalCycleFixture) {
				if _, err := f.st.db.ExecContext(context.Background(),
					`UPDATE external_finding_dispositions SET reason = 'rewritten'`); err != nil {
					t.Fatal(err)
				}
			},
			want: errRowInconsistent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalCycle(t)
			d := f.fixed(f.second, f.fixedBy.InvocationID)
			if err := f.put(d); err != nil {
				t.Fatalf("put: %v", err)
			}
			tc.alter(t, f)
			if _, err := f.get(d); !errors.Is(err, tc.want) {
				t.Fatalf("get = %v, want %v", err, tc.want)
			}
			if _, err := f.list(); !errors.Is(err, tc.want) {
				t.Fatalf("list = %v, want %v", err, tc.want)
			}
			// A replayed write re-reads the stored row, so it cannot hide
			// the damage behind an unchanged body.
			if err := f.put(d); err == nil {
				t.Fatal("replay converged over altered state")
			}
		})
	}
}
