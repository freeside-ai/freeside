package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The lineup probe (#2208) makes one bounded provider call through an image
// a lineup line launches, with that line's model and effort, so a model the
// pinned CLI or the subscription refuses, or an effort the model refuses,
// fails before a run spends anything. It runs the same CLI flags the launch
// does, under provider-only egress, and returns what the CLI printed; the
// caller judges it with JudgeLineupProbe.
//
// The credential reaches the container the way every other host-to-guest
// byte does, through CopyIntoContainer into the container's own root
// filesystem, and the command removes it before the probe's output is
// written: the Claude command reads the token into a shell variable and
// deletes the staged copy before the CLI runs, and the Codex command deletes
// CODEX_HOME, auth.json with it, as soon as the CLI exits. The root
// filesystem is exported only when the container stopped on its own, into a
// private directory removed before the probe returns, and an export that
// still holds either path is refused.
//
// The two copy targets sit at the root, whose parent always exists: the
// host copies in as soon as the container starts, before its script can
// create anything, as the enrollment seeder's /credential-input does.
const (
	lineupProbeRoot      = "/var/lib/freeside/lineup-probe"
	lineupProbeStageDir  = "/lineup-probe-stage"
	lineupProbeReadyDir  = "/lineup-probe-ready"
	lineupProbeOutDir    = lineupProbeRoot + "/out"
	lineupProbeCredName  = "credential"
	lineupProbeVersion   = lineupProbeOutDir + "/version"
	lineupProbeStdout    = lineupProbeOutDir + "/stdout"
	lineupProbeStderr    = lineupProbeOutDir + "/stderr"
	lineupProbeStatus    = lineupProbeOutDir + "/status"
	lineupProbeOutputCap = 64 << 10
	// lineupProbeReadyTicks bounds, in one-second ticks, the container's wait
	// for the host's two copies. lineupProbeTimeout bounds the whole run from
	// start to stop: the copies, --version, and one provider call, whose
	// refusal arrives in about a second and whose answer at a high effort
	// can take most of a minute.
	lineupProbeReadyTicks = 60
	lineupProbeTimeout    = 4 * time.Minute
	// lineupProbeExportTimeout bounds the export, which runs detached from
	// the caller's cancellation for the reason readCredProof gives: a
	// container CLI killed inside an export leaves its temporary archive
	// behind.
	lineupProbeExportTimeout = 2 * time.Minute
	// lineupProbeCodexExpiryFloor is the least access-token lifetime a Codex
	// probe starts with. The probe never refreshes, so a token that could
	// expire inside the call is renewed by the operator first.
	lineupProbeCodexExpiryFloor = 10 * time.Minute
	// LineupProbePrompt is the probe's one message.
	LineupProbePrompt = "Reply with the single word OK."
)

// LineupProbeCredential is the credential one probe container reads. Build
// it with SetupTokenProbeCredential or ReviewSnapshotProbeCredential; the zero
// value is refused.
type LineupProbeCredential struct {
	mode CodexAuthMode
	body []byte
}

// SetupTokenProbeCredential wraps the Claude setup token a writer or a
// Claude reviewer launches with.
func SetupTokenProbeCredential(token SetupToken) LineupProbeCredential {
	return LineupProbeCredential{mode: CodexAuthSetupToken, body: token.Reveal()}
}

// ReviewSnapshotProbeCredential reads a reviewer's auth snapshot from its
// private input root and derives the container-facing credential the review
// lifecycle would seed: the setup token for a Claude reviewer, or the Codex
// auth.json without its refresh token. A Codex access token that expires
// within lineupProbeCodexExpiryFloor of now is refused, because the probe cannot
// refresh it.
func ReviewSnapshotProbeCredential(
	harness domain.HarnessClientKind, mode CodexAuthMode, inputRoot, snapshot string, now time.Time,
) (LineupProbeCredential, error) {
	provider, err := lineupProbeProvider(harness)
	if err != nil {
		return LineupProbeCredential{}, err
	}
	if !provider.acceptsAuthMode(mode) {
		return LineupProbeCredential{}, fmt.Errorf("%s reviewer does not take auth mode %q", harness, mode)
	}
	_, body, err := readCodexReviewInput(inputRoot, snapshot, maxCodexAuthSnapshotBytes)
	if err != nil {
		return LineupProbeCredential{}, fmt.Errorf("read review auth snapshot: %w", err)
	}
	derived, expires, err := provider.inspectAgentAuthSnapshot(mode, body)
	if err != nil {
		return LineupProbeCredential{}, fmt.Errorf("review auth snapshot: %w", err)
	}
	if provider.requiresExpiringCredential() && (expires == nil || expires.Before(now.Add(lineupProbeCodexExpiryFloor))) {
		return LineupProbeCredential{}, errors.New(
			"review auth snapshot's access token expires within ten minutes; run freesided renew-codex first")
	}
	return LineupProbeCredential{mode: mode, body: derived}, nil
}

// LineupProbe is one call to make through an image.
type LineupProbe struct {
	// Harness is the CLI the image runs: claude_code or codex_cli.
	Harness domain.HarnessClientKind
	// Image is the digest-pinned image the role launches.
	Image string
	// Model is the launch model; empty sends none (Claude only).
	Model string
	// NativeEffort is the effort as the CLI takes it
	// (domain.TranslateEffort); empty sends none.
	NativeEffort string
	Credential   LineupProbeCredential
}

// LineupProbeOutput is what a probe printed, unjudged. Version is the CLI's
// --version output; Stdout and Stderr are the call's; ExitStatus is its exit
// status. ProviderReached reports whether the egress proxy relayed at least
// one byte from the provider, which tells a call the provider refused from
// one that never reached it; JudgeLineupProbe does not read it.
type LineupProbeOutput struct {
	Version         []byte
	Stdout          []byte
	Stderr          []byte
	ExitStatus      int
	ProviderReached bool
}

func (p LineupProbe) validate() error {
	provider, err := lineupProbeProvider(p.Harness)
	if err != nil {
		return err
	}
	switch {
	case !digestPinnedImagePattern.MatchString(p.Image):
		return errors.New("lineup probe image is not digest-pinned")
	case p.Harness == domain.HarnessClientCodexCLI && p.Model == "":
		return errors.New("a Codex lineup probe needs a model")
	case !lineupProbeValue(p.Model) || !lineupProbeValue(p.NativeEffort):
		return errors.New("lineup probe model or effort is not a single option value")
	case len(p.Credential.body) == 0 || !provider.acceptsAuthMode(p.Credential.mode):
		return fmt.Errorf("lineup probe credential does not fit the %s harness", p.Harness)
	}
	return nil
}

// lineupProbeValue admits what a launch passes as one option value: no
// leading dash, no space or control character. Empty means "send none".
func lineupProbeValue(v string) bool {
	return len(v) <= 256 && !strings.HasPrefix(v, "-") &&
		!strings.ContainsFunc(v, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsGraphic(r) })
}

