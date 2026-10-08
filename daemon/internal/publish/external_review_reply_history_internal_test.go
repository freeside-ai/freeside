package publish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (h *replyHarness) activateSecond() {
	h.t.Helper()
	h.write(func(tx *store.WriteTx) error { return tx.PutExternalFindingDisposition(context.Background(), h.second) })
	h.d = h.second
}

func (h *replyHarness) acknowledgeAmbiguity() {
	h.t.Helper()
	ctx := context.Background()
	service := signet.NewService(h.f.st,
		signet.WithClock(func() time.Time { return h.now }),
		signet.WithPairingKey([]byte("reply-test-pairing-key")),
		signet.WithHostFacts(signet.HostFacts{DisplayName: "reply test", ConnectionMode: domain.ConnectionLoopback}),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL: "https://ntfy.example.test", TopicKey: []byte(strings.Repeat("n", 32)), ClickBaseURL: "https://daemon.example.test",
		}),
	)
	code, _, err := service.MintPairingCode(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	grant, err := service.Pair(ctx, code, "reply test")
	if err != nil {
		h.t.Fatal(err)
	}
	var item domain.AttentionItem
	var snapshot store.Snapshot
	h.read(func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].HealthDiagnostic.Code != externalReplyAmbiguous {
			h.t.Fatalf("prior ambiguous notice: %+v", items)
		}
		item, snapshot, err = tx.GetAttentionItemSnapshot(ctx, items[0].ID)
		return err
	})
	if _, err := service.Submit(ctx, signet.ClientCommand{
		CommandID: "ack-prior-ambiguity", DeviceID: grant.Device.Device.ID, ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, ItemVersion: item.ItemVersion,
			PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests, Action: domain.ActionAcknowledge,
		},
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *replyHarness) intent() externalReplyIntent {
	h.t.Helper()
	var i externalReplyIntent
	h.read(func(tx *store.ReadTx) error {
		row, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round))
		if err != nil {
			return err
		}
		i, err = decodeExternalReplyIntent(row)
		return err
	})
	return i
}

func historyReplyIntent(binding domain.ReadyItemPRBinding, thread string, at time.Time) externalReplyIntent {
	return externalReplyIntent{
		ItemID: binding.ItemID, FindingID: "historical-finding", RunID: binding.RunID, Round: 1,
		Repo: binding.Repo, PRNumber: binding.PRNumber, Thread: thread, HeadSHA: binding.HeadSHA,
		Disposition: domain.ReviewDispositionDeclined, Ruleset: domain.IssueTextRulesetGitHubIssue1,
		TextDigest: "sha256:historic", BotID: 123, CreatedAt: at,
	}
}

func (h *replyHarness) recordReplyHistory(i externalReplyIntent, code string) {
	h.t.Helper()
	intent, err := json.Marshal(i)
	if err != nil {
		h.t.Fatal(err)
	}
	o := externalReplyOutcome{FindingID: i.FindingID, Round: i.Round, Code: code}
	if code == "" {
		o.CommentID = 100
	}
	outcome, err := json.Marshal(o)
	if err != nil {
		h.t.Fatal(err)
	}
	h.write(func(tx *store.WriteTx) error {
		ctx := context.Background()
		if _, _, err := tx.RecordDispatchedOutbox(ctx, i.key(), ExternalReplyIntentKind, intent); err != nil {
			return err
		}
		_, _, err := tx.RecordDispatchedOutbox(ctx, i.key()+"/outcome", ExternalReplyOutcomeKind, outcome)
		return err
	})
}

func TestExternalReviewReplierHistoryCollectionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name           string
		current, prior string
		repo           string
		repositoryID   int64
		prNumber       int
		code           string
		blocked        bool
	}{
		{"another run and review", "review/42", "review/999", "owner/repo", 424242, 450, externalReplyAmbiguous, true},
		{"renamed repository", "review/42", "review/999", "old-owner/old-repo", 424242, 450, externalReplyAmbiguous, true},
		{"same inline root", "review_comment/43", "review_comment/43", "owner/repo", 424242, 450, externalReplyAmbiguous, true},
		{"other repository", "review/42", "review/999", "owner/other", 999, 450, externalReplyAmbiguous, false},
		{"other pull request", "review/42", "review/999", "owner/repo", 424242, 451, externalReplyAmbiguous, false},
		{"other inline root", "review_comment/43", "review_comment/44", "owner/repo", 424242, 450, externalReplyAmbiguous, false},
		{"conversation after inline", "review/42", "review_comment/43", "owner/repo", 424242, 450, externalReplyAmbiguous, false},
		{"inline after conversation", "review_comment/43", "review/42", "owner/repo", 424242, 450, externalReplyAmbiguous, false},
		{"prior success", "review/42", "review/999", "owner/repo", 424242, 450, "", false},
		{"prior refusal", "review/42", "review/999", "owner/repo", 424242, 450, externalReplyRefused, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReplyHarness(t, tc.current, domain.ReviewDispositionDeclined, "Already checked.")
			prior := seedReplyBindingIn(t, h.f.st, replyBindingSeed{"earlier-run", tc.repo, tc.repositoryID, tc.prNumber}, "feat/earlier-reply", h.f.binding.HeadSHA, h.f.binding.HeadSHA)
			i := historyReplyIntent(prior.binding, tc.prior, h.now.Add(-time.Hour))
			// The restriction persists even if the App account has changed.
			i.BotID = 321
			h.recordReplyHistory(i, tc.code)
			h.restart()
			h.status = http.StatusInternalServerError
			h.pass()
			h.pass()
			h.comments = append(h.comments, h.comment(777, 123))
			h.restart()
			h.pass()
			h.now = h.now.Add(11 * time.Minute)
			h.pass()
			h.pass()
			got := h.outcome()
			if tc.blocked {
				if got.Code != externalReplyAmbiguous || got.CommentID != 0 {
					t.Fatalf("restricted collection adopted: %+v", got)
				}
			} else if got.Code != "" || got.CommentID != 777 {
				t.Fatalf("independent collection did not recover: %+v", got)
			}
			if h.creates != 1 {
				t.Fatalf("unknown send retried: %d", h.creates)
			}
		})
	}
}

func TestExternalReviewReplierPriorAmbiguityAllowsProvenCreate(t *testing.T) {
	h := newReplyHarnessThreads(t, "review/42", "review/43", domain.ReviewDispositionDeclined, "Already checked.")
	h.status = http.StatusInternalServerError
	h.pass()
	h.pass()
	h.now = h.now.Add(11 * time.Minute)
	h.pass()
	h.restart()
	h.activateSecond()
	h.status = http.StatusCreated
	h.pass()
	h.pass()
	h.restart()
	h.pass()
	if got := h.outcome(); got.Code != "" || got.CommentID != 102 || h.creates != 2 {
		t.Fatalf("proven later create: %+v, creates=%d", got, h.creates)
	}
}

func TestExternalReviewReplierSerializesRenamedRepository(t *testing.T) {
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	prior := seedReplyBindingIn(t, h.f.st, replyBindingSeed{"earlier-run", "former-owner/former-repo", 424242, 450}, "feat/earlier-reply", h.f.binding.HeadSHA, h.f.binding.HeadSHA)
	i := historyReplyIntent(prior.binding, "review/99", h.now)
	payload, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	h.write(func(tx *store.WriteTx) error {
		_, _, err := tx.EnqueueOutbox(context.Background(), i.key(), ExternalReplyIntentKind, payload)
		if err != nil {
			return err
		}
		return tx.MarkOutboxDispatching(context.Background(), i.key())
	})
	h.restart()
	h.pass()
	h.pass()
	h.read(func(tx *store.ReadTx) error {
		rows, err := tx.ListPendingOutbox(context.Background(), ExternalReplyIntentKind)
		if len(rows) != 1 || rows[0].IdempotencyKey != i.key() || h.creates != 0 {
			t.Fatalf("rename bypassed serialization: %+v, creates=%d", rows, h.creates)
		}
		return err
	})
}

func (h *replyHarness) damageReplyHistory(query string, args ...any) {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.f.path)
	if err != nil {
		h.t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), query, args...)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		h.t.Fatalf("damage history fixture: %v, %v", err, closeErr)
	}
}

