package inference

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	ledgerVersionV1 = "freeside.inference-budget/v1"
	ledgerVersionV2 = "freeside.inference-budget/v2"
	ledgerVersion   = "freeside.inference-budget/v3"
)

type usage struct {
	WindowStart    time.Time     `json:"window_start"`
	Calls          int64         `json:"calls"`
	ComputeUnits   int64         `json:"compute_units"`
	AttentionItems int64         `json:"attention_items"`
	Starvation     time.Duration `json:"starvation"`
}

type ledgerState struct {
	Version   string                     `json:"version"`
	Epoch     string                     `json:"epoch"`
	Usage     map[string]usage           `json:"usage"`
	Calls     []callRecord               `json:"calls,omitempty"`
	AuditDebt map[string]bool            `json:"audit_debt,omitempty"`
	Attention map[string]attentionRecord `json:"attention,omitempty"`
}

type callRecord struct {
	ID               string    `json:"id"`
	Site             string    `json:"site"`
	Project          string    `json:"project"`
	RootLineage      string    `json:"root_lineage"`
	Producer         string    `json:"producer"`
	InputDigest      string    `json:"input_digest"`
	CalledAt         time.Time `json:"called_at"`
	RetainUntil      time.Time `json:"retain_until"`
	Ordinal          int64     `json:"ordinal"`
	AuditRequired    bool      `json:"audit_required"`
	AuditComplete    bool      `json:"audit_complete"`
	AuditTransferred bool      `json:"audit_transferred"`
	// Identity is what the call ran as. Every call this version reserves
	// carries one; a record a v2 ledger wrote has none, and stays readable
	// until retention prunes it.
	Identity *callIdentity `json:"identity,omitempty"`
}

// callIdentity stamps a call with the admission it ran under and what it
// was compared by (plan §5.4, §8). It is written when the call is reserved,
// before the driver runs, and only Observed is written afterwards.
type callIdentity struct {
	// Admission is the wardless admission result: role, agent and prompt
	// digests, lineup revision, enrollment generation, and the launch proof.
	Admission          domain.WardlessAdmission `json:"admission"`
	TreatmentDigest    domain.Digest            `json:"treatment_digest"`
	SiteContractDigest domain.Digest            `json:"site_contract_digest"`
	// CredentialSource says whose bytes authenticated the call, beside the
	// enrollment generation the admission names.
	CredentialSource CredentialSource `json:"credential_source"`
	// RequestedModelID and RequestedEffort are the admitted agent's. The
	// call does not yet send them (#1619), so Observed may disagree, and the
	// record claims no agreement.
	RequestedModelID string             `json:"requested_model_id"`
	RequestedEffort  domain.EffortLevel `json:"requested_effort"`
	// Observed is null until a driver answered within the site's contract.
	Observed *observedCall `json:"observed"`
	// ShadowOf is the record id of the call this one shadows. Nothing writes
	// it yet (#1429).
	ShadowOf *string `json:"shadow_of"`
	// SelectionSource and SelectionRecordID are omitted when the lineup
	// selected the agent, as domain.AdmissionAgentBinding omits them; the
	// alternate-agent card adds its value (#1432).
	SelectionSource   domain.AgentSelectionSource `json:"selection_source,omitempty"`
	SelectionRecordID domain.Digest               `json:"selection_record_id,omitempty"`
	// Independence is the judging roles' record (plan §7): one entry per
	// writing role, in domain.WritingRoles order. It is null for every other
	// role. It is a recorded fact and gates nothing.
	Independence []independenceEntry `json:"independence"`
}

// observedCall is what the driver reported. A null identifier is a fact the
// driver does not have.
type observedCall struct {
	ModelID         *string `json:"model_id"`
	ServingOperator *string `json:"serving_operator"`
	OutputTokens    int64   `json:"output_tokens"`
}

type independenceEntry struct {
	WritingRole domain.RoleName        `json:"writing_role"`
	Lineage     domain.LineageRelation `json:"lineage"`
}