func lineupProbeProvider(harness domain.HarnessClientKind) (reviewProvider, error) {
	switch harness {
	case domain.HarnessClientClaudeCode:
		return claudeReviewProvider{}, nil
	case domain.HarnessClientCodexCLI:
		return codexReviewProvider{}, nil
	}
	return nil, fmt.Errorf("no lineup probe for harness %q", harness)
}

// lineupProbeCommand is the container's whole program. It waits for the
// host's ready copy, takes the credential out of the stage, prints the CLI
// build, runs one call with the launch's flags, and records the call's exit
// status. The prompt is a positional argument, never shell text.
func lineupProbeCommand(p LineupProbe, homeTarget, configTarget string) []string {
	stagedCredential := shellQuote(lineupProbeStageDir + "/" + lineupProbeCredName)
	prologue := "set -e; mkdir -p " + shellQuote(homeTarget) + " " + shellQuote(configTarget) + " " +
		shellQuote(lineupProbeOutDir) + "; cd " + shellQuote(homeTarget) + "; " +
		"n=0; while [ ! -f " + shellQuote(lineupProbeReadyDir+"/"+seedReadyFile) + " ]; do n=$((n+1)); [ \"$n\" -le " +
		strconv.Itoa(lineupProbeReadyTicks) + " ] || exit 1; sleep 1; done; "
	outputs := " < /dev/null > " + shellQuote(lineupProbeStdout) + " 2> " + shellQuote(lineupProbeStderr) + "; "
	var call string
	switch p.Harness {
	case domain.HarnessClientClaudeCode:
		// The flags are claudeinference's judgment launch (commandArgs) and
		// the review's --safe-mode; a writer's launch takes the same model
		// and effort options.
		call = "token=\"$(cat " + stagedCredential + ")\"; rm -rf " + shellQuote(lineupProbeStageDir) + "; set +e; " +
			"claude --version > " + shellQuote(lineupProbeVersion) + " 2>&1; " +
			"CLAUDE_CODE_OAUTH_TOKEN=\"$token\" claude -p \"$1\" --output-format json" +
			lineupProbeOption("--model", p.Model) + lineupProbeOption("--effort", p.NativeEffort) +
			" --safe-mode --tools '' --strict-mcp-config --mcp-config " + shellQuote(`{"mcpServers":{}}`) +
			" --no-session-persistence --max-turns 1" + outputs +
			"status=$?; unset token; "
	case domain.HarnessClientCodexCLI:
		// The flags are codexReviewCommand's, without the review's schema,
		// output file, and workspace access check.
		auth := shellQuote(CodexAuthFileTarget)
		effort := ""
		if p.NativeEffort != "" {
			effort = " -c " + shellQuote("model_reasoning_effort=\""+p.NativeEffort+"\"")
		}
		call = "cp " + stagedCredential + " " + auth + "; chmod 0600 " + auth + "; rm -rf " +
			shellQuote(lineupProbeStageDir) + "; set +e; " +
			"codex --version > " + shellQuote(lineupProbeVersion) + " 2>&1; " +
			"codex exec --json --ephemeral --skip-git-repo-check -s read-only -C " + shellQuote(homeTarget) +
			" -m " + shellQuote(p.Model) + effort +
			" -c project_doc_max_bytes=0 --ignore-user-config --ignore-rules" +
			" --disable apps -c " + shellQuote("web_search=\"disabled\"") + " -- \"$1\"" + outputs +
			"status=$?; rm -rf " + shellQuote(configTarget) + "; "
	}
	return []string{
		"sh", "-c", prologue + call +
			"printf '%s\\n' \"$status\" > " + shellQuote(lineupProbeStatus) + "; sync; exit 0",
		"freeside-lineup-probe", LineupProbePrompt,
	}
}

