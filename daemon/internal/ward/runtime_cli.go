package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"sort"
	"strings"
)

// CLIRuntime is the Runtime over the Apple container CLI (the reference
// runtime, release 1.1.0). It is the package's only os/exec user. It runs
// the CLI on the host — the control plane stays outside every VM (check 2's
// structural half) — and its decoding maps unknown mount kinds and states
// through verbatim, so downstream verification rejects what it does not
// know instead of never seeing it.
type CLIRuntime struct {
	bin string
}

// NewCLIRuntime returns a CLIRuntime invoking binPath (usually the absolute
// path to the container CLI).
func NewCLIRuntime(binPath string) *CLIRuntime { return &CLIRuntime{bin: binPath} }

var _ Runtime = (*CLIRuntime)(nil)

// run executes one CLI invocation and returns its stdout, folding a bounded
// stderr tail into the error for diagnosis.
func (c *CLIRuntime) run(ctx context.Context, args ...string) ([]byte, error) {
	return c.runCommand(ctx, true, args...)
}

// runRedactedStderr preserves the command's safe exit error but suppresses
// stderr, which may echo a create argument containing an explicit environment
// credential. Other runtime calls carry only gate-generated identifiers and
// use run so their bounded stderr remains available for diagnosis.
func (c *CLIRuntime) runRedactedStderr(ctx context.Context, args ...string) ([]byte, error) {
	return c.runCommand(ctx, false, args...)
}

