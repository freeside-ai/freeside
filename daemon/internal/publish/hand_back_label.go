package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// HandBackLabelRemovalKind is the package-owned outbox record of one removed
// hand-back label: the ready item a hand-back cycle earned, and the pull
// request the label came off. It contains no artifact references.
const HandBackLabelRemovalKind = "publish.hand_back_label_removal"

type handBackLabelRemoval struct {
	ItemID   domain.ItemID `json:"item_id"`
	RunID    domain.RunID  `json:"run_id"`
	Repo     string        `json:"repo"`
	PRNumber int           `json:"pr_number"`
}

func handBackLabelRemovalKey(item domain.ItemID) string {
	return "hand-back-label-removal/" + string(item)
}

// HandBackLabelBackupPayloadDigests validates a removal record before backup
// accepts the outbox as complete.
func HandBackLabelBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	var r handBackLabelRemoval
	if entry.Kind != HandBackLabelRemovalKind ||
		strictjson.Decode(entry.Payload, &r, strictjson.RejectInvalidUTF8, strictjson.NoLimit) != nil ||
		r.ItemID == "" || r.RunID == "" || r.PRNumber < 1 || entry.IdempotencyKey != handBackLabelRemovalKey(r.ItemID) {
		return nil, errors.New("hand-back label: invalid removal record")
	}
	if _, err := parseRepo(r.Repo); err != nil {
		return nil, errors.New("hand-back label: invalid repository")
	}
	return nil, nil
}

// HandBackLabelRemover takes the hand-back label off a pull request once a
// hand-back cycle has made it ready again (issue #3160). It derives its work
// from the store on every pass: the run's current ready item, read through
// the store's gates (store.HandBackReadyBinding). Removing a label is
// idempotent, so it records each removal after the forge confirms it and
// sends again after a crash. It removes only the label it was configured with
// and never touches any other, the owner's approval label included.
type HandBackLabelRemover struct {
	store *store.Store
	forge *forge
	label string
	mu    sync.Mutex
}

// NewHandBackLabelRemover wires the remover. tokens must mint only
// HandBackLabelPermissions (HandBackLabelTokenSource).
func NewHandBackLabelRemover(
	s *store.Store, tokens TokenSource, client *http.Client, baseURL, label string,
) (*HandBackLabelRemover, error) {
	if s == nil || tokens == nil || client == nil || strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("hand-back label: nil or empty dependency")
	}
	if err := ValidateLabelName(label); err != nil {
		return nil, fmt.Errorf("hand-back label: %w", err)
	}
	return &HandBackLabelRemover{
		store: s, forge: newForge(tokens, client, strings.TrimRight(baseURL, "/")), label: label,
	}, nil
}