func lineupProbeOption(flag, value string) string {
	if value == "" {
		return ""
	}
	return " " + flag + " " + shellQuote(value)
}

// ProbeLineupImage runs one probe container: a host-only network whose
// CONNECT proxy admits only the credential's provider endpoint, and a
// container verified against its spec before it starts, given its
// credential once it runs, and exported once it stops on its own. Every
// object it creates carries a fresh ownership label and is removed before it
// returns. authorize, when set, grants the container and network names
// before either is created, as preflight's helpers do.
func ProbeLineupImage(
	ctx context.Context, rt Runtime, p LineupProbe, authorize RuntimeResourceAuthorizer,
) (LineupProbeOutput, error) {
	return probeLineupImage(ctx, rt, p, authorize, Config{})
}

// probeLineupImage is ProbeLineupImage with the ward settings it runs under,
// so a test can inject the proxy's upstream dialer.
func probeLineupImage(
	ctx context.Context, rt Runtime, p LineupProbe, authorize RuntimeResourceAuthorizer, base Config,
) (LineupProbeOutput, error) {
	if rt == nil {
		return LineupProbeOutput{}, errors.New("lineup probe requires a runtime")
	}
	if err := p.validate(); err != nil {
		return LineupProbeOutput{}, err
	}
	cfg := base.withDefaults()
	owner, err := newOwnershipLabel()
	if err != nil {
		return LineupProbeOutput{}, err
	}
	name := "freeside-lineup-probe-" + owner.Value[:12]
	network := name + "-egress"
	if authorize != nil {
		if err := authorize(ctx, RuntimeResourceNames{Containers: []string{name}, Networks: []string{network}}); err != nil {
			return LineupProbeOutput{}, err
		}
	}
	run := &lineupProbeRun{
		cfg: cfg, ops: newRuntimeOps(rt, cfg), rt: rt, owner: owner, name: name, network: network,
		labels: append(runLabels("lineup-probe-"+owner.Value[:12]), owner),
	}
	out, runErr := run.probe(ctx, p)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), max(cfg.TeardownTimeout, time.Second))
	defer cancel()
	if cleanupErr := run.teardown(cleanupCtx); cleanupErr != nil {
		return LineupProbeOutput{}, errors.Join(runErr, cleanupErr)
	}
	if runErr != nil {
		return LineupProbeOutput{}, runErr
	}
	return out, nil
}

