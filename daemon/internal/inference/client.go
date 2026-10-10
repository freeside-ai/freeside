package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Config binds the inference boundary once at daemon composition.
type Config struct {
	StatePath  string
	AnchorPath string
	// Roles resolves each call's role through the lineup. Nil is the
	// explicit inference-down state: every call returns its site fallback.
	Roles RoleSource
	// Health hears which roles could not be admitted. Nil reports nothing.
	Health   HealthReporter
	Sites    []Site
	Advisory AdvisoryWriter
	Now      func() time.Time
}

// AdvisoryWriter deliberately exposes only the append boundary. Inference
// code cannot read advisory output back into a later policy-affecting call.
type AdvisoryWriter interface {
	Append(context.Context, advisory.Entry) error
	Prune(context.Context) error
}

// Client validates and accounts every daemon-side inference call.
type Client struct {
	roles      RoleSource
	health     HealthReporter
	callLaunch domain.CallLaunch
	sites      map[string]Site
	siteRoles  map[string]domain.RoleName
	ledger     *ledger
	advisory   AdvisoryWriter
	now        func() time.Time
	inFlightMu sync.Mutex
	inFlight   map[string]bool
}

// New constructs a client. A nil RoleSource, or a resolved role with a nil
// Driver, is permitted and represents the explicit inference-down state;
// every such call returns its site fallback.
func New(cfg Config) (*Client, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AnchorPath == "" && cfg.StatePath != "" {
		cfg.AnchorPath = cfg.StatePath + ".anchor"
	}
	if cfg.Advisory == nil {
		return nil, errors.New("invalid inference composition")
	}
	callLaunch, err := domain.NewCallLaunch()
	if err != nil {
		return nil, err
	}
	sites := make(map[string]Site, len(cfg.Sites))
	siteRoles := make(map[string]domain.RoleName, len(cfg.Sites))
	var sharedBudget *Budget
	for _, site := range cfg.Sites {
		if err := site.validate(); err != nil || sites[site.ID].ID != "" {
			return nil, errors.New("invalid inference site registry")
		}
		// Every site belongs to exactly one role (plan §5.13); a site no
		// role names has no line to look up and cannot be registered.
		role, ok := domain.RoleForSite(site.ID)
		if !ok {
			return nil, errors.New("invalid inference site registry")
		}
		siteRoles[site.ID] = role
		if sharedBudget == nil {
			budget := site.Budget
			sharedBudget = &budget
		} else if site.Budget.Window != sharedBudget.Window ||
			site.Budget.Project != sharedBudget.Project || site.Budget.Global != sharedBudget.Global ||
			site.Budget.MaxCallsPerRoot != sharedBudget.MaxCallsPerRoot ||
			site.Budget.MaxStarvationPerRoot != sharedBudget.MaxStarvationPerRoot {
			return nil, errors.New("inference sites disagree on shared cumulative budgets")
		}
		site.Fields = append([]FieldPolicy(nil), site.Fields...)
		if site.Annotation != nil {
			annotation := *site.Annotation
			annotation.Materiality = append([]string(nil), annotation.Materiality...)
			annotation.Confidence = append([]string(nil), annotation.Confidence...)
			annotation.ReducesWork = append([]AnnotationOutput(nil), annotation.ReducesWork...)
			annotation.SeverityMappings = append([]SeverityMapping(nil), annotation.SeverityMappings...)
			annotation.NormalizedSeverityCeilings = append(
				[]SeverityCeiling(nil), annotation.NormalizedSeverityCeilings...,
			)
			annotation.SecondAdjudicationRules = append([]SecondAdjudicationRule(nil), annotation.SecondAdjudicationRules...)
			site.Annotation = &annotation
		}
		if site.Adjudication != nil {
			adjudication := *site.Adjudication
			adjudication.GoalRelationships = append([]string(nil), adjudication.GoalRelationships...)
			adjudication.ProposedCompatibilities = append(
				[]string(nil), adjudication.ProposedCompatibilities...,
			)
			adjudication.Routes = append([]string(nil), adjudication.Routes...)
			adjudication.Rows = make([]AdjudicationRow, len(adjudication.Rows))
			for index, row := range site.Adjudication.Rows {
				adjudication.Rows[index] = row
				if row.ProposedCompatibility != nil {
					compatibility := *row.ProposedCompatibility
					adjudication.Rows[index].ProposedCompatibility = &compatibility
				}
			}
			adjudication.Confidence = append([]string(nil), adjudication.Confidence...)
			adjudication.ReducesWork = append([]string(nil), adjudication.ReducesWork...)
			adjudication.SeverityMappings = append(
				[]SeverityMapping(nil), adjudication.SeverityMappings...,
			)
			adjudication.NormalizedSeverityCeilings = append(
				[]SeverityCeiling(nil), adjudication.NormalizedSeverityCeilings...,
			)
			adjudication.SecondAdjudicationRules = append(
				[]SecondAdjudicationRule(nil), adjudication.SecondAdjudicationRules...,
			)
			site.Adjudication = &adjudication
		}
		if site.DriftAudit != nil {
			driftAudit := *site.DriftAudit
			driftAudit.Verdicts = append([]string(nil), driftAudit.Verdicts...)
			driftAudit.Confidence = append([]string(nil), driftAudit.Confidence...)
			driftAudit.ReducesWork = append([]string(nil), driftAudit.ReducesWork...)
			driftAudit.SeverityMappings = append([]SeverityMapping(nil), driftAudit.SeverityMappings...)
			driftAudit.NormalizedSeverityCeilings = append(
				[]SeverityCeiling(nil), driftAudit.NormalizedSeverityCeilings...,
			)
			driftAudit.SecondAdjudicationRules = append(
				[]SecondAdjudicationRule(nil), driftAudit.SecondAdjudicationRules...,
			)
			site.DriftAudit = &driftAudit
		}
		sites[site.ID] = site
	}
	ledger, err := openLedger(cfg.StatePath, cfg.AnchorPath, cfg.Now)
	if err != nil {
		return nil, err
	}
	return &Client{
		roles: cfg.Roles, health: cfg.Health, callLaunch: callLaunch,
		sites: sites, siteRoles: siteRoles, ledger: ledger, advisory: cfg.Advisory,
		now: cfg.Now, inFlight: map[string]bool{},
	}, nil
}