func (i callIdentity) validate() error {
	if err := i.Admission.Validate(); err != nil {
		return err
	}
	if !contentaddr.Valid(string(i.TreatmentDigest)) || !contentaddr.Valid(string(i.SiteContractDigest)) ||
		!i.CredentialSource.valid() || i.RequestedModelID == "" ||
		!slices.Contains(domain.AllEffortLevels, i.RequestedEffort) {
		return errors.New("invalid inference call identity")
	}
	if observed := i.Observed; observed != nil {
		for _, id := range []*string{observed.ModelID, observed.ServingOperator} {
			if id != nil && (*id == "" || !(Observed{ModelID: *id}).valid()) {
				return errors.New("invalid inference call observation")
			}
		}
		if observed.OutputTokens < 0 {
			return errors.New("invalid inference call observation")
		}
	}
	if i.ShadowOf != nil && !contentaddr.Valid(*i.ShadowOf) {
		return errors.New("invalid inference call shadow link")
	}
	switch {
	case i.SelectionSource == "" && i.SelectionRecordID == "":
	case !slices.Contains(domain.AllAgentSelectionSources, i.SelectionSource),
		i.SelectionSource == domain.AgentSelectionSourceLineup,
		!contentaddr.Valid(string(i.SelectionRecordID)):
		return errors.New("invalid inference call selection source")
	}
	// A stored record is history, so it is checked for shape and never
	// against today's registries: which roles write, which roles judge, and
	// which role a site belongs to can all change while a record is retained,
	// and a ledger that then failed to load would send every site to its
	// fallback with no way to prune the records that did it.
	for _, entry := range i.Independence {
		if !slices.Contains(domain.AllRoleNames, entry.WritingRole) || entry.Lineage.Validate() != nil {
			return errors.New("invalid inference call independence record")
		}
	}
	return nil
}

type attentionRecord struct {
	Site        string    `json:"site"`
	Project     string    `json:"project"`
	RootLineage string    `json:"root_lineage"`
	RecordedAt  time.Time `json:"recorded_at"`
	Permitted   bool      `json:"permitted"`
}

type ledger struct {
	mu         sync.Mutex
	path       string
	anchorPath string
	state      ledgerState
	now        func() time.Time
	disabled   error
	protected  map[string]bool
}

