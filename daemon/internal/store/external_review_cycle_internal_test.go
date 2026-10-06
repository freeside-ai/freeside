package store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// externalCycleFixture is a sealed external_review cycle with the
// review rounds a disposition can name. Round 2 is the cycle's first round,
// on the authority's base and head; its review record raised one finding of
// its own. Round 3 is a remediation round: the same base, another head.
// Rounds 4 and 5 are what a remediation may not be: another base, and the
// cycle's own head.
type externalCycleFixture struct {
	externalReviewFixture
	// internal is the finding the cycle's first-round review record lists.
	internal domain.Finding
	// second is another admitted external finding on the cycle's head.
	second domain.Finding
	// laterInternal is the finding round 3's review record lists.
	laterInternal domain.Finding
	fixedBy       domain.ReviewRecord
	otherBase     domain.ReviewRecord
	sameHead      domain.ReviewRecord
}

const externalCycleRound = 2

func putCycleReview(
	t *testing.T, st *Store, runID domain.RunID, round int, base, head string, findings ...domain.Finding,
) domain.ReviewRecord {
	t.Helper()
	outcome := domain.ReviewClean
	ids := make([]domain.FindingID, 0, len(findings))
	for _, finding := range findings {
		outcome = domain.ReviewFindings
		ids = append(ids, finding.ID)
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-cycle-" + strconv.Itoa(round)), RunID: runID, Round: round,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: base, HeadSHA: head,
		CompletedAt:        reentryAt.Add(time.Duration(round) * time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            outcome, FindingIDs: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *WriteTx) error {
		return tx.PutReviewRecord(context.Background(), record, findings)
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func cycleInternalFinding(id domain.FindingID, runID domain.RunID) domain.Finding {
	return domain.Finding{
		ID: id, RunID: runID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 1, EndLine: 1},
		Message:  "finding " + string(id), RawText: "finding " + string(id), CreatedAt: reentryAt,
	}
}

func seedExternalCycle(t *testing.T) externalCycleFixture {
	t.Helper()
	return seedExternalCycleReviewedOn(t, reentryBase1, reentryHead1)
}

// seedExternalCycleReviewedOn puts the cycle's first-round review
// record on the given base and head. Nothing in the store ties that record
// to the authority's coordinates when it is written, so a fixture can place
// it elsewhere.
func seedExternalCycleReviewedOn(t *testing.T, base, head string) externalCycleFixture {
	t.Helper()
	f := externalCycleFixture{externalReviewFixture: seedExternalReview(t, externalReviewOptions{})}
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	runID := f.run.ID
	f.second = externalFindingBy(t, runID, reentryHead1, externalReviewerID, externalReviewerLogin,
		"PRRT_second", reentryAt.Add(time.Second))
	putExternalFinding(t, f.st, f.second)
	f.internal = cycleInternalFinding("finding-cycle-internal", runID)
	f.laterInternal = cycleInternalFinding("finding-later-internal", runID)
	putCycleReview(t, f.st, runID, externalCycleRound, base, head, f.internal)
	f.fixedBy = putCycleReview(t, f.st, runID, 3, reentryBase1, reentryHead2, f.laterInternal)
	f.otherBase = putCycleReview(t, f.st, runID, 4, reentryBase2, reentryHead2)
	f.sameHead = putCycleReview(t, f.st, runID, 5, reentryBase1, reentryHead1)
	return f
}

// adjudication builds a round's artifact from the route each finding takes:
// the engine's remediation row, or a model row that authorizes the final
// declined or deferred disposition.
func (f externalCycleFixture) adjudication(
	t *testing.T, round int, routes map[domain.FindingID]domain.AdjudicationRoute,
) domain.FindingAdjudication {
	t.Helper()
	entries := make([]domain.FindingAdjudicationEntry, 0, len(routes))
	for id, route := range routes {
		var (
			entry domain.FindingAdjudicationEntry
			err   error
		)
		switch route {
		case domain.RouteDecline:
			entry, err = domain.NewModelAdjudicationEntry(id, domain.GoalContradictory, nil, route,
				domain.ConfidenceHigh, "authorizes the final disposition", nil, nil, nil, nil, nil)
		case domain.RouteDefer:
			entry, err = domain.NewModelAdjudicationEntry(id, domain.GoalAdjacent, nil, route,
				domain.ConfidenceHigh, "authorizes the final disposition", nil, nil, nil, nil, nil)
		default:
			allowed := domain.CompatibilityAllowed
			entry, err = domain.NewEngineAdjudicationEntry(id, domain.GoalRequired, &allowed, route,
				"in declared scope", nil, nil, nil, nil, nil)
		}
		if err != nil {
			t.Fatalf("adjudication entry %q: %v", id, err)
		}
		entries = append(entries, entry)
	}
	artifact, err := domain.NewFindingAdjudication(f.run.ID, round,
		f.run.SpecDigest, domain.Digest("sha256:"+strings.Repeat("d", 64)), f.run.PolicyDigest,
		entries, "", reentryAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("new adjudication: %v", err)
	}
	return artifact
}

func (f externalCycleFixture) putAdjudication(t *testing.T, artifact domain.FindingAdjudication) error {
	t.Helper()
	ctx := context.Background()
	return f.st.Write(ctx, func(tx *WriteTx) error { return tx.PutFindingAdjudication(ctx, artifact) })
}

// rewriteAuthority edits the sealed authority's payload in place and restamps
// the outbox row's payload digest, so the row still authenticates as stored
// and only a gate can refuse what it now says.
func (f externalCycleFixture) rewriteAuthority(t *testing.T, from, to string) {
	t.Helper()
	ctx := context.Background()
	var payload []byte
	if err := f.st.db.QueryRowContext(ctx, `SELECT payload FROM outbox WHERE idempotency_key = ?`,
		f.authority.Key()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), from) {
		t.Fatalf("authority payload does not contain %q", from)
	}
	rewritten := []byte(strings.ReplaceAll(string(payload), from, to))
	if _, err := f.st.db.ExecContext(ctx, `UPDATE outbox SET payload = ?, payload_digest = ? WHERE idempotency_key = ?`,
		rewritten, contentaddr.Sum(rewritten), f.authority.Key()); err != nil {
		t.Fatal(err)
	}
}

// forgeSuccessor stores a dispatched successor of the given ready item as a
// row written past the seal door would be. The row is well formed, but no
// feedback return stands behind it, so its own gate refuses it.
func (f externalCycleFixture) forgeSuccessor(t *testing.T, predecessor domain.ItemID) {
	t.Helper()
	forged := domain.PublicationSuccessor{
		Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorFeedback,
		RunID: f.authority.RunID, CommandID: "return-forged", FeedbackInvocationID: "inv-feedback-forged",
		PredecessorItemID: predecessor, PriorReviewInvocationID: f.authority.PriorReviewInvocationID,
		ReviewRound: f.authority.ReviewRound,
	}
	if err := forged.Validate(); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.db.ExecContext(context.Background(), `INSERT INTO outbox
		(idempotency_key, kind, payload, payload_version, payload_digest, status, created_at)
		SELECT ?, kind, ?, payload_version, ?, status, created_at FROM outbox WHERE idempotency_key = ?`,
		forged.Key(), payload, contentaddr.Sum(payload), f.authority.Key()); err != nil {
		t.Fatal(err)
	}
}

// offChainAuthorities are the two ways a forged row takes the authority off
// the run's authenticated chain: beside it, as a second successor of the
// ready item it supersedes, and after it, where the row links cleanly and
// only its own gate gives it away.
var offChainAuthorities = map[string]func(*testing.T, externalCycleFixture){
	"a forged successor beside the authority": func(t *testing.T, f externalCycleFixture) {
		f.forgeSuccessor(t, f.authority.PredecessorItemID)
	},
	"a forged successor after the authority": func(t *testing.T, f externalCycleFixture) {
		f.forgeSuccessor(t, f.authority.ReadyItemID())
	},
}

// TestExternalReviewCycleForRoundUnderReconstruction: a read that is already
// reconstructing a successor cannot gate the chain again, so there the lookup
// links the sealed rows and gates the authority alone. A branch is still
// refused. A row after the authority is left to the read that started the
// reconstruction; a lookup that starts one itself refuses it.
func TestExternalReviewCycleForRoundUnderReconstruction(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		forge       func(*testing.T, externalCycleFixture)
		top, nested error
	}{
		"the sealed chain": {},
		"a forged successor beside the authority": {
			forge: offChainAuthorities["a forged successor beside the authority"],
			top:   domain.ErrParentKeyMismatch, nested: domain.ErrParentKeyMismatch,
		},
		"a forged successor after the authority": {
			forge: offChainAuthorities["a forged successor after the authority"],
			top:   domain.ErrParentKeyMismatch,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalCycle(t)
			if tc.forge != nil {
				tc.forge(t, f)
			}
			top := context.Background()
			nested, err := publicationReadContext(top, "publication-successor/enclosing-read")
			if err != nil {
				t.Fatal(err)
			}
			for _, lookup := range []struct {
				when string
				ctx  context.Context
				want error
			}{{"top-level", top, tc.top}, {"under reconstruction", nested, tc.nested}} {
				err := f.st.Read(top, func(tx *ReadTx) error {
					record, err := tx.reviewRecordForRound(top, f.run.ID, externalCycleRound)
					if err != nil {
						t.Fatal(err)
					}
					authority, found, err := tx.externalReviewCycleForRound(lookup.ctx, record)
					if err == nil && (!found || !reflect.DeepEqual(authority, f.authority)) {
						t.Fatalf("%s: cycle = %#v, %t; want %#v", lookup.when, authority, found, f.authority)
					}
					return err
				})
				if !errors.Is(err, lookup.want) {
					t.Fatalf("%s: lookup = %v, want %v", lookup.when, err, lookup.want)
				}
			}
		})
	}
}