type lineupProbeRun struct {
	cfg                   Config
	ops                   runtimeOps
	rt                    Runtime
	owner                 Label
	labels                []Label
	name, network         string
	container, networkObj objectClaim
	proxy                 *connectProxy
}

func (r *lineupProbeRun) probe(ctx context.Context, p LineupProbe) (LineupProbeOutput, error) {
	provider, err := lineupProbeProvider(p.Harness)
	if err != nil {
		return LineupProbeOutput{}, err
	}
	r.networkObj.attempted = true
	if err := r.rt.CreateNetwork(ctx, r.network, slices.Clone(r.labels)); err != nil {
		return LineupProbeOutput{}, fmt.Errorf("create lineup probe network: %w", err)
	}
	r.networkObj.owned = true
	report, err := r.rt.InspectNetwork(ctx, r.network)
	if err != nil {
		return LineupProbeOutput{}, fmt.Errorf("inspect lineup probe network: %w", err)
	}
	if r.networkObj.fingerprint, err = ownedFingerprint(
		report.CreationDate, report.Labels, report.LabelsObserved, r.owner,
	); err != nil {
		return LineupProbeOutput{}, fmt.Errorf("lineup probe network ownership: %w", err)
	}
	r.proxy, err = startConnectProxy(
		context.WithoutCancel(ctx), report.IPv4Gateway, report.IPv4Subnet,
		p.Credential.mode.providerEndpoints(), r.cfg.EgressProxyTimeout, r.cfg.EgressDialContext, time.Now,
	)
	if err != nil {
		return LineupProbeOutput{}, fmt.Errorf("start lineup probe proxy: %w", err)
	}
	spec := ContainerSpec{
		Name: r.name, Image: p.Image,
		Command: lineupProbeCommand(p, provider.homeTarget(), provider.configHomeTarget()),
		Env:     append(provider.containerEnv(), proxyEnvironment(r.proxy.URL())...),
		Labels:  slices.Clone(r.labels), Network: r.network,
		Size: DefaultLaunchSize(LaunchReview),
	}
	r.container.attempted = true
	if err := r.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return LineupProbeOutput{}, fmt.Errorf("create lineup probe container: %w", err)
	}
	r.container.owned = true
	rep, err := r.rt.Inspect(ctx, r.name)
	if err != nil {
		return LineupProbeOutput{}, fmt.Errorf("inspect lineup probe container: %w", err)
	}
	if r.container.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, r.owner); err != nil {
		return LineupProbeOutput{}, fmt.Errorf("lineup probe container ownership: %w", err)
	}
	if err := verifyAgentAllowlist(rep, spec); err != nil {
		return LineupProbeOutput{}, err
	}
	if err := r.rt.StartContainer(ctx, r.name); err != nil {
		return LineupProbeOutput{}, fmt.Errorf("start lineup probe container: %w", err)
	}
	if err := r.deliver(ctx, p.Credential); err != nil {
		return LineupProbeOutput{}, err
	}
	if err := r.ops.waitStopped(ctx, r.name, r.container, r.owner, lineupProbeTimeout); err != nil {
		return LineupProbeOutput{}, fmt.Errorf("lineup probe container: %w", err)
	}
	out, err := r.collect(ctx)
	if err != nil {
		return LineupProbeOutput{}, err
	}
	out.ProviderReached = !r.proxy.LastProviderByte().IsZero()
	if err := r.rt.DeleteContainer(ctx, r.name); err != nil {
		return LineupProbeOutput{}, fmt.Errorf("delete lineup probe container: %w", err)
	}
	if err := r.ops.verifyContainerAbsent(ctx, r.name, r.container, r.owner, CheckTeardown); err != nil {
		return LineupProbeOutput{}, err
	}
	r.container = objectClaim{}
	return out, nil
}