func openLedger(path, anchorPath string, now func() time.Time) (*ledger, error) {
	if path == "" || anchorPath == "" || path == anchorPath || now == nil {
		return nil, errors.New("invalid inference ledger configuration")
	}
	l := &ledger{path: path, anchorPath: anchorPath, now: now, state: ledgerState{
		Version: ledgerVersion, Usage: map[string]usage{}, AuditDebt: map[string]bool{},
		Attention: map[string]attentionRecord{},
	}, protected: map[string]bool{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		l.disabled = fmt.Errorf("create inference state directory: %w", err)
		return l, nil
	}
	body, err := os.ReadFile(path) //nolint:gosec // daemon-owned configured state path
	if errors.Is(err, os.ErrNotExist) {
		anchor, anchorErr := os.ReadFile(anchorPath) //nolint:gosec // daemon-owned external anchor path
		if anchorErr == nil || !errors.Is(anchorErr, os.ErrNotExist) {
			_ = anchor
			l.disabled = errors.New("inference ledger missing after initialization")
			return l, nil
		}
		epoch, epochErr := newLedgerEpoch()
		if epochErr != nil {
			l.disabled = epochErr
			return l, nil
		}
		l.state.Epoch = epoch
		if err := l.persist(l.state); err != nil {
			return l, nil
		}
		if err := atomicfile.WriteFileNoReplace(anchorPath, []byte(epoch), 0o600); err != nil {
			l.disabled = fmt.Errorf("persist inference ledger anchor: %w", err)
		}
		return l, nil
	}
	if err != nil {
		l.disabled = fmt.Errorf("read inference ledger: %w", err)
		return l, nil
	}
	if err := json.Unmarshal(body, &l.state); err != nil {
		l.disabled = errors.New("decode inference ledger")
		return l, nil
	}
	migrate := false
	switch l.state.Version {
	case ledgerVersionV1:
		// V1 had no durable summary for a required audit whose detailed call
		// record was retention-pruned. Refuse a v1-labelled file carrying the
		// new field; a rolled-back binary would ignore and erase it.
		if len(l.state.AuditDebt) != 0 || recordsCarryIdentity(l.state.Calls) {
			l.disabled = errors.New("decode inference ledger")
			return l, nil
		}
		l.state.Version = ledgerVersion
		migrate = true
	case ledgerVersionV2:
		// V2 records carry no call identity. Refuse a v2-labelled file that
		// carries one, for the reason v1 refuses audit debt: the binary that
		// writes v2 would ignore the field and erase it on its next write.
		if recordsCarryIdentity(l.state.Calls) {
			l.disabled = errors.New("decode inference ledger")
			return l, nil
		}
		l.state.Version = ledgerVersion
		migrate = true
	case ledgerVersion:
	default:
		l.disabled = errors.New("decode inference ledger")
		return l, nil
	}
	if validateLedgerState(l.state) != nil {
		l.disabled = errors.New("decode inference ledger")
		return l, nil
	}
	if l.state.Attention == nil {
		l.state.Attention = map[string]attentionRecord{}
	}
	if l.state.AuditDebt == nil {
		l.state.AuditDebt = map[string]bool{}
	}
	anchor, anchorErr := os.ReadFile(anchorPath) //nolint:gosec // daemon-owned external anchor path
	if anchorErr != nil || string(anchor) != l.state.Epoch {
		l.disabled = errors.New("inference ledger anchor missing or mismatched")
		return l, nil
	}
	if migrate {
		if err := l.persist(l.state); err != nil {
			return l, nil
		}
	}
	return l, nil
}

func recordsCarryIdentity(records []callRecord) bool {
	return slices.ContainsFunc(records, func(record callRecord) bool { return record.Identity != nil })
}

func newLedgerEpoch() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate inference ledger epoch: %w", err)
	}
	return contentaddr.Sum(value[:]), nil
}

func validateLedgerState(state ledgerState) error {
	if state.Version != ledgerVersion || !contentaddr.Valid(state.Epoch) || state.Usage == nil {
		return errors.New("invalid inference ledger header")
	}
	for key, current := range state.Usage {
		if key == "" || current.WindowStart.IsZero() || current.Calls < 0 ||
			current.ComputeUnits < 0 || current.AttentionItems < 0 || current.Starvation < 0 {
			return errors.New("invalid inference ledger usage")
		}
	}
	for _, record := range state.Calls {
		if !contentaddr.Valid(record.ID) || record.Site == "" || record.Project == "" || record.RootLineage == "" ||
			record.Producer == "" || !contentaddr.Valid(record.InputDigest) || record.CalledAt.IsZero() ||
			!record.RetainUntil.After(record.CalledAt) || record.Ordinal < 1 ||
			(!record.AuditRequired && (record.AuditComplete || record.AuditTransferred)) ||
			(record.AuditTransferred && !record.AuditComplete) {
			return errors.New("invalid inference call record")
		}
		// A record written before v3 has no identity; one that has an
		// identity has a whole one.
		if identity := record.Identity; identity != nil {
			if err := identity.validate(); err != nil {
				return err
			}
		}
	}
	for site, pending := range state.AuditDebt {
		if site == "" || !pending {
			return errors.New("invalid inference audit debt")
		}
	}
	for id, record := range state.Attention {
		if id == "" || record.Site == "" || record.Project == "" || record.RootLineage == "" || record.RecordedAt.IsZero() {
			return errors.New("invalid inference attention record")
		}
	}
	return nil
}