// SupportsSite reports whether the composition registered siteID. Callers use
// it to keep optional inference sites fail-safe when a narrower registry is
// deliberately composed.
func (c *Client) SupportsSite(siteID string) bool {
	if c == nil {
		return false
	}
	_, ok := c.sites[siteID]
	return ok
}

// admittedCall is a role admitted for one call: the admission, the identity
// the ledger stamps, and the means to make the call.
type admittedCall struct {
	identity   callIdentity
	producer   string
	prompt     RolePrompt
	driver     Driver
	credential Secret
}

// errRoleUnbound is the fixed fallback reason for a role that could not be
// admitted. The specific reason goes to the health item, not the result.
var errRoleUnbound = errors.New("judgment role has no admissible lineup line")

// admit resolves the site's role and runs the wardless admission on what the
// source returned. The source's answer is input, not a verdict: the prompt
// bytes are held to the digest the line names, and the admission class
// re-checks the agent, the launch proof, and the credential window against
// this call's deadline. Any failure is the role being unbound.
func (c *Client) admit(ctx context.Context, site Site, role domain.RoleName) (admittedCall, error) {
	return admitRole(ctx, c.roles, c.callLaunch, c.now(), site, role)
}

func admitRole(
	ctx context.Context, roles RoleSource, launch domain.CallLaunch, now time.Time, site Site, role domain.RoleName,
) (admittedCall, error) {
	resolved, err := roles.ResolveRole(ctx, role)
	if err != nil {
		return admittedCall{}, err
	}
	prompt := resolved.Prompt
	if prompt.Name == "" || resolved.Line.PromptName != prompt.Name {
		return admittedCall{}, fmt.Errorf("role %s: the lineup names prompt %q, the daemon resolves %q",
			role, resolved.Line.PromptName, prompt.Name)
	}
	if actual := domain.Digest(contentaddr.Sum(prompt.Body)); actual != prompt.Digest {
		return admittedCall{}, fmt.Errorf("role %s: prompt %s resolves to %s, its bytes hash to %s",
			role, prompt.Name, prompt.Digest, actual)
	}
	if !resolved.CredentialSource.valid() {
		return admittedCall{}, fmt.Errorf("role %s: credential source %q is unknown", role, resolved.CredentialSource)
	}
	admission, err := domain.AdmitWardlessRole(domain.WardlessAdmissionInput{
		Role: role, Line: resolved.Line, LineupRevision: resolved.LineupRevision,
		Agent: resolved.Agent, Route: resolved.Route, Adapter: resolved.Adapter, Offer: resolved.Offer,
		PromptDigest: prompt.Digest, Enrollment: resolved.Enrollment, Generation: resolved.Generation,
		Deadline: now.UTC().Add(site.Timeout), ExpiryMargin: resolved.ExpiryMargin,
		LaunchProof: resolved.LaunchProof,
	})
	if err != nil {
		return admittedCall{}, err
	}
	effort, err := domain.TranslateEffort(resolved.Adapter.ClientKind, resolved.Agent.Effort)
	if err != nil {
		return admittedCall{}, fmt.Errorf("role %s: %w", role, err)
	}
	// A call's treatment is a run's with the call launch in the launch
	// position (plan §5.4), so it is the same at every site of an agent.
	treatment, err := domain.ComputeTreatmentDigest(
		resolved.Route, resolved.Adapter.Digest, launch.Digest, resolved.Offer,
		resolved.Agent.Effort, effort.Effective(),
	)
	if err != nil {
		return admittedCall{}, fmt.Errorf("role %s: %w", role, err)
	}
	identity := callIdentity{
		Admission: admission, TreatmentDigest: treatment, SiteContractDigest: site.ContractDigest(),
		CredentialSource: resolved.CredentialSource,
		RequestedModelID: resolved.Offer.RouteModelID, RequestedEffort: resolved.Agent.Effort,
	}
	if role.JudgesWrittenWork() {
		// Recorded, never enforced (plan revision 65): the call runs whatever
		// the relation is.
		for _, writer := range domain.WritingRoles {
			identity.Independence = append(identity.Independence, independenceEntry{
				WritingRole: writer,
				Lineage:     domain.CompareLineage(resolved.Offer.LineageGroup, resolved.WriterLineage[writer]),
			})
		}
	}
	return admittedCall{
		identity: identity, producer: resolved.Route.ServiceOperator + "/" + resolved.Offer.RouteModelID,
		prompt: prompt, driver: resolved.Driver, credential: resolved.Credential,
	}, nil
}