func (c *CLIRuntime) runCommand(ctx context.Context, reportStderr bool, args ...string) ([]byte, error) {
	cmd := osexec.CommandContext(ctx, c.bin, args...) //nolint:gosec // bin is operator-configured; caller fields reach argv only through the fail-closed spec phrasing above
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if !reportStderr {
			return nil, fmt.Errorf("container %s: %w", args[0], err)
		}
		msg := strings.TrimSpace(stderr.String())
		const maxStderr = 512
		if len(msg) > maxStderr {
			msg = msg[:maxStderr] + "..."
		}
		return nil, fmt.Errorf("container %s: %w: %s", args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

func (c *CLIRuntime) CreateVolume(ctx context.Context, name string, sizeMB int64, labels []Label) error {
	args := []string{"volume", "create", "-s", fmt.Sprintf("%dM", sizeMB)}
	for _, l := range labels {
		args = append(args, "--label", l.Key+"="+l.Value)
	}
	args = append(args, name)
	_, err := c.run(ctx, args...)
	return err
}

func (c *CLIRuntime) DeleteVolume(ctx context.Context, name string) error {
	_, err := c.run(ctx, "volume", "delete", name)
	return err
}

func (c *CLIRuntime) ListVolumes(ctx context.Context) ([]VolumeSummary, error) {
	out, err := c.run(ctx, "volume", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeVolumeList(out)
}

func (c *CLIRuntime) CreateContainer(ctx context.Context, spec ContainerSpec) error {
	args, err := createContainerArgs(spec)
	if err != nil {
		return err
	}
	_, err = c.runRedactedStderr(ctx, args...)
	return err
}

// createContainerArgs phrases a spec as CLI arguments. It refuses any
// non-volume mount: the gate only ever generates volume mounts, and the
// runtime is never even asked for anything else.
func createContainerArgs(spec ContainerSpec) ([]string, error) {
	args := []string{"create", "--name", spec.Name}
	for _, l := range spec.Labels {
		args = append(args, "--label", l.Key+"="+l.Value)
	}
	for _, m := range spec.Mounts {
		if m.Type != MountVolume {
			return nil, fmt.Errorf("refusing to create container %q with %s mount at %q",
				spec.Name, m.Type, m.Target)
		}
		// A comma or control character in a field would let the CLI parse an
		// injected mount option; refuse to phrase it rather than escape it.
		if !cliSafe(m.Source) || !cliSafe(m.Target) {
			return nil, fmt.Errorf("refusing to create container %q: mount field for target %q carries a CLI delimiter",
				spec.Name, m.Target)
		}
		mount := fmt.Sprintf("type=volume,source=%s,target=%s", m.Source, m.Target)
		if m.ReadOnly {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}
	for _, e := range spec.Env {
		// A bare `--env key` makes the CLI inherit the host value; require an
		// explicit key=value so no host credential is pulled into the VM.
		if envInherits(e) {
			return nil, fmt.Errorf("refusing to create container %q with host-inheriting env entry",
				spec.Name)
		}
		args = append(args, "--env", e)
	}
	// Terminate option parsing before the positional image and command: a
	// dash-prefixed image (e.g. "--mount") or command word would otherwise be
	// reparsed as a create option, realizing a mount/SSH topology outside the
	// validated spec. Everything after "--" is positional. The other CLI
	// methods take only gate-generated identifiers (validated RunID-derived
	// names, internal temp paths), so this is the one argv site fed
	// caller-supplied positionals.
	args = append(args, "--", spec.Image)
	args = append(args, spec.Command...)
	return args, nil
}

func (c *CLIRuntime) StartContainer(ctx context.Context, id string) error {
	_, err := c.run(ctx, "start", id)
	return err
}

func (c *CLIRuntime) StopContainer(ctx context.Context, id string) error {
	_, err := c.run(ctx, "stop", id)
	return err
}

func (c *CLIRuntime) Inspect(ctx context.Context, id string) (InspectReport, error) {
	out, err := c.run(ctx, "inspect", id)
	if err != nil {
		return InspectReport{}, err
	}
	return decodeInspect(out, id)
}

func (c *CLIRuntime) DeleteContainer(ctx context.Context, id string) error {
	_, err := c.run(ctx, "delete", id)
	return err
}

func (c *CLIRuntime) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	out, err := c.run(ctx, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeContainerList(out)
}

func (c *CLIRuntime) ExportRootFS(ctx context.Context, id, destPath string) error {
	_, err := c.run(ctx, "export", "--output", destPath, id)
	return err
}

// The 1.1.0 JSON shapes, pinned by the decode fixtures under testdata/. Only
// the fields the gate reads are decoded; everything else is ignored by
// construction and re-verified by the checks that consume these reports.

type cliVolumeType struct {
	Name string `json:"name"`
}

type cliMount struct {
	Destination string                     `json:"destination"`
	Options     []string                   `json:"options"`
	Source      string                     `json:"source"`
	Type        map[string]json.RawMessage `json:"type"`
}

type cliInitProcess struct {
	// Pointer so an absent environment key is distinguishable from an
	// explicitly empty one: the allowlist must observe the field, not
	// assume clean when the CLI shape drifts.
	Environment *[]string `json:"environment"`
}

type cliConfiguration struct {
	ID          string            `json:"id"`
	Labels      map[string]string `json:"labels"`
	Mounts      []cliMount        `json:"mounts"`
	InitProcess cliInitProcess    `json:"initProcess"`
	// Pointers for the same reason as Environment: check 4's allowlist inputs
	// must be observed, so an absent field fails closed rather than reading
	// as "no SSH / no publications".
	SSH              *bool              `json:"ssh"`
	PublishedPorts   *[]json.RawMessage `json:"publishedPorts"`
	PublishedSockets *[]json.RawMessage `json:"publishedSockets"`
}

type cliStatus struct {
	State string `json:"state"`
}

type cliContainer struct {
	Configuration cliConfiguration `json:"configuration"`
	ID            string           `json:"id"`
	Status        cliStatus        `json:"status"`
}

type cliVolume struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type cliVolumeEntry struct {
	Configuration cliVolume `json:"configuration"`
	ID            string    `json:"id"`
}

// toMount maps one CLI mount to the seam vocabulary. The type field is a
// single-key object; "volume" carries the volume name, "virtiofs" is a host
// bind. Anything else — including a malformed multi-key object — maps to an
// invalid MountType verbatim, so verification fails closed on it.
func (m cliMount) toMount() Mount {
	out := Mount{Target: m.Destination}
	for _, o := range m.Options {
		if o == "ro" {
			out.ReadOnly = true
		}
	}
	keys := make([]string, 0, len(m.Type))
	for k := range m.Type {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch {
	case len(keys) == 1 && keys[0] == "volume":
		var v cliVolumeType
		if err := json.Unmarshal(m.Type["volume"], &v); err == nil && v.Name != "" {
			out.Type = MountVolume
			out.Source = v.Name
			return out
		}
		out.Type = MountType("volume/unnamed")
	case len(keys) == 1 && keys[0] == "virtiofs":
		out.Type = MountBind
		out.Source = m.Source
	default:
		out.Type = MountType(strings.Join(keys, "+"))
	}
	return out
}

// allowlistFieldsPresent reports whether every security-relevant inspect
// field check 4 reads was actually present in the JSON. A drifted or partial
// report (renamed/omitted keys) must not read as an explicit clean report.
func (c cliContainer) allowlistFieldsPresent() bool {
	return c.Configuration.InitProcess.Environment != nil &&
		c.Configuration.SSH != nil &&
		c.Configuration.PublishedPorts != nil &&
		c.Configuration.PublishedSockets != nil
}

func (c cliContainer) toReport() InspectReport {
	rep := InspectReport{
		State: ContainerState(c.Status.State),
		Env:   *c.Configuration.InitProcess.Environment,
		SSH:   *c.Configuration.SSH,
	}
	for _, m := range c.Configuration.Mounts {
		rep.Mounts = append(rep.Mounts, m.toMount())
	}
	for _, p := range *c.Configuration.PublishedSockets {
		rep.PublishedSockets = append(rep.PublishedSockets, string(p))
	}
	for _, p := range *c.Configuration.PublishedPorts {
		rep.PublishedPorts = append(rep.PublishedPorts, string(p))
	}
	return rep
}

// decodeInspect decodes `container inspect` output: an array with exactly
// one element for a single-ID query.
func decodeInspect(out []byte, id string) (InspectReport, error) {
	var ctrs []cliContainer
	if err := json.Unmarshal(out, &ctrs); err != nil {
		return InspectReport{}, fmt.Errorf("decode inspect output for %q: %w", id, err)
	}
	if len(ctrs) != 1 {
		return InspectReport{}, fmt.Errorf("inspect %q returned %d containers, want 1", id, len(ctrs))
	}
	// The report is only trustworthy if it is for the requested container: a
	// missing or mismatched id would let checks 3 and 4 act on the wrong (or
	// unidentified) object's state and mounts while the requested name is
	// started/deleted/exported. Fail closed, matching the list decoders.
	if ctrs[0].ID != id {
		return InspectReport{}, fmt.Errorf("inspect %q returned a report identified as %q", id, ctrs[0].ID)
	}
	// The mounts, environment, SSH, and publications check 4 reads all live
	// under configuration, so its identity must match too: a report cannot
	// carry the requested top-level id but another object's configuration.
	if ctrs[0].Configuration.ID != id {
		return InspectReport{}, fmt.Errorf("inspect %q returned a configuration identified as %q", id, ctrs[0].Configuration.ID)
	}
	// Every allowlist input check 4 reads must have been present: an absent
	// env/ssh/publications field would otherwise decode to a clean-looking
	// zero value and let the gate approve without observing it. The other
	// security-relevant fields already fail closed on absence: an empty state
	// is not "stopped", zero mounts fails the "exactly one" rule, and an
	// unnamed/typeless mount decodes to an invalid type.
	if !ctrs[0].allowlistFieldsPresent() {
		return InspectReport{}, fmt.Errorf("inspect %q omitted a required allowlist field (environment/ssh/publishedPorts/publishedSockets)", id)
	}
	return ctrs[0].toReport(), nil
}

func decodeContainerList(out []byte) ([]ContainerSummary, error) {
	var ctrs []cliContainer
	if err := json.Unmarshal(out, &ctrs); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	summaries := make([]ContainerSummary, len(ctrs))
	for i, c := range ctrs {
		// An entry with no id cannot be matched against the run's names, so
		// the absence proofs (verifyContainerAbsent, teardown) would silently
		// treat an unidentified survivor as absent; fail closed instead. The
		// configuration id must agree, so a report cannot present one identity
		// at the top level and another in the body.
		if c.ID == "" {
			return nil, fmt.Errorf("container list entry %d has no id", i)
		}
		if c.Configuration.ID != c.ID {
			return nil, fmt.Errorf("container list entry %d id %q disagrees with configuration id %q", i, c.ID, c.Configuration.ID)
		}
		labels := make([]Label, 0, len(c.Configuration.Labels))
		for k, val := range c.Configuration.Labels {
			labels = append(labels, Label{Key: k, Value: val})
		}
		sort.Slice(labels, func(a, b int) bool { return labels[a].Key < labels[b].Key })
		summaries[i] = ContainerSummary{ID: c.ID, State: ContainerState(c.Status.State), Labels: labels}
	}
	return summaries, nil
}

func decodeVolumeList(out []byte) ([]VolumeSummary, error) {
	var vols []cliVolumeEntry
	if err := json.Unmarshal(out, &vols); err != nil {
		return nil, fmt.Errorf("decode volume list: %w", err)
	}
	summaries := make([]VolumeSummary, len(vols))
	for i, v := range vols {
		// An unnamed volume cannot be matched in the teardown sweep, so a
		// nameless survivor would pass as reaped; fail closed instead. The
		// top-level id must agree with the name, so a survivor cannot keep an
		// id that names the workspace while its configuration name drifts out
		// of the teardown match.
		if v.Configuration.Name == "" {
			return nil, fmt.Errorf("volume list entry %d has no name", i)
		}
		if v.ID != v.Configuration.Name {
			return nil, fmt.Errorf("volume list entry %d id %q disagrees with configuration name %q", i, v.ID, v.Configuration.Name)
		}
		labels := make([]Label, 0, len(v.Configuration.Labels))
		for k, val := range v.Configuration.Labels {
			labels = append(labels, Label{Key: k, Value: val})
		}
		sort.Slice(labels, func(a, b int) bool { return labels[a].Key < labels[b].Key })
		summaries[i] = VolumeSummary{Name: v.Configuration.Name, Labels: labels}
	}
	return summaries, nil
}