// reserveCall admits one call against the cumulative budgets and writes its
// record, identity included, before the driver runs.
func (l *ledger) reserveCall(
	site Site, project, root, producer, digest string, identity callIdentity,
) (callRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled != nil {
		return callRecord{}, l.disabled
	}
	calls := append([]callRecord(nil), l.state.Calls...)
	auditDebt := maps.Clone(l.state.AuditDebt)
	forceAudit := auditDebt[site.ID]
	for index := range calls {
		if calls[index].Site == site.ID && calls[index].AuditRequired && !calls[index].AuditComplete {
			forceAudit = true
		}
	}
	now := l.now().UTC()
	next, exhausted := advanceUsage(l.state.Usage, site, project, root, usage{
		Calls: 1, ComputeUnits: site.MaxComputeUnits, Starvation: site.Timeout,
	}, now)
	ordinal := next["site:"+site.ID].Calls
	called := callRecord{
		Site: site.ID, Project: project, RootLineage: root, Producer: producer,
		InputDigest: digest, CalledAt: now, RetainUntil: now.Add(site.Retention), Ordinal: ordinal,
		AuditRequired: !exhausted && (forceAudit || (ordinal-1)%site.AuditEvery == 0),
		Identity:      &identity,
	}
	called.ID = contentaddr.Sum([]byte(site.ID + "\x00" + project + "\x00" + root + "\x00" +
		fmt.Sprint(ordinal) + "\x00" + digest + "\x00" + now.Format(time.RFC3339Nano)))
	if !exhausted && forceAudit {
		for index := range calls {
			if calls[index].Site == site.ID && calls[index].AuditRequired && !calls[index].AuditComplete {
				// A crash, provider failure, invalid response, or advisory failure
				// produced no accepted sample. Transfer that exact obligation only
				// after a replacement carrying it has been admitted.
				calls[index].AuditComplete = true
				calls[index].AuditTransferred = true
			}
		}
		delete(auditDebt, site.ID)
	}
	retainedCalls := make([]callRecord, 0, len(calls)+1)
	for _, record := range calls {
		if now.Before(record.RetainUntil) || l.protected[record.ID] {
			retainedCalls = append(retainedCalls, record)
		} else if record.AuditRequired && !record.AuditComplete {
			auditDebt[record.Site] = true
		}
	}
	if !exhausted {
		retainedCalls = append(retainedCalls, called)
	}
	state := ledgerState{
		Version: ledgerVersion, Epoch: l.state.Epoch, Usage: next,
		Calls: retainedCalls, AuditDebt: auditDebt, Attention: l.state.Attention,
	}
	if err := l.persist(state); err != nil {
		return callRecord{}, err
	}
	if exhausted {
		return callRecord{}, errors.New("inference cumulative budget exhausted")
	}
	l.protected[called.ID] = true
	return called, nil
}

func advanceUsage(
	currentUsage map[string]usage, site Site, project, root string, delta usage, now time.Time,
) (map[string]usage, bool) {
	keys := []struct {
		key    string
		limits Limits
	}{
		{"site:" + site.ID, site.Budget.Site},
		{"project:" + project, site.Budget.Project},
		{"global", site.Budget.Global},
		{"root:" + root, Limits{
			Calls: site.Budget.MaxCallsPerRoot, ComputeUnits: site.Budget.Global.ComputeUnits,
			AttentionItems: site.Budget.Global.AttentionItems, Starvation: site.Budget.MaxStarvationPerRoot,
		}},
	}
	next := make(map[string]usage, len(currentUsage))
	for key, value := range currentUsage {
		next[key] = value
	}
	exhausted := false
	for _, item := range keys {
		current := next[item.key]
		if current.WindowStart.IsZero() || !now.Before(current.WindowStart.Add(site.Budget.Window)) {
			current = usage{WindowStart: now}
		}
		current.Calls = saturatingAdd(current.Calls, delta.Calls)
		current.ComputeUnits = saturatingAdd(current.ComputeUnits, delta.ComputeUnits)
		current.AttentionItems = saturatingAdd(current.AttentionItems, delta.AttentionItems)
		current.Starvation = time.Duration(saturatingAdd(int64(current.Starvation), int64(delta.Starvation)))
		if current.Calls > item.limits.Calls || current.ComputeUnits > item.limits.ComputeUnits ||
			current.AttentionItems > item.limits.AttentionItems || current.Starvation > item.limits.Starvation {
			exhausted = true
		}
		next[item.key] = current
	}
	return next, exhausted
}