// TestExternalReviewAuthorityForRound: only an external_review authority
// starting at the round is the round's cycle, and two of them are refused
// instead of one being preferred. The stored cases above cannot hold two:
// each would need its own superseded ready item.
func TestExternalReviewAuthorityForRound(t *testing.T) {
	t.Parallel()
	external := func(predecessor domain.ItemID, round int) domain.PublicationSuccessor {
		return domain.PublicationSuccessor{
			Origin: domain.PublicationSuccessorExternalReview, PredecessorItemID: predecessor, ReviewRound: round,
		}
	}
	first, second := external("item-1", 2), external("item-2", 2)
	remediation := domain.PublicationSuccessor{
		Origin: domain.PublicationSuccessorRemediation, PredecessorItemID: "item-3", ReviewRound: 2,
	}
	later := external("item-4", 3)

	got, found, err := externalReviewAuthorityForRound([]domain.PublicationSuccessor{remediation, later, first}, 2)
	if err != nil || !found || !reflect.DeepEqual(got, first) {
		t.Fatalf("one cycle for the round = %#v, %t, %v; want %#v", got, found, err, first)
	}
	if _, found, err := externalReviewAuthorityForRound([]domain.PublicationSuccessor{remediation, later}, 2); err != nil || found {
		t.Fatalf("no cycle for the round = %t, %v; want not found", found, err)
	}
	if _, _, err := externalReviewAuthorityForRound(
		[]domain.PublicationSuccessor{first, second}, 2); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("two cycles for the round = %v, want ErrParentKeyMismatch", err)
	}
}