// Call enforces the site allowlist, sensitivity declaration, the role's
// admission through the lineup, redaction, cumulative budgets, output bounds,
// schema, producer label, and audit sample.
func (c *Client) Call(ctx context.Context, siteID, project, root string, fields map[string]InputField) (CallResult, error) {
	site, ok := c.sites[siteID]
	if !ok || project == "" || root == "" {
		return CallResult{}, errors.New("unknown or unscoped inference call")
	}
	allowed := make(map[string]Sensitivity, len(site.Fields))
	for _, policy := range site.Fields {
		allowed[policy.Name] = policy.Sensitivity
	}
	for name, field := range fields {
		maximum, permitted := allowed[name]
		if !permitted || !field.Sensitivity.valid() || field.Sensitivity != maximum {
			return fallback(site, unboundProducer, "outbound field policy rejected input"), nil
		}
		if !utf8.ValidString(field.Value) {
			return fallback(site, unboundProducer, "outbound field is not valid UTF-8"), nil
		}
	}
	if len(fields) != len(site.Fields) {
		return fallback(site, unboundProducer, "outbound field allowlist incomplete"), nil
	}
	if c.roles == nil {
		return fallback(site, unboundProducer, ErrUnavailable.Error()), nil
	}
	role := c.siteRoles[site.ID]
	admitted, err := c.admit(ctx, site, role)
	switch {
	case errors.Is(err, ErrRoleOff):
		c.reportResolved(ctx, role)
		return fallback(site, unboundProducer, ErrUnavailable.Error()), nil
	case err != nil:
		if c.health != nil {
			c.health.RoleUnbound(ctx, role, err.Error())
		}
		return fallback(site, unboundProducer, errRoleUnbound.Error()), nil
	}
	c.reportResolved(ctx, role)
	outbound := make(map[string]string, len(fields))
	for name, field := range fields {
		value := field.Value
		if secret := admitted.credential.Reveal(); secret != "" {
			value = redactCredential(value, secret)
		}
		outbound[name] = value
	}
	body, err := json.Marshal(outbound)
	if err != nil || len(body) > site.MaxInputBytes {
		return fallback(site, admitted.producer, "input size limit exceeded"), nil
	}
	digest := contentaddr.Sum(body)
	if admitted.driver == nil {
		return fallback(site, admitted.producer, ErrUnavailable.Error()), nil
	}
	if !c.beginDriver(site.ID) {
		return fallback(site, admitted.producer, "inference driver call remains in flight"), nil
	}
	record, err := c.ledger.reserveCall(site, project, root, admitted.producer, digest, admitted.identity)
	if err != nil {
		c.endDriver(site.ID)
		return fallback(site, admitted.producer, err.Error()), nil
	}
	identity := &CallIdentity{
		RecordID: record.ID, Role: role, AgentDigest: admitted.identity.Admission.AgentDigest,
		PromptDigest:       admitted.identity.Admission.PromptDigest,
		TreatmentDigest:    admitted.identity.TreatmentDigest,
		SiteContractDigest: admitted.identity.SiteContractDigest,
	}
	fail := func(reason string) CallResult {
		result := fallback(site, admitted.producer, reason)
		result.Identity = identity
		return result
	}
	// Only the outer call can produce and settle this record's audit. Release
	// its retention protection when that path returns, even if a provider that
	// ignored cancellation remains orphaned in the background.
	defer c.ledger.releaseCall(record.ID)
	callCtx, cancel := context.WithTimeout(ctx, site.Timeout)
	defer cancel()
	type completion struct {
		response Response
		err      error
	}
	completed := make(chan completion)
	abandoned := make(chan struct{})
	request := Request{
		SiteID: site.ID, InputDigest: digest, Fields: cloneStrings(outbound),
		MaxOutput: site.MaxOutputBytes, MaxComputeUnits: site.MaxComputeUnits,
		RolePrompt: bytes.Clone(admitted.prompt.Body), RolePromptDigest: admitted.prompt.Digest,
	}
	go func() {
		response, err := admitted.driver.Complete(callCtx, request, admitted.credential)
		select {
		case completed <- completion{response: response, err: err}:
		case <-abandoned:
			c.endDriver(site.ID)
		}
	}()
	var response Response
	select {
	case result := <-completed:
		defer c.endDriver(site.ID)
		if result.err != nil {
			return fail(ErrUnavailable.Error()), nil
		}
		response = result.response
	case <-callCtx.Done():
		close(abandoned)
		return fail(ErrUnavailable.Error()), nil
	}
	if len(response.Output) > site.MaxOutputBytes || response.ComputeUnits < 0 ||
		response.ComputeUnits > site.MaxComputeUnits || !response.Observed.valid() {
		return fail("driver response exceeded contract"), nil
	}
	// The observation is recorded for every answer within the contract,
	// whether or not its output then passes the schema: what answered is a
	// fact about the call, not about the answer's quality.
	if err := c.ledger.recordObserved(record.ID, observedFrom(response.Observed)); err != nil {
		return fail("call record persistence failed"), err
	}
	if err := site.ValidateOutput(response.Output); err != nil {
		result := fail("output schema rejected response")
		var refusal *authorOutputRefusal
		if site.ID == PublicationAuthorExplainSiteID && errors.As(err, &refusal) {
			result.AuthorOutputRefusalReason = refusal.reason
		}
		return result, nil
	}
	result := CallResult{
		Output: bytes.Clone(response.Output), Producer: admitted.producer, InputDigest: digest, Identity: identity,
	}
	if err := c.audit(ctx, site, record, result); err != nil {
		return fail("audit persistence failed"), err
	}
	return result, nil
}

