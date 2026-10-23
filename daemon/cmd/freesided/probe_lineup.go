package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	// lineupProbeRecordName sits beside the composition manifest. Nothing
	// that composes the manifest reads it (#2208).
	lineupProbeRecordName    = "lineup-probe.json"
	lineupProbeRecordVersion = "freeside-lineup-probe-v1"
)

func runProbeLineupMain(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runProbeLineupCommand(ctx, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided probe-lineup:", err)
		os.Exit(1)
	}
}

// lineupSurface is what a probe call runs through. The zero value "" is
// invalid by design.
type lineupSurface string

const (
	lineupSurfaceAgentImage     lineupSurface = "agent_image"
	lineupSurfaceReviewImage    lineupSurface = "review_image"
	lineupSurfaceJudgmentBinary lineupSurface = "judgment_binary"
)

// allLineupSurfaces is the single registration point for lineupSurface.
var allLineupSurfaces = []lineupSurface{
	lineupSurfaceAgentImage, lineupSurfaceReviewImage, lineupSurfaceJudgmentBinary,
}

func (s lineupSurface) valid() bool {
	switch s {
	case lineupSurfaceAgentImage, lineupSurfaceReviewImage, lineupSurfaceJudgmentBinary:
		return true
	default:
		return false
	}
}

// lineupVerdict is how one probe call ended: the CLI answered, the CLI
// refused, or the probe could not run. The zero value "" is invalid by
// design.
type lineupVerdict string

const (
	lineupVerdictPass    lineupVerdict = "pass"
	lineupVerdictRefused lineupVerdict = "refused"
	lineupVerdictError   lineupVerdict = "error"
)

// allLineupVerdicts is the single registration point for lineupVerdict.
var allLineupVerdicts = []lineupVerdict{lineupVerdictPass, lineupVerdictRefused, lineupVerdictError}

func (v lineupVerdict) valid() bool {
	switch v {
	case lineupVerdictPass, lineupVerdictRefused, lineupVerdictError:
		return true
	default:
		return false
	}
}

// lineupCall is one distinct launch the settings name, with every role that
// launches it. Model is the value the launch passes; empty passes none.
type lineupCall struct {
	Roles           []domain.RoleName
	Surface         lineupSurface
	Harness         domain.HarnessClientKind
	Image           string
	Model           string
	RequestedEffort domain.EffortLevel
	NativeEffort    string
	PinnedBuild     string
}

// lineupProbeRecord is lineup-probe.json: evidence that each launch the
// settings name was answered by its pinned CLI, written on pass and fail.
type lineupProbeRecord struct {
	Version        string                  `json:"version"`
	SettingsDigest domain.Digest           `json:"settings_digest"`
	DaemonBuild    string                  `json:"daemon_build"`
	ProbedAt       time.Time               `json:"probed_at"`
	Passed         bool                    `json:"passed"`
	Calls          []lineupProbeCallRecord `json:"calls"`
}

// lineupProbeCallRecord is one call. Image is null for the judgment binary;
// ObservedBuild and Refusal are null when the probe has none. A mismatch
// between the pinned and observed build does not fail the probe (#2209).
type lineupProbeCallRecord struct {
	Roles           []domain.RoleName        `json:"roles"`
	Surface         lineupSurface            `json:"surface"`
	Harness         domain.HarnessClientKind `json:"harness"`
	Image           *string                  `json:"image"`
	Model           string                   `json:"model"`
	RequestedEffort domain.EffortLevel       `json:"requested_effort"`
	NativeEffort    string                   `json:"native_effort"`
	PinnedBuild     string                   `json:"pinned_build"`
	ObservedBuild   *string                  `json:"observed_build"`
	Verdict         lineupVerdict            `json:"verdict"`
	Refusal         *string                  `json:"refusal"`
	Usage           ward.LineupProbeUsage    `json:"usage"`
}

// lineupProber makes one probe call and returns what the CLI printed. Its
// error is a probe that could not run, never a refusal.
type lineupProber interface {
	probe(ctx context.Context, call lineupCall) (ward.LineupProbeOutput, error)
}