func (f externalCycleFixture) getAdjudication(digest domain.Digest) (domain.FindingAdjudication, error) {
	ctx := context.Background()
	var got domain.FindingAdjudication
	err := f.st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetFindingAdjudication(ctx, digest)
		return err
	})
	return got, err
}

// TestFindingAdjudicationAdmitsExternalCycleEntries: the artifact for an
// external cycle's first round may carry an entry per admitted external
// finding of that cycle beside every finding of the round's review record,
// and it reads back the same after the owner edits the allowlist. It need
// not carry every admitted finding: the store rule is one-way.
func TestFindingAdjudicationAdmitsExternalCycleEntries(t *testing.T) {
	t.Parallel()
	for name, routes := range map[string]func(externalCycleFixture) map[domain.FindingID]domain.AdjudicationRoute{
		"every admitted finding": func(f externalCycleFixture) map[domain.FindingID]domain.AdjudicationRoute {
			return map[domain.FindingID]domain.AdjudicationRoute{
				f.internal.ID: domain.RouteRemediate, f.finding.ID: domain.RouteDecline, f.second.ID: domain.RouteDefer,
			}
		},
		"one of the admitted findings": func(f externalCycleFixture) map[domain.FindingID]domain.AdjudicationRoute {
			return map[domain.FindingID]domain.AdjudicationRoute{
				f.internal.ID: domain.RouteRemediate, f.second.ID: domain.RouteRemediate,
			}
		},
		"the review record alone": func(f externalCycleFixture) map[domain.FindingID]domain.AdjudicationRoute {
			return map[domain.FindingID]domain.AdjudicationRoute{f.internal.ID: domain.RouteRemediate}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := seedExternalCycle(t)
			want := f.adjudication(t, externalCycleRound, routes(f))
			if err := f.putAdjudication(t, want); err != nil {
				t.Fatalf("put: %v", err)
			}
			if err := f.putAdjudication(t, want); err != nil {
				t.Fatalf("replay: %v", err)
			}
			check := func(when string) {
				t.Helper()
				got, err := f.getAdjudication(want.Digest)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: get = %#v, %v; want %#v", when, got, err, want)
				}
				if err := f.st.Read(ctx, func(tx *ReadTx) error {
					_, err := tx.GetFindingAdjudicationForRound(ctx, f.run.ID, externalCycleRound)
					return err
				}); err != nil {
					t.Fatalf("%s: get for round: %v", when, err)
				}
			}
			check("after write")
			activateProfile(t, f.st, f.binding.Repo, f.binding.RepositoryID, reentryAt.Add(time.Hour),
				reviewerEntry(900, "maintainer"))
			check("after the allowlist drops the reviewer")
		})
	}
}