func saturatingAdd(current, delta int64) int64 {
	if delta < 0 || current > math.MaxInt64-delta {
		return math.MaxInt64
	}
	return current + delta
}

func (l *ledger) completeAudit(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled != nil {
		return l.disabled
	}
	calls := append([]callRecord(nil), l.state.Calls...)
	for index := range calls {
		if calls[index].ID != id {
			continue
		}
		if !calls[index].AuditRequired {
			return errors.New("call has no audit obligation")
		}
		calls[index].AuditComplete = true
		return l.persist(ledgerState{
			Version: ledgerVersion, Epoch: l.state.Epoch, Usage: l.state.Usage,
			Calls: calls, AuditDebt: l.state.AuditDebt, Attention: l.state.Attention,
		})
	}
	return errors.New("unknown inference call record")
}

// recordObserved writes what the driver reported onto the call's record. It
// is the only write to an identity after the call was reserved.
func (l *ledger) recordObserved(id string, observed observedCall) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled != nil {
		return l.disabled
	}
	calls := append([]callRecord(nil), l.state.Calls...)
	for index := range calls {
		if calls[index].ID != id {
			continue
		}
		if calls[index].Identity == nil {
			return errors.New("call record has no identity")
		}
		identity := *calls[index].Identity
		identity.Observed = &observed
		calls[index].Identity = &identity
		return l.persist(ledgerState{
			Version: ledgerVersion, Epoch: l.state.Epoch, Usage: l.state.Usage,
			Calls: calls, AuditDebt: l.state.AuditDebt, Attention: l.state.Attention,
		})
	}
	return errors.New("unknown inference call record")
}

func (l *ledger) recordAttention(site Site, project, root, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled != nil {
		return l.disabled
	}
	if existing, exists := l.state.Attention[id]; exists {
		if existing.Permitted {
			return nil
		}
		return errors.New("inference cumulative attention budget exhausted")
	}
	now := l.now().UTC()
	usageNext, exhausted := advanceUsage(l.state.Usage, site, project, root, usage{AttentionItems: 1}, now)
	attention := make(map[string]attentionRecord, len(l.state.Attention)+1)
	for key, value := range l.state.Attention {
		attention[key] = value
	}
	attention[id] = attentionRecord{
		Site: site.ID, Project: project, RootLineage: root, RecordedAt: now, Permitted: !exhausted,
	}
	if err := l.persist(ledgerState{
		Version: ledgerVersion, Epoch: l.state.Epoch, Usage: usageNext,
		Calls: l.state.Calls, AuditDebt: l.state.AuditDebt, Attention: attention,
	}); err != nil {
		return err
	}
	if exhausted {
		return errors.New("inference cumulative attention budget exhausted")
	}
	return nil
}

func (l *ledger) releaseCall(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.protected, id)
}

func (l *ledger) pruneCalls() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled != nil {
		return l.disabled
	}
	now := l.now().UTC()
	retained := make([]callRecord, 0, len(l.state.Calls))
	auditDebt := maps.Clone(l.state.AuditDebt)
	for _, record := range l.state.Calls {
		if now.Before(record.RetainUntil) || l.protected[record.ID] {
			retained = append(retained, record)
		} else if record.AuditRequired && !record.AuditComplete {
			auditDebt[record.Site] = true
		}
	}
	if len(retained) == len(l.state.Calls) {
		return nil
	}
	return l.persist(ledgerState{
		Version: l.state.Version, Epoch: l.state.Epoch, Usage: l.state.Usage,
		Calls: retained, AuditDebt: auditDebt, Attention: l.state.Attention,
	})
}

func (l *ledger) persist(state ledgerState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(l.path, body, 0o600); err != nil {
		l.disabled = fmt.Errorf("persist inference ledger: %w", err)
		return l.disabled
	}
	l.state = state
	return nil
}