// Run sweeps on the supplied interval until ctx ends. Pass errors are
// reported without stopping later passes.
func (r *HandBackLabelRemover) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 {
		return errors.New("hand-back label: interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Pass removes the label from every pull request a hand-back cycle made
// ready since the last pass. Errors crossing this boundary are fixed text:
// transports and token sources may echo credentials.
func (r *HandBackLabelRemover) Pass(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.pass(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("hand-back label: pass failed; retry on next sweep")
	}
	return nil
}

func (r *HandBackLabelRemover) pass(ctx context.Context) error {
	var runs []store.Snapshotted[domain.Run]
	if err := r.store.Read(ctx, func(tx *store.ReadTx) (err error) { runs, err = tx.ListRuns(ctx); return err }); err != nil {
		return err
	}
	var errs []error
	for _, snapshot := range runs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.remove(ctx, snapshot.Value.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *HandBackLabelRemover) remove(ctx context.Context, runID domain.RunID) error {
	var (
		ready domain.ReadyItemPRBinding
		due   bool
	)
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if ready, due, err = tx.HandBackReadyBinding(ctx, runID); err != nil || !due {
			return err
		}
		_, err = tx.GetOutbox(ctx, handBackLabelRemovalKey(ready.ItemID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		due = false
		return err
	}); err != nil || !due {
		return err
	}
	return r.removeFor(ctx, ready)
}

// removeFor takes the label off ready's pull request and records that it is
// gone. The caller has proved through the store that a hand-back cycle made
// ready current.
func (r *HandBackLabelRemover) removeFor(ctx context.Context, ready domain.ReadyItemPRBinding) error {
	repo, err := parseRepo(ready.Repo)
	if err != nil {
		return err
	}
	if err := r.forge.removeLabel(ctx, repo, ready.PRNumber, r.label); err != nil {
		return err
	}
	payload, err := json.Marshal(handBackLabelRemoval{
		ItemID: ready.ItemID, RunID: ready.RunID, Repo: ready.Repo, PRNumber: ready.PRNumber,
	})
	if err != nil {
		return err
	}
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		row, _, err := tx.RecordDispatchedOutbox(ctx, handBackLabelRemovalKey(ready.ItemID), HandBackLabelRemovalKind, payload)
		if err != nil {
			return err
		}
		_, err = HandBackLabelBackupPayloadDigests(row)
		return err
	})
}

// removeLabel takes one label off an issue or pull request. A 404 means the
// label is not on it, which is the state the call asks for.
func (f *forge) removeLabel(ctx context.Context, repo repoRef, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo.path(), number, url.PathEscape(label))
	resp, err := f.do(ctx, http.MethodDelete, repo, path, "", nil)
	if err != nil {
		return fmt.Errorf("remove label: %w", err)
	}
	defer drainAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("remove label: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
}

// HandBackLabelTokenSource caches only the label-removal grant, separately
// from publication, filing, and read credentials. Each call revalidates trust
// and the App binding before it looks in the cache.
type HandBackLabelTokenSource struct {
	minter *Minter
	now    func() time.Time
	mu     sync.Mutex

	// tokens holds pointers so a holder that keeps this source in an
	// unexported field cannot print a cached token (see
	// FollowUpFilingTokenSource). Token returns a copy.
	tokens map[tokenCacheKey]*InstallationToken
}

func NewHandBackLabelTokenSource(m *Minter, now func() time.Time) *HandBackLabelTokenSource {
	return &HandBackLabelTokenSource{minter: m, now: now, tokens: map[tokenCacheKey]*InstallationToken{}}
}

// Format renders the source without its cache.
func (*HandBackLabelTokenSource) Format(f fmt.State, _ rune) {
	io.WriteString(f, "publish.HandBackLabelTokenSource") //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

// Token mints only HandBackLabelPermissions for the repository's current
// trusted installation.
func (s *HandBackLabelTokenSource) Token(ctx context.Context, repo string) (InstallationToken, error) {
	if s == nil || s.minter == nil || s.now == nil {
		return InstallationToken{}, errors.New("hand-back label token: nil dependency")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, parsed, repositoryID, _, err := s.minter.resolveTrusted(ctx, repo)
	if err != nil {
		return InstallationToken{}, err
	}
	key := tokenCacheKey{
		registrationID: binding.RegistrationID,
		installationID: binding.InstallationID,
		repositoryID:   repositoryID,
	}
	if tok, ok := s.tokens[key]; ok && tok.ExpiresAt.After(s.now().Add(tokenExpirySkew)) {
		return *tok, nil
	}
	tok, err := s.minter.mintResolved(ctx, binding, parsed, repositoryID,
		HandBackLabelPermissions, handBackLabelPermissionScopes)
	if err != nil {
		return InstallationToken{}, err
	}
	s.tokens[key] = &tok
	return tok, nil
}

// ValidateLabelName refuses a string that cannot be a GitHub label name: an
// empty name, one longer than GitHub's 50 characters, one with surrounding
// space, or one with a control character or invalid UTF-8.
func ValidateLabelName(label string) error {
	if label == "" || strings.TrimSpace(label) != label || !utf8.ValidString(label) ||
		utf8.RuneCountInString(label) > 50 || strings.ContainsFunc(label, unicode.IsControl) {
		return errors.New("label name is empty, too long, padded, or holds a control character")
	}
	return nil
}
