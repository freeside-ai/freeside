package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Config binds the inference boundary once at daemon composition.
type Config struct {
	StatePath  string
	AnchorPath string
	Binding    Binding
	Sites      []Site
	Advisory   AdvisoryWriter
	Now        func() time.Time
}

// AdvisoryWriter deliberately exposes only the append boundary. Inference
// code cannot read advisory output back into a later policy-affecting call.
type AdvisoryWriter interface {
	Append(context.Context, advisory.Entry) error
	Prune(context.Context) error
}

// Client validates and accounts every daemon-side inference call.
type Client struct {
	binding    Binding
	sites      map[string]Site
	ledger     *ledger
	advisory   AdvisoryWriter
	now        func() time.Time
	inFlightMu sync.Mutex
	inFlight   map[string]bool
}

// New constructs a client. A nil Driver is permitted and represents the
// explicit inference-down state; every call then returns its site fallback.
func New(cfg Config) (*Client, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AnchorPath == "" && cfg.StatePath != "" {
		cfg.AnchorPath = cfg.StatePath + ".anchor"
	}
	if cfg.Binding.Provider == "" || cfg.Binding.Model == "" || cfg.Advisory == nil {
		return nil, errors.New("invalid inference binding")
	}
	sites := make(map[string]Site, len(cfg.Sites))
	var sharedBudget *Budget
	for _, site := range cfg.Sites {
		if err := site.validate(); err != nil || sites[site.ID].ID != "" {
			return nil, errors.New("invalid inference site registry")
		}
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
		sites[site.ID] = site
	}
	ledger, err := openLedger(cfg.StatePath, cfg.AnchorPath, cfg.Now)
	if err != nil {
		return nil, err
	}
	return &Client{
		binding: cfg.Binding, sites: sites, ledger: ledger, advisory: cfg.Advisory,
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

// Call enforces the site allowlist, sensitivity declaration, redaction,
// cumulative budgets, output bounds, schema, producer label, and audit sample.
func (c *Client) Call(ctx context.Context, siteID, project, root string, fields map[string]InputField) (CallResult, error) {
	site, ok := c.sites[siteID]
	if !ok || project == "" || root == "" {
		return CallResult{}, errors.New("unknown or unscoped inference call")
	}
	allowed := make(map[string]Sensitivity, len(site.Fields))
	for _, policy := range site.Fields {
		allowed[policy.Name] = policy.Sensitivity
	}
	outbound := make(map[string]string, len(fields))
	for name, field := range fields {
		maximum, permitted := allowed[name]
		if !permitted || !field.Sensitivity.valid() || field.Sensitivity != maximum {
			return c.fallback(site, "outbound field policy rejected input"), nil
		}
		if !utf8.ValidString(field.Value) {
			return c.fallback(site, "outbound field is not valid UTF-8"), nil
		}
		value := field.Value
		if secret := c.binding.Credential.Reveal(); secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
		outbound[name] = value
	}
	if len(outbound) != len(site.Fields) {
		return c.fallback(site, "outbound field allowlist incomplete"), nil
	}
	body, err := json.Marshal(outbound)
	if err != nil || len(body) > site.MaxInputBytes {
		return c.fallback(site, "input size limit exceeded"), nil
	}
	digest := contentaddr.Sum(body)
	if c.binding.Driver == nil {
		return c.fallback(site, ErrUnavailable.Error()), nil
	}
	if !c.beginDriver(site.ID) {
		return c.fallback(site, "inference driver call remains in flight"), nil
	}
	record, err := c.ledger.reserveCall(site, project, root, c.binding.producer(), digest)
	if err != nil {
		c.endDriver(site.ID)
		return c.fallback(site, err.Error()), nil
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
	}
	go func() {
		response, err := c.binding.Driver.Complete(callCtx, request, c.binding.Credential)
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
			return c.fallback(site, ErrUnavailable.Error()), nil
		}
		response = result.response
	case <-callCtx.Done():
		close(abandoned)
		return c.fallback(site, ErrUnavailable.Error()), nil
	}
	if len(response.Output) > site.MaxOutputBytes || response.ComputeUnits < 0 ||
		response.ComputeUnits > site.MaxComputeUnits {
		return c.fallback(site, "driver response exceeded contract"), nil
	}
	if err := site.ValidateOutput(response.Output); err != nil {
		return c.fallback(site, "output schema rejected response"), nil
	}
	result := CallResult{Output: bytes.Clone(response.Output), Producer: c.binding.producer(), InputDigest: digest}
	if err := c.audit(ctx, site, record, result); err != nil {
		return c.fallback(site, "audit persistence failed"), err
	}
	return result, nil
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

func (c *Client) fallback(site Site, reason string) CallResult {
	return CallResult{Producer: c.binding.producer(), Fallback: true, Reason: reason, Output: []byte(site.FailSafe)}
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