// RoleCheck is one judgment role's standing admission result.
type RoleCheck struct {
	Role domain.RoleName
	// Off reports a role this configuration deliberately leaves off.
	Off bool
	// Err is why no call for the role can be admitted now; nil when the role
	// is admitted or off.
	Err error
}

// roleSite is one site and the role that calls it.
type roleSite struct {
	site Site
	role domain.RoleName
}

// checkRoleSites admits each role once per site, in site order, and returns
// the roles' results in domain.AllRoleNames order.
func checkRoleSites(
	ctx context.Context, roles RoleSource, launch domain.CallLaunch, now time.Time, sites []roleSite,
) []RoleCheck {
	slices.SortFunc(sites, func(a, b roleSite) int { return strings.Compare(a.site.ID, b.site.ID) })
	checks := map[domain.RoleName]RoleCheck{}
	for _, entry := range sites {
		// A role's sites differ only in their deadline; its first failure is
		// the role's result.
		if prior, seen := checks[entry.role]; seen && (prior.Off || prior.Err != nil) {
			continue
		}
		check := RoleCheck{Role: entry.role}
		if _, err := admitRole(ctx, roles, launch, now, entry.site, entry.role); errors.Is(err, ErrRoleOff) {
			check.Off = true
		} else if err != nil {
			check.Err = err
		}
		checks[entry.role] = check
	}
	out := make([]RoleCheck, 0, len(checks))
	for _, role := range domain.AllRoleNames {
		if check, ok := checks[role]; ok {
			out = append(out, check)
		}
	}
	return out
}