// TestFindingAdjudicationRefusesForeignEntries: an entry outside the round's
// review record is admitted nowhere but an external cycle's first round and
// for nothing but a finding that cycle admits, and the artifact still has to
// cover the review record. Each refusal holds at the write door and, for a
// row placed with direct SQL, on the read.
func TestFindingAdjudicationRefusesForeignEntries(t *testing.T) {
	t.Parallel()
	type build func(*testing.T, externalCycleFixture) domain.FindingAdjudication
	external := func(head string, accountID int64, login string) build {
		return func(t *testing.T, f externalCycleFixture) domain.FindingAdjudication {
			finding := externalFindingBy(t, f.run.ID, head, accountID, login, "PRRT_foreign", reentryAt)
			putExternalFinding(t, f.st, finding)
			return f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
				f.internal.ID: domain.RouteRemediate, finding.ID: domain.RouteDecline,
			})
		}
	}
	for name, build := range map[string]build{
		"external entry in the round before the cycle": func(t *testing.T, f externalCycleFixture) domain.FindingAdjudication {
			return f.adjudication(t, 1, map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteDecline})
		},
		"external entry in a later round": func(t *testing.T, f externalCycleFixture) domain.FindingAdjudication {
			return f.adjudication(t, 3, map[domain.FindingID]domain.AdjudicationRoute{
				f.laterInternal.ID: domain.RouteRemediate, f.finding.ID: domain.RouteDecline,
			})
		},
		"external entry in a round on the cycle's head with no cycle": func(t *testing.T, f externalCycleFixture) domain.FindingAdjudication {
			return f.adjudication(t, 5, map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteDecline})
		},
		"entry for an unlisted reviewer's finding":    external(reentryHead1, 900, "maintainer"),
		"entry for a listed reviewer on another head": external(reentryHead2, externalReviewerID, externalReviewerLogin),
		"entry for another round's review finding": func(t *testing.T, f externalCycleFixture) domain.FindingAdjudication {
			return f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
				f.internal.ID: domain.RouteRemediate, f.laterInternal.ID: domain.RouteRemediate,
			})
		},
		"review record finding omitted": func(t *testing.T, f externalCycleFixture) domain.FindingAdjudication {
			return f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
				f.finding.ID: domain.RouteDecline,
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := seedExternalCycle(t)
			artifact := build(t, f)
			if err := f.putAdjudication(t, artifact); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
			body, err := encode(artifact)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.db.ExecContext(ctx, putFindingAdjudicationSQL,
				artifact.RunID, artifact.Round, artifact.Revision, nil,
				artifact.Digest, artifact.FindingBatchDigest,
				artifact.ApprovedSpecDigest, artifact.InstructionSnapshotDigest,
				artifact.ResolvedPolicyDigest, formatTime(artifact.CreatedAt),
				reviewBodyDigest(body), body); err != nil {
				t.Fatalf("direct insert: %v", err)
			}
			if _, err := f.getAdjudication(artifact.Digest); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("get of the stored artifact = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestFindingAdjudicationRefusesExternalEntriesForRoundReviewedElsewhere: a
// cycle's first round must have reviewed the base and head its authority
// names, or nothing the authority admits was in front of that review.
func TestFindingAdjudicationRefusesExternalEntriesForRoundReviewedElsewhere(t *testing.T) {
	t.Parallel()
	for name, on := range map[string]struct{ base, head string }{
		"another head": {reentryBase1, reentryHead2},
		"another base": {reentryBase2, reentryHead1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalCycleReviewedOn(t, on.base, on.head)
			artifact := f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
				f.internal.ID: domain.RouteRemediate, f.finding.ID: domain.RouteDecline,
			})
			if err := f.putAdjudication(t, artifact); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put adjudication = %v, want ErrParentKeyMismatch", err)
			}
			// The record's own finding alone needs no cycle, wherever the
			// round was reviewed.
			ordinary := f.adjudication(t, externalCycleRound, map[domain.FindingID]domain.AdjudicationRoute{
				f.internal.ID: domain.RouteRemediate,
			})
			if err := f.putAdjudication(t, ordinary); err != nil {
				t.Fatalf("put adjudication of the record's findings alone: %v", err)
			}
		})
	}
}