// deliver copies the credential, then the ready sentinel, into the running
// container. The host's staged copy lives in a private temporary directory
// removed before this returns.
func (r *lineupProbeRun) deliver(ctx context.Context, credential LineupProbeCredential) (retErr error) {
	stage, err := os.MkdirTemp("", "freeside-lineup-probe-")
	if err != nil {
		return errors.New("create lineup probe staging directory")
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			retErr = errors.Join(retErr, errors.New("remove lineup probe staging directory"))
		}
	}()
	input, ready := filepath.Join(stage, "input"), filepath.Join(stage, "ready")
	for _, dir := range []string{input, ready} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return errors.New("create lineup probe staging directory")
		}
	}
	if err := os.WriteFile(filepath.Join(input, lineupProbeCredName), credential.body, 0o600); err != nil {
		return errors.New("stage lineup probe credential")
	}
	if err := os.WriteFile(filepath.Join(ready, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		return errors.New("stage lineup probe ready sentinel")
	}
	for _, copy := range []struct{ source, target string }{
		{input, lineupProbeStageDir}, {ready, lineupProbeReadyDir},
	} {
		copyCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := r.rt.CopyIntoContainer(copyCtx, r.name, copy.source, copy.target)
		cancel()
		if err != nil {
			// The runtime's error names paths, never file contents.
			return fmt.Errorf("copy lineup probe input: %w", err)
		}
	}
	return nil
}

// collect exports the stopped container and reads its four output files.
func (r *lineupProbeRun) collect(ctx context.Context) (out LineupProbeOutput, retErr error) {
	dir, err := os.MkdirTemp("", "freeside-lineup-probe-export-")
	if err != nil {
		return LineupProbeOutput{}, errors.New("create lineup probe export directory")
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			out, retErr = LineupProbeOutput{}, errors.Join(retErr, errors.New("remove lineup probe export directory"))
		}
	}()
	if err := ctx.Err(); err != nil {
		return LineupProbeOutput{}, err
	}
	exportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lineupProbeExportTimeout)
	defer cancel()
	tarPath := filepath.Join(dir, "rootfs.tar")
	if err := r.ops.materializeRootFS(exportCtx, r.name, tarPath, CheckTeardown); err != nil {
		return LineupProbeOutput{}, err
	}
	if err := ctx.Err(); err != nil {
		return LineupProbeOutput{}, err
	}
	f, err := os.Open(tarPath) //nolint:gosec // G304: gate-owned path under a fresh temp directory.
	if err != nil {
		return LineupProbeOutput{}, errors.New("open lineup probe export")
	}
	defer func() { _ = f.Close() }() // Read-only descriptor.
	return readLineupProbeArchive(f)
}