func TestExternalReviewReplierDamagedHistoryNeverAdopts(t *testing.T) {
	for _, tc := range []string{"missing intent", "missing binding", "invalid binding", "run", "repository", "pull request", "head", "thread", "finding key", "round key", "intent kind", "malformed outcome", "outcome key", "unreadable row"} {
		t.Run(tc, func(t *testing.T) {
			h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
			prior := seedReplyBindingIn(t, h.f.st, replyBindingSeed{"earlier-run", "owner/repo", 424242, 450}, "feat/earlier-reply", h.f.binding.HeadSHA, h.f.binding.HeadSHA)
			i := historyReplyIntent(prior.binding, "review/99", h.now)
			key := i.key()
			switch tc {
			case "missing binding":
				i.ItemID = "missing-item"
			case "run":
				i.RunID = "wrong-run"
			case "repository":
				i.Repo = "owner/other"
			case "pull request":
				i.PRNumber++
			case "head":
				i.HeadSHA = strings.Repeat("b", 40)
			case "thread":
				i.Thread = "invalid/99"
			case "finding key":
				i.FindingID = "wrong-finding"
			case "round key":
				i.Round++
			}
			payload, err := json.Marshal(i)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := json.Marshal(externalReplyOutcome{"historical-finding", 1, 0, externalReplyAmbiguous})
			if err != nil {
				t.Fatal(err)
			}
			if tc == "malformed outcome" {
				outcome = []byte(`{"finding_id":`)
			}
			h.write(func(tx *store.WriteTx) error {
				ctx := context.Background()
				if tc != "missing intent" {
					kind := ExternalReplyIntentKind
					if tc == "intent kind" {
						kind = "other-kind"
					}
					if _, _, err := tx.RecordDispatchedOutbox(ctx, key, kind, payload); err != nil {
						return err
					}
				}
				outcomeKey := key + "/outcome"
				if tc == "outcome key" {
					outcomeKey = "wrong-key/outcome"
				}
				_, _, err := tx.RecordDispatchedOutbox(ctx, outcomeKey, ExternalReplyOutcomeKind, outcome)
				return err
			})
			h.status = http.StatusInternalServerError
			h.pass()
			h.pass()
			h.comments = append(h.comments, h.comment(777, 123))
			if tc == "invalid binding" {
				h.damageReplyHistory("UPDATE ready_item_pr_bindings SET body = '{}' WHERE item_id = ?", prior.binding.ItemID)
			}
			if tc == "unreadable row" {
				h.damageReplyHistory("UPDATE outbox SET created_at = 'invalid' WHERE idempotency_key = ?", key+"/outcome")
			}
			h.now = h.now.Add(11 * time.Minute)
			if err := h.r.Pass(context.Background()); err == nil {
				t.Fatal("damaged history authorized recovery")
			}
			h.read(func(tx *store.ReadTx) error {
				_, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round)+"/outcome")
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("outcome recorded with damaged history: %v", err)
				}
				return nil
			})
			if h.creates != 1 {
				t.Fatalf("damaged history retried send: %d", h.creates)
			}
		})
	}
}

func TestExternalReviewReplierHistoryReadFailure(t *testing.T) {
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	i := h.intent()
	if err := h.f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if ambiguous, err := h.r.priorAmbiguousReply(context.Background(), i); err == nil || ambiguous {
		t.Fatalf("unreadable store reported clean history: %v, %v", ambiguous, err)
	}
}

func TestExternalReviewReplierDoesNotAdoptPriorAmbiguousReply(t *testing.T) {
	for _, threads := range [][2]string{{"review/42", "review/43"}, {"review_comment/43", "review_comment/43"}} {
		for _, restart := range []bool{false, true} {
			name := threads[0] + "/live"
			if restart {
				name = threads[0] + "/restart"
			}
			t.Run(name, func(t *testing.T) {
				h := newReplyHarnessThreads(t, threads[0], threads[1], domain.ReviewDispositionDeclined, "Already checked.")
				h.status = http.StatusInternalServerError
				h.pass()
				h.pass()
				h.now = h.now.Add(11 * time.Minute)
				h.pass()
				if got := h.outcome(); got.Code != externalReplyAmbiguous || got.CommentID != 0 {
					t.Fatalf("first outcome: %+v", got)
				}
				if restart {
					h.acknowledgeAmbiguity()
					h.restart()
				}
				h.activateSecond()
				h.pass()
				h.pass()
				// Only A posts, after B's unknown send, inside B's window.
				h.comments = append(h.comments, h.comment(777, 123))
				if restart {
					h.restart()
					h.pass()
				}
				h.now = h.now.Add(11 * time.Minute)
				h.pass()
				h.pass()
				if got := h.outcome(); got.Code != externalReplyAmbiguous || got.CommentID != 0 || h.creates != 2 {
					t.Fatalf("late A attributed to B: outcome=%+v, creates=%d", got, h.creates)
				}
				h.read(func(tx *store.ReadTx) error {
					items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
					if len(items) != 2 {
						t.Fatalf("ambiguous notices = %d", len(items))
					}
					return err
				})
			})
		}
	}
}