// TestFindingAdjudicationExternalEntriesRegateAuthority: an accepted
// artifact with an external entry stops reading once the authority that
// admitted the entry no longer starts its cycle at the artifact's round, or
// is no longer on the run's authenticated chain. An artifact over the review
// record alone never depended on it.
func TestFindingAdjudicationExternalEntriesRegateAuthority(t *testing.T) {
	t.Parallel()
	alterations := map[string]func(*testing.T, externalCycleFixture){
		"authority moved to another round": func(t *testing.T, f externalCycleFixture) {
			f.rewriteAuthority(t, `"review_round":2`, `"review_round":3`)
		},
	}
	maps.Copy(alterations, offChainAuthorities)
	for name, tc := range map[string]struct {
		external bool
		want     error
	}{
		"with an external entry":  {external: true, want: domain.ErrParentKeyMismatch},
		"the review record alone": {},
	} {
		for altered, alter := range alterations {
			t.Run(name+"/"+altered, func(t *testing.T) {
				t.Parallel()
				f := seedExternalCycle(t)
				routes := map[domain.FindingID]domain.AdjudicationRoute{f.internal.ID: domain.RouteRemediate}
				if tc.external {
					routes[f.finding.ID] = domain.RouteDecline
				}
				artifact := f.adjudication(t, externalCycleRound, routes)
				if err := f.putAdjudication(t, artifact); err != nil {
					t.Fatalf("put: %v", err)
				}
				alter(t, f)
				if _, err := f.getAdjudication(artifact.Digest); !errors.Is(err, tc.want) {
					t.Fatalf("get = %v, want %v", err, tc.want)
				}
			})
		}
	}
}
