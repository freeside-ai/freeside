package inference_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

type fallbackDriver func(context.Context) (inference.Response, error)

func (f fallbackDriver) Complete(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
	return f(ctx)
}

// Every Call fallback path must identify its cause without copying input,
// provider output, or an error's text into the diagnostic log.
func TestCallFallbackCausesAndLogs(t *testing.T) {
	for _, reason := range inference.AllFallbackReasons {
		t.Run(string(reason), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const marker = "UNTRUSTED-CONTENT-MUST-NOT-BE-LOGGED"
				var logs bytes.Buffer
				dir := t.TempDir()
				statePath := filepath.Join(dir, "ledger.json")
				site := inference.DiscussionSite(testBudget(1))
				site.Timeout = time.Second
				response := inference.Response{Output: []byte(`{"reply":"` + marker + `"}`)}
				blockLedger := func() error {
					if err := os.Remove(statePath); err != nil {
						return err
					}
					return os.Mkdir(statePath, 0o700)
				}
				var driver inference.Driver = fallbackDriver(func(context.Context) (inference.Response, error) { return response, nil })
				store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10)
				if err != nil {
					t.Fatal(err)
				}
				cfg := inference.Config{
					StatePath: statePath, Sites: []inference.Site{site}, Advisory: store,
					Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
				}
				fields := siteFields(site)
				for name, field := range fields {
					field.Value = marker
					fields[name] = field
				}
				roles := fake.Roles{Provider: "fake", Model: "test"}
				var beforeCall func(*inference.Client)
				switch reason {
				case inference.FallbackFieldPolicyRejected:
					fields[marker] = inference.InputField{Value: marker}
				case inference.FallbackFieldNotUTF8:
					field := fields["reason"]
					field.Value = "\xff"
					fields["reason"] = field
				case inference.FallbackFieldAllowlistIncomplete:
					delete(fields, "reason")
				case inference.FallbackRoleSourceAbsent:
					// Roles stays nil below.
				case inference.FallbackRoleOff:
					roles.Edit = func(domain.RoleName, *inference.RoleCall) error { return inference.ErrRoleOff }
				case inference.FallbackRoleUnbound:
					roles.Edit = func(domain.RoleName, *inference.RoleCall) error { return errors.New(marker) }
				case inference.FallbackInputTooLarge:
					field := fields["reason"]
					field.Value = strings.Repeat(marker, site.MaxInputBytes)
					fields["reason"] = field
				case inference.FallbackDriverAbsent:
					driver = nil
				case inference.FallbackCallInFlight:
					release := make(chan struct{})
					defer close(release)
					driver = stuckDriver{release: release}
					beforeCall = func(c *inference.Client) {
						first, err := c.Call(t.Context(), site.ID, "project", "run", fields)
						if err != nil || first.Reason != inference.FallbackDeadlineExceeded {
							t.Fatalf("first call: %+v, %v", first, err)
						}
					}
				case inference.FallbackBudgetExhausted:
					beforeCall = func(c *inference.Client) {
						first, err := c.Call(t.Context(), site.ID, "project", "run", fields)
						if err != nil || first.Fallback {
							t.Fatalf("first call: %+v, %v", first, err)
						}
					}
				case inference.FallbackLedgerUnavailable:
					beforeCall = func(*inference.Client) {
						if err := blockLedger(); err != nil {
							t.Fatal(err)
						}
					}
				case inference.FallbackDriverFailed:
					driver = fallbackDriver(func(context.Context) (inference.Response, error) { return response, errors.New(marker) })
				case inference.FallbackDeadlineExceeded:
					driver = fallbackDriver(func(ctx context.Context) (inference.Response, error) { <-ctx.Done(); return response, ctx.Err() })
				case inference.FallbackResponseExceededContract:
					response.Output = []byte(strings.Repeat(marker, site.MaxOutputBytes))
				case inference.FallbackCallRecordPersistenceFailed:
					driver = fallbackDriver(func(context.Context) (inference.Response, error) { return response, blockLedger() })
				case inference.FallbackOutputSchemaRejected:
					response.Output = []byte(marker)
				case inference.FallbackAuditPersistenceFailed:
					cfg.Advisory = &failingAdvisory{store: store, fail: true}
				}
				roles.Driver = driver
				if reason != inference.FallbackRoleSourceAbsent {
					cfg.Roles = roles
				}
				client, err := inference.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if beforeCall != nil {
					beforeCall(client)
				}
				logs.Reset()
				result, callErr := client.Call(t.Context(), site.ID, "project", "run", fields)
				wantError := reason == inference.FallbackCallRecordPersistenceFailed || reason == inference.FallbackAuditPersistenceFailed
				if (callErr != nil) != wantError || !result.Fallback || result.Reason != reason || !slices.Contains(inference.AllFallbackReasons, result.Reason) {
					t.Fatalf("fallback = %+v, error %v; want %s", result, callErr, reason)
				}
				line := logs.String()
				level := "WARN"
				if reason == inference.FallbackRoleOff || reason == inference.FallbackRoleSourceAbsent {
					level = "DEBUG"
				}
				for _, want := range []string{"site=attention_discussion", "project=project", "root_lineage=run", "producer=", "reason=" + string(reason), "level=" + level} {
					if !strings.Contains(line, want) {
						t.Errorf("log missing %q: %s", want, line)
					}
				}
				if strings.Contains(line, marker) || strings.Count(line, "\n") != 1 {
					t.Fatalf("unsafe or repeated log: %s", line)
				}
				if result.Identity != nil && !strings.Contains(line, "record_id="+result.Identity.RecordID) {
					t.Errorf("missing record ID: %s", line)
				}
			})
		})
	}
}

func TestAdjudicatorPreservesPersistenceFallbackCause(t *testing.T) {
	for _, reason := range []inference.FallbackReason{
		inference.FallbackCallRecordPersistenceFailed, inference.FallbackAuditPersistenceFailed,
	} {
		t.Run(string(reason), func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "ledger.json")
			store, err := advisory.Open(filepath.Join(dir, "advisory.json"), 20, 16<<10)
			if err != nil {
				t.Fatal(err)
			}
			driver := fallbackDriver(func(context.Context) (inference.Response, error) {
				if reason == inference.FallbackCallRecordPersistenceFailed {
					if err := os.Remove(statePath); err != nil {
						return inference.Response{}, err
					}
					if err := os.Mkdir(statePath, 0o700); err != nil {
						return inference.Response{}, err
					}
				}
				return inference.Response{Output: []byte(acceptedAdjudicatorOutput), ComputeUnits: 1}, nil
			})
			client, err := inference.New(inference.Config{
				StatePath: statePath, Roles: fake.Roles{Provider: "fake", Model: "test", Driver: driver},
				Sites:    []inference.Site{inference.AdjudicatorSite(testBudget(1))},
				Advisory: &failingAdvisory{store: store, fail: reason == inference.FallbackAuditPersistenceFailed},
			})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := client.AdjudicateFindings(t.Context(), "project", "run", adjudicatorInput())
			var refusal *inference.AdjudicationUnavailable
			if entries != nil || !errors.Is(err, inference.ErrAdjudicationNotAvailable) ||
				!errors.As(err, &refusal) || refusal.Cause != inference.AdjudicationJudgmentFallback ||
				refusal.Fallback != reason || refusal.Err == nil || !errors.Is(err, refusal.Err) {
				t.Fatalf("persistence refusal = %+v, %v; want %s", refusal, err, reason)
			}
		})
	}
}