// CheckRoles admits every role the sites call, as a call at each of them
// would be admitted at now. It makes no call, reads no ledger, and reports to
// no one, so preflight can name a role the lineup cannot fill before a daemon
// starts. A site no role names is an error.
func CheckRoles(ctx context.Context, roles RoleSource, sites []Site, now time.Time) ([]RoleCheck, error) {
	launch, err := domain.NewCallLaunch()
	if err != nil {
		return nil, err
	}
	entries := make([]roleSite, 0, len(sites))
	for _, site := range sites {
		role, ok := domain.RoleForSite(site.ID)
		if !ok {
			return nil, fmt.Errorf("inference site %s belongs to no role", site.ID)
		}
		entries = append(entries, roleSite{site: site, role: role})
	}
	return checkRoleSites(ctx, roles, launch, now, entries), nil
}

// CheckRoles admits every role with a registered site as CheckRoles does and
// tells the health reporter each result. The daemon runs it at startup, so a
// role the lineup cannot fill is named before a site returns its fail-safe,
// and so an item whose role has since been fixed is resolved even while that
// item is what holds the work that would have called the role. A nil
// RoleSource has no role to check.
func (c *Client) CheckRoles(ctx context.Context) []RoleCheck {
	if c.roles == nil {
		return nil
	}
	entries := make([]roleSite, 0, len(c.sites))
	for id, site := range c.sites {
		entries = append(entries, roleSite{site: site, role: c.siteRoles[id]})
	}
	checks := checkRoleSites(ctx, c.roles, c.callLaunch, c.now(), entries)
	for _, check := range checks {
		if check.Err == nil {
			c.reportResolved(ctx, check.Role)
		} else if c.health != nil {
			c.health.RoleUnbound(ctx, check.Role, check.Err.Error())
		}
	}
	return checks
}

func (c *Client) reportResolved(ctx context.Context, role domain.RoleName) {
	if c.health != nil {
		c.health.RoleResolved(ctx, role)
	}
}

// observedFrom turns a driver's observation into the record's shape, where a
// fact the driver does not have is null and not an empty string.
func observedFrom(observed Observed) observedCall {
	optional := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	return observedCall{
		ModelID: optional(observed.ModelID), ServingOperator: optional(observed.ServingOperator),
		OutputTokens: observed.OutputTokens,
	}
}

func redactCredential(value, secret string) string {
	value = strings.ReplaceAll(value, secret, "[REDACTED]")
	encoded, err := json.Marshal(secret)
	if err != nil || len(encoded) < 2 {
		return value
	}
	escaped := string(encoded[1 : len(encoded)-1])
	return strings.ReplaceAll(value, escaped, "[REDACTED]")
}

func (c *Client) beginDriver(siteID string) bool {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	if c.inFlight[siteID] {
		return false
	}
	c.inFlight[siteID] = true
	return true
}

func (c *Client) endDriver(siteID string) {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	delete(c.inFlight, siteID)
}

func fallback(site Site, producer, reason string) CallResult {
	return CallResult{Producer: producer, Fallback: true, Reason: reason, Output: []byte(site.FailSafe)}
}

func (c *Client) audit(ctx context.Context, site Site, record callRecord, result CallResult) error {
	// The durable site-call ordinal is daemon-owned and deterministic across
	// restart; untrusted input cannot steer whether its call is sampled.
	if !record.AuditRequired {
		return nil
	}
	created := c.now().UTC()
	if err := c.advisory.Append(ctx, advisory.Entry{
		ID: record.ID, RootLineage: record.RootLineage, Site: site.ID, Producer: result.Producer, Kind: "audit_sample",
		InputDigest: result.InputDigest, Body: string(result.Output), CreatedAt: created,
		RetainUntil: created.Add(site.Retention),
	}); err != nil {
		return err
	}
	return c.ledger.completeAudit(record.ID)
}

// Maintain enforces physical retention for advisory rows and inference-call
// metadata. Maintenance failure never changes daemon workflow authority, but
// later inference calls still fail closed at their audit boundary.
func (c *Client) Maintain(ctx context.Context) error {
	return errors.Join(c.advisory.Prune(ctx), c.ledger.pruneCalls())
}

func decodeStrictObject(data []byte, dst any, max int) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	return strictjson.Decode(data, dst, strictjson.RejectInvalidUTF8, strictjson.Limit(max))
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var visit func() error
	visit = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return errors.New("duplicate object key")
				}
				seen[key] = true
				if err := visit(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := visit(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
		_, err = decoder.Token()
		return err
	}
	return visit()
}