// readLineupProbeArchive walks the export once. It returns the four output
// files and refuses an archive that still holds the staged credential or
// CODEX_HOME, a duplicate output entry, or an output that is not a
// regular file within the cap. A missing version, stdout, or stderr reads
// as empty; a missing or malformed status fails, since without it the probe
// never ran to its end.
func readLineupProbeArchive(r io.Reader) (LineupProbeOutput, error) {
	want := map[string]*[]byte{}
	var out LineupProbeOutput
	var status []byte
	for p, dst := range map[string]*[]byte{
		lineupProbeVersion: &out.Version, lineupProbeStdout: &out.Stdout,
		lineupProbeStderr: &out.Stderr, lineupProbeStatus: &status,
	} {
		want[strings.TrimPrefix(p, "/")] = dst
	}
	forbidden := []string{
		strings.TrimPrefix(lineupProbeStageDir, "/"), strings.TrimPrefix(CodexHomeTarget, "/"),
	}
	seen := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return LineupProbeOutput{}, fmt.Errorf("read lineup probe export: %w", err)
		}
		if len(hdr.Name) > maxArchivePathBytes {
			return LineupProbeOutput{}, errors.New("lineup probe export entry path exceeds the length cap")
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		for _, f := range forbidden {
			if name == f || strings.HasPrefix(name, f+"/") {
				return LineupProbeOutput{}, errors.New("lineup probe export still holds the credential")
			}
		}
		dst, ok := want[name]
		if !ok {
			continue
		}
		if seen[name] {
			return LineupProbeOutput{}, fmt.Errorf("lineup probe export carries %s twice", name)
		}
		seen[name] = true
		if hdr.Typeflag != tar.TypeReg || hdr.Size > lineupProbeOutputCap {
			return LineupProbeOutput{}, fmt.Errorf("lineup probe output %s is not a regular file within the cap", name)
		}
		body, err := io.ReadAll(io.LimitReader(tr, lineupProbeOutputCap+1))
		if err != nil || len(body) > lineupProbeOutputCap {
			return LineupProbeOutput{}, fmt.Errorf("read lineup probe output %s", name)
		}
		*dst = body
	}
	code, err := strconv.Atoi(strings.TrimSuffix(string(status), "\n"))
	if err != nil || code < 0 || code > 255 {
		return LineupProbeOutput{}, errors.New("lineup probe recorded no exit status")
	}
	out.ExitStatus = code
	return out, nil
}

// teardown reaps whatever the run still owns, closing the proxy first so no
// connection outlives the network.
func (r *lineupProbeRun) teardown(ctx context.Context) error {
	var errs []error
	if r.container.attempted {
		errs = append(errs, r.ops.reapUnlistedContainer(ctx, r.name, r.container, r.owner))
	}
	if r.proxy != nil {
		errs = append(errs, r.proxy.Close())
	}
	if r.networkObj.attempted {
		errs = append(errs, r.ops.teardownNetwork(ctx, r.network, r.networkObj, r.owner))
	}
	return errors.Join(errs...)
}

// LineupProbeUsage is what the call reported spending. A nil field is one
// the CLI did not report.
type LineupProbeUsage struct {
	InputTokens  *int64  `json:"input_tokens"`
	OutputTokens *int64  `json:"output_tokens"`
	CostUSD      *string `json:"cost_usd"`
}

// LineupProbeVerdict is the judgment of one probe's output.
type LineupProbeVerdict struct {
	Passed bool
	// Refusal is the CLI's own account of a failed call, or a fixed
	// description when it gave none. Empty on a pass.
	Refusal string
	// CLIBuild is the first line of --version, as the CLI printed it.
	CLIBuild string
	Usage    LineupProbeUsage
}

// lineupProbeRefusalCap bounds the refusal text a verdict carries.
const lineupProbeRefusalCap = 1024

// JudgeLineupProbe judges one probe's output, from an image or from the
// judgment binary alike. A Claude call passes when it exits 0 with a result
// envelope that is not an error and names the requested model among the
// models that answered; a Codex call passes when it exits 0 and its last
// turn completed. Anything else is a refusal, whose text is the CLI's
// structured error message when it printed one and the tail of its stderr
// otherwise.
func JudgeLineupProbe(harness domain.HarnessClientKind, model string, out LineupProbeOutput) LineupProbeVerdict {
	verdict := LineupProbeVerdict{CLIBuild: firstLine(out.Version)}
	var passed bool
	var refusal string
	switch harness {
	case domain.HarnessClientClaudeCode:
		passed, refusal, verdict.Usage = judgeClaudeProbe(model, out)
	case domain.HarnessClientCodexCLI:
		passed, refusal, verdict.Usage = judgeCodexProbe(out)
	default:
		refusal = fmt.Sprintf("no lineup probe judgment for harness %q", harness)
	}
	if passed {
		verdict.Passed = true
		return verdict
	}
	if refusal == "" {
		refusal = lastLines(out.Stderr)
	}
	if refusal == "" {
		refusal = lastLines(out.Stdout)
	}
	if refusal == "" {
		refusal = fmt.Sprintf("the CLI exited %d with no error message", out.ExitStatus)
	}
	verdict.Refusal = truncateText(refusal, lineupProbeRefusalCap)
	return verdict
}