type probeLineupConfig struct {
	Root         string
	RigTokenFile string
	ContainerBin string
}

func parseProbeLineupConfig(args []string, stderr io.Writer) (probeLineupConfig, error) {
	fs := flag.NewFlagSet("probe-lineup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg probeLineupConfig
	fs.StringVar(&cfg.Root, "root", "", "standing root whose daemon/settings.json names the lineup (absolute)")
	fs.StringVar(&cfg.RigTokenFile, "rig-token-file", "", "held rig's acquisition file; binds the probe's runtime objects into the rig")
	fs.StringVar(&cfg.ContainerBin, "container-bin", "container", "Apple container CLI")
	if err := fs.Parse(args); err != nil {
		return probeLineupConfig{}, err
	}
	if fs.NArg() != 0 {
		return probeLineupConfig{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if !filepath.IsAbs(cfg.Root) {
		return probeLineupConfig{}, errors.New("-root must be an absolute path")
	}
	return cfg, nil
}

// runProbeLineupCommand probes every launch the root's deputy settings name
// and writes lineup-probe.json beside the composition manifest. It refuses
// while a daemon holds the root's database, and fails when any call does.
func runProbeLineupCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := parseProbeLineupConfig(args, stderr)
	if err != nil {
		return err
	}
	stateRoot, err := filepath.EvalSymlinks(filepath.Join(cfg.Root, "daemon"))
	if err != nil {
		return fmt.Errorf("resolve state root: %w", err)
	}
	paths := newDeputyPaths(stateRoot)
	settings, err := loadDeputySettings(paths.Settings)
	if err != nil {
		return err
	}
	calls, err := lineupCalls(&settings)
	if err != nil {
		return err
	}
	lock, err := daemonlock.Acquire(paths.DB)
	if err != nil {
		return fmt.Errorf("a daemon holds this root, or its lock is unreadable: %w", err)
	}
	defer func() { _ = lock.Close() }() // Nothing follows the close to report its error to.
	containerBin, err := exec.LookPath(cfg.ContainerBin)
	if err != nil {
		return fmt.Errorf("find container CLI: %w", err)
	}
	prober := &productionLineupProber{
		settings:  &settings,
		runtime:   ward.NewCLIRuntime(containerBin),
		authorize: productionRigRuntimeAuthorizer(stateRoot, cfg.RigTokenFile),
		now:       time.Now,
	}
	_, _ = fmt.Fprintf(stderr, "freesided probe-lineup: build %s, %d calls\n", buildVersion(), len(calls))
	record := probeLineup(ctx, calls, prober, settings.Digest, buildVersion(), time.Now().UTC())
	return reportLineupProbe(stateRoot, record, stdout, stderr)
}

// reportLineupProbe writes the record, prints it, and names each failed
// call: its roles, model, effort, and CLI build, with the CLI's words.
func reportLineupProbe(stateRoot string, record lineupProbeRecord, stdout, stderr io.Writer) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := atomicfile.WriteFile(filepath.Join(stateRoot, lineupProbeRecordName), body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", lineupProbeRecordName, err)
	}
	if _, err := stdout.Write(body); err != nil {
		return err
	}
	if record.Passed {
		return nil
	}
	failed := 0
	for _, call := range record.Calls {
		if call.Verdict == lineupVerdictPass {
			continue
		}
		failed++
		build := "unknown"
		if call.ObservedBuild != nil {
			build = *call.ObservedBuild
		}
		_, _ = fmt.Fprintf(stderr, "%s %v: model %q at effort %q (sent %q) on %s: %s\n",
			call.Verdict, call.Roles, call.Model, call.RequestedEffort, call.NativeEffort, build, *call.Refusal)
	}
	return fmt.Errorf("%d of %d lineup calls failed; see %s", failed, len(record.Calls), lineupProbeRecordName)
}

// lineupCalls lists the distinct launches the settings name: the writers on
// the agent image, the reviewer on the review image, and, when judgments
// are on, every judgment role on the pinned host binary. Roles that launch
// the same surface, model, and effort share one call.
func lineupCalls(settings *loadedDeputySettings) ([]lineupCall, error) {
	var calls []lineupCall
	add := func(role domain.RoleName, call lineupCall) error {
		translation, err := domain.TranslateEffort(call.Harness, call.RequestedEffort)
		if err != nil {
			return fmt.Errorf("%s effort: %w", role, err)
		}
		call.NativeEffort = translation.Native
		for i := range calls {
			existing := calls[i]
			if existing.Surface == call.Surface && existing.Harness == call.Harness && existing.Image == call.Image &&
				existing.Model == call.Model &&
				existing.RequestedEffort == call.RequestedEffort && existing.NativeEffort == call.NativeEffort {
				calls[i].Roles = append(calls[i].Roles, role)
				return nil
			}
		}
		call.Roles = []domain.RoleName{role}
		calls = append(calls, call)
		return nil
	}
	for _, writer := range settings.writerSelections() {
		if err := add(writer.role, lineupCall{
			Surface: lineupSurfaceAgentImage, Harness: domain.HarnessClientClaudeCode, Image: settings.Images.Agent,
			Model:           domain.ClaudeCodeLaunchModel(writer.selection.Model),
			RequestedEffort: writer.selection.Effort, PinnedBuild: agentbaseline.ClaudeCodeHarnessBuild,
		}); err != nil {
			return nil, err
		}
	}
	reviewer := lineupCall{
		Surface: lineupSurfaceReviewImage, Harness: settings.reviewHarness(), Image: settings.Images.Review,
		Model: settings.Review.Model, RequestedEffort: settings.Review.Effort,
	}
	switch reviewer.Harness {
	case domain.HarnessClientCodexCLI:
		reviewer.PinnedBuild = agentbaseline.CodexHarnessBuild
	case domain.HarnessClientClaudeCode:
		reviewer.PinnedBuild = agentbaseline.ClaudeCodeHarnessBuild
	}
	if err := add(domain.RoleReviewer, reviewer); err != nil {
		return nil, err
	}
	if settings.Judgment.ClaudeBin == "" {
		return calls, nil
	}
	for _, role := range judgmentProbeRoles() {
		if err := add(role, lineupCall{
			Surface: lineupSurfaceJudgmentBinary, Harness: domain.HarnessClientClaudeCode,
			Model: settings.Judgment.Model, RequestedEffort: settings.Judgment.Effort,
			PinnedBuild: agentbaseline.ClaudeCallHarnessBuild,
		}); err != nil {
			return nil, err
		}
	}
	return calls, nil
}

// judgmentProbeRoles is every role a judgment site with an instruction
// serves, in judgmentSites order: the roles the judgment line binds.
func judgmentProbeRoles() []domain.RoleName {
	var roles []domain.RoleName
	for _, site := range judgmentSites(inference.Budget{}) {
		role, ok := domain.RoleForSite(site.ID)
		if site.Instruction == "" || !ok || slices.Contains(roles, role) {
			continue
		}
		roles = append(roles, role)
	}
	return roles
}

// probeLineup runs every call in order and judges it. A call that could not
// run is recorded as an error, so the record names every call either way.
// Once ctx ends, the remaining calls are recorded as errors without being
// started, so an interrupted probe binds no further runtime objects.
func probeLineup(
	ctx context.Context, calls []lineupCall, prober lineupProber,
	settingsDigest domain.Digest, build string, now time.Time,
) lineupProbeRecord {
	record := lineupProbeRecord{
		Version: lineupProbeRecordVersion, SettingsDigest: settingsDigest, DaemonBuild: build,
		ProbedAt: now, Passed: true, Calls: make([]lineupProbeCallRecord, 0, len(calls)),
	}
	for _, call := range calls {
		entry := lineupProbeCallRecord{
			Roles: slices.Clone(call.Roles), Surface: call.Surface, Harness: call.Harness,
			Model: call.Model, RequestedEffort: call.RequestedEffort, NativeEffort: call.NativeEffort,
			PinnedBuild: call.PinnedBuild,
		}
		if call.Image != "" {
			entry.Image = &call.Image
		}
		out, err := ward.LineupProbeOutput{}, ctx.Err()
		if err == nil {
			out, err = prober.probe(ctx, call)
		}
		if err != nil {
			message := err.Error()
			entry.Verdict, entry.Refusal = lineupVerdictError, &message
		} else {
			verdict := ward.JudgeLineupProbe(call.Harness, call.Model, out)
			if verdict.CLIBuild != "" {
				entry.ObservedBuild = &verdict.CLIBuild
			}
			entry.Usage = verdict.Usage
			entry.Verdict = lineupVerdictPass
			if !verdict.Passed {
				entry.Verdict, entry.Refusal = lineupVerdictRefused, &verdict.Refusal
			}
		}
		record.Passed = record.Passed && entry.Verdict == lineupVerdictPass
		record.Calls = append(record.Calls, entry)
	}
	return record
}

// productionLineupProber runs image calls through the container runtime and
// judgment calls through the pinned host binary. It reads the agent tree's
// setup token once, through the exporter image as the judgment credential
// reader does, and shares it between the writers and judgment.
type productionLineupProber struct {
	settings  *loadedDeputySettings
	runtime   ward.Runtime
	authorize ward.RuntimeResourceAuthorizer
	now       func() time.Time
	token     *ward.SetupToken
}

func (p *productionLineupProber) setupToken(ctx context.Context) (ward.SetupToken, error) {
	if p.token != nil {
		return *p.token, nil
	}
	read, err := ward.ReadSetupToken(
		ctx, p.runtime, p.settings.Images.Exporter, p.settings.AgentTree.AuthVolume, p.authorize)
	if err != nil {
		return ward.SetupToken{}, fmt.Errorf("read setup token from %s: %w", p.settings.AgentTree.AuthVolume, err)
	}
	p.token = &read.Token
	return read.Token, nil
}

func (p *productionLineupProber) probe(ctx context.Context, call lineupCall) (ward.LineupProbeOutput, error) {
	switch call.Surface {
	case lineupSurfaceAgentImage:
		token, err := p.setupToken(ctx)
		if err != nil {
			return ward.LineupProbeOutput{}, err
		}
		return ward.ProbeLineupImage(ctx, p.runtime, ward.LineupProbe{
			Harness: call.Harness, Image: call.Image, Model: call.Model, NativeEffort: call.NativeEffort,
			Credential: ward.SetupTokenProbeCredential(token),
		}, p.authorize)
	case lineupSurfaceReviewImage:
		review := p.settings.Review
		credential, err := ward.ReviewSnapshotProbeCredential(
			call.Harness, review.AuthMode, review.InputRoot, review.AuthSnapshot, p.now())
		if err != nil {
			return ward.LineupProbeOutput{}, err
		}
		return ward.ProbeLineupImage(ctx, p.runtime, ward.LineupProbe{
			Harness: call.Harness, Image: call.Image, Model: call.Model, NativeEffort: call.NativeEffort,
			Credential: credential,
		}, p.authorize)
	case lineupSurfaceJudgmentBinary:
		token, err := p.setupToken(ctx)
		if err != nil {
			return ward.LineupProbeOutput{}, err
		}
		driver, err := claudeinference.New(claudeinference.Config{
			Binary: p.settings.Judgment.ClaudeBin, SHA256: p.settings.Judgment.ClaudeSHA256,
		})
		if err != nil {
			return ward.LineupProbeOutput{}, err
		}
		out, err := driver.Probe(ctx, call.Model, call.NativeEffort, inference.Secret(token.Reveal()))
		if err != nil {
			return ward.LineupProbeOutput{}, err
		}
		return ward.LineupProbeOutput{
			Version: out.Version, Stdout: out.Stdout, Stderr: out.Stderr, ExitStatus: out.ExitStatus,
		}, nil
	}
	return ward.LineupProbeOutput{}, fmt.Errorf("no prober for surface %q", call.Surface)
}