type claudeProbeEnvelope struct {
	Type         string       `json:"type"`
	IsError      *bool        `json:"is_error"`
	Result       string       `json:"result"`
	TotalCostUSD *json.Number `json:"total_cost_usd"`
	Usage        *struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	} `json:"usage"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

func judgeClaudeProbe(model string, out LineupProbeOutput) (bool, string, LineupProbeUsage) {
	var envelope claudeProbeEnvelope
	found := false
	for line := range bytes.SplitSeq(out.Stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || RejectDuplicateJSONKeys(line) != nil {
			continue
		}
		var candidate claudeProbeEnvelope
		if json.Unmarshal(line, &candidate) != nil || candidate.Type != "result" {
			continue
		}
		envelope, found = candidate, true
	}
	if !found {
		return false, "", LineupProbeUsage{}
	}
	var usage LineupProbeUsage
	if envelope.Usage != nil {
		usage.InputTokens, usage.OutputTokens = envelope.Usage.InputTokens, envelope.Usage.OutputTokens
	}
	if envelope.TotalCostUSD != nil {
		cost := envelope.TotalCostUSD.String()
		usage.CostUSD = &cost
	}
	if envelope.IsError == nil || *envelope.IsError {
		return false, envelope.Result, usage
	}
	if out.ExitStatus != 0 {
		return false, fmt.Sprintf("the CLI exited %d after a result that is not an error", out.ExitStatus), usage
	}
	if model == "" {
		return true, "", usage
	}
	answered := make([]string, 0, len(envelope.ModelUsage))
	for key := range envelope.ModelUsage {
		// The CLI may tag a model with its context variant, as in
		// "claude-opus-5-5[1m]".
		if key == model || strings.HasPrefix(key, model+"[") {
			return true, "", usage
		}
		answered = append(answered, key)
	}
	return false, fmt.Sprintf("the CLI answered with %q, not %q", answered, model), usage
}

func judgeCodexProbe(out LineupProbeOutput) (bool, string, LineupProbeUsage) {
	var usage LineupProbeUsage
	completed := false
	var errorMessage string
	for line := range bytes.SplitSeq(out.Stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		eventType, _, ok := codexTurnEvent(line)
		if !ok {
			continue
		}
		switch eventType {
		case "turn.started", "turn.failed":
			completed = false
		case "turn.completed":
			completed = true
			var event struct {
				Usage *struct {
					InputTokens  *int64 `json:"input_tokens"`
					OutputTokens *int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(line, &event) == nil && event.Usage != nil {
				usage.InputTokens, usage.OutputTokens = event.Usage.InputTokens, event.Usage.OutputTokens
			}
		case "error":
			// A stream error event carries its text at the top level; the
			// turn.failed that follows it repeats it under error.message.
			var event struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(line, &event) == nil && event.Message != "" {
				errorMessage = event.Message
			}
		}
	}
	if completed && out.ExitStatus == 0 {
		return true, "", usage
	}
	if message, failed := codexTerminalFailure(out.Stdout); failed && message != "" {
		return false, message, usage
	}
	return false, errorMessage, usage
}

func firstLine(b []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(b), []byte("\n"))
	return truncateText(strings.TrimSpace(string(line)), 256)
}

// lastLines is the tail of a stream, up to the refusal cap, starting on a
// rune boundary.
func lastLines(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > lineupProbeRefusalCap {
		b = b[len(b)-lineupProbeRefusalCap:]
		for len(b) > 0 && !utf8.RuneStart(b[0]) {
			b = b[1:]
		}
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// truncateText cuts s to at most limit bytes on a rune boundary.
func truncateText(s string, limit int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
