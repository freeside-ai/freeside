package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	osexec "os/exec"
	"sort"
	"strings"
	"unicode"
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

// maxRuntimeOutput bounds the stdout buffered from a JSON-producing runtime
// call. Honest inspect/list output is far smaller; the cap stops a wedged or
// hostile runtime returning an unbounded stream from exhausting daemon memory
// before decoding — the stdout analogue of the stderr and archive bounds.
const maxRuntimeOutput = 16 << 20

// run executes a JSON-producing CLI call (list/inspect) and returns its stdout,
// capped so a wedged runtime cannot exhaust memory. Stderr is withheld from the
// error: unlike the mutation calls on the gate's own deterministic names, these
// enumerate or inspect objects beyond the gate's control, so a runtime/XPC
// message could name a user-created object, and this error is wrapped into
// ConformanceFailure.Reason, which must carry no runtime-observed identity. It
// fails closed if the command produced more than the cap rather than decoding a
// truncated stream.
func (c *CLIRuntime) run(ctx context.Context, args ...string) ([]byte, error) {
	stdout := &capWriter{max: maxRuntimeOutput}
	if err := c.runTo(ctx, stdout, false, args...); err != nil {
		return nil, err
	}
	if stdout.truncated {
		return nil, fmt.Errorf("container %s: stdout exceeded the %d-byte cap", args[0], maxRuntimeOutput)
	}
	return stdout.buf.Bytes(), nil
}

// runDiscard executes a CLI call whose stdout is ignored, draining it to
// io.Discard rather than buffering output that is thrown away. reportStderr
// keeps the bounded stderr tail for diagnosis only for mutations on the gate's
// own deterministic names (volume/container create-with-name is the exception:
// its stderr may echo an argument carrying an explicit environment credential).
// Calls that touch objects beyond those names (list/inspect/export) withhold
// stderr entirely, since a runtime message could name a foreign object and the
// error reaches ConformanceFailure.Reason.
func (c *CLIRuntime) runDiscard(ctx context.Context, reportStderr bool, args ...string) error {
	return c.runTo(ctx, io.Discard, reportStderr, args...)
}

func (c *CLIRuntime) runTo(ctx context.Context, stdout io.Writer, reportStderr bool, args ...string) error {
	cmd := osexec.CommandContext(ctx, c.bin, args...) //nolint:gosec // bin is operator-configured; caller fields reach argv only through the fail-closed spec phrasing above
	return runPrepared(cmd, stdout, reportStderr, args[0])
}

// capWriter buffers at most max bytes and drops the rest, so a noisy or wedged
// runtime (or its XPC service) cannot grow the captured stderr without bound
// before the call fails closed.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) <= room {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
			w.truncated = true
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	// Always report full consumption so os/exec's copier never blocks the child
	// on a short write once the cap is reached.
	return len(p), nil
}

func runPrepared(cmd *osexec.Cmd, stdout io.Writer, reportStderr bool, operation string) error {
	cmd.Stdout = stdout
	if !reportStderr {
		// A redacted call never reports stderr (it may echo a caller-supplied
		// credential), so drain it rather than buffering bytes that are thrown
		// away — an unbounded buffer would be a memory-exhaustion vector.
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("container %s: %w", operation, err)
		}
		return nil
	}
	const maxStderr = 512
	stderr := &capWriter{max: maxStderr}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.buf.String())
		if stderr.truncated {
			msg += "..."
		}
		return fmt.Errorf("container %s: %w: %s", operation, err, msg)
	}
	return nil
}

func (c *CLIRuntime) CreateVolume(ctx context.Context, name string, sizeMB int64, labels []Label) error {
	args := []string{"volume", "create", "-s", fmt.Sprintf("%dM", sizeMB)}
	for _, l := range labels {
		args = append(args, "--label", l.Key+"="+l.Value)
	}
	args = append(args, name)
	return c.runDiscard(ctx, true, args...)
}

func (c *CLIRuntime) DeleteVolume(ctx context.Context, name string) error {
	return c.runDiscard(ctx, true, "volume", "delete", name)
}

func (c *CLIRuntime) ListVolumes(ctx context.Context) ([]VolumeSummary, error) {
	out, err := c.run(ctx, "volume", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeVolumeList(out)
}

func (c *CLIRuntime) InspectVolume(ctx context.Context, name string) (VolumeSummary, error) {
	out, err := c.run(ctx, "volume", "inspect", name)
	if err != nil {
		return VolumeSummary{}, err
	}
	return decodeVolumeInspect(out, name)
}

func (c *CLIRuntime) CreateContainer(ctx context.Context, spec ContainerSpec) error {
	args, err := createContainerArgs(spec)
	if err != nil {
		return err
	}
	return c.runDiscard(ctx, false, args...)
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
	if spec.NetworkDisabled {
		// Apple container 1.1.0's public no-network sentinel produces an empty
		// configuration.networks attachment set. The pre-start inspect verifies
		// that realized state before any exporter payload executes.
		args = append(args, "--network", "none")
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
	return c.runDiscard(ctx, true, "start", id)
}

func (c *CLIRuntime) StopContainer(ctx context.Context, id string) error {
	return c.runDiscard(ctx, true, "stop", id)
}

func (c *CLIRuntime) Inspect(ctx context.Context, id string) (InspectReport, error) {
	out, err := c.run(ctx, "inspect", id)
	if err != nil {
		return InspectReport{}, err
	}
	return decodeInspect(out, id)
}

func (c *CLIRuntime) DeleteContainer(ctx context.Context, id string) error {
	return c.runDiscard(ctx, true, "delete", id)
}

func (c *CLIRuntime) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	out, err := c.run(ctx, "list", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeContainerList(out)
}

func (c *CLIRuntime) ExportRootFS(ctx context.Context, id string, dest io.Writer, _ int64) error {
	// Omit --output so the CLI copies its completed archive to stdout, where
	// the caller-owned Writer enforces the gate's exact byte cap. Apple
	// container 1.1.0 first asks its already-running XPC service to materialize
	// a private temp archive; a resource limit on this CLI child would not
	// constrain that separate writer and must not be represented as doing so.
	// Stderr is withheld (false): a failed export can reference a foreign object
	// recycled into the name, and this error reaches ConformanceFailure.Reason.
	return c.runTo(ctx, dest, false, "export", id)
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
	Executable       string    `json:"executable"`
	Arguments        *[]string `json:"arguments"`
	WorkingDirectory *string   `json:"workingDirectory"`
	// Pointer so an absent environment key is distinguishable from an
	// explicitly empty one: the allowlist must observe the field, not
	// assume clean when the CLI shape drifts.
	Environment *[]string `json:"environment"`
}

// cliImage decodes configuration.image. Only reference is consumed: it carries
// the pinned @sha256 digest trust binds to. The sibling descriptor.digest is
// the runtime's resolved (arch-dependent) digest, unpredictable from the spec,
// so it is intentionally not decoded and its presence in the JSON is ignored.
type cliImage struct {
	Reference string `json:"reference"`
}

type cliConfiguration struct {
	ID           string             `json:"id"`
	CreationDate string             `json:"creationDate"`
	Image        *cliImage          `json:"image"`
	Labels       *map[string]string `json:"labels"`
	Mounts       []cliMount         `json:"mounts"`
	InitProcess  cliInitProcess     `json:"initProcess"`
	// Pointers for the same reason as Environment: check 4's allowlist inputs
	// must be observed, so an absent field fails closed rather than reading
	// as "no SSH / no publications".
	SSH              *bool              `json:"ssh"`
	PublishedPorts   *[]json.RawMessage `json:"publishedPorts"`
	PublishedSockets *[]json.RawMessage `json:"publishedSockets"`
	Networks         *[]json.RawMessage `json:"networks"`
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
	Name         string             `json:"name"`
	CreationDate string             `json:"creationDate"`
	Labels       *map[string]string `json:"labels"`
}

type cliVolumeEntry struct {
	Configuration cliVolume `json:"configuration"`
	ID            string    `json:"id"`
}

// rejectDuplicateJSONKeys rejects ambiguous runtime evidence before the
// typed decoders apply encoding/json's last-value-wins behavior. The CLI
// output is already byte-capped by run, so this structural pass is also
// resource-bounded.
func rejectDuplicateJSONKeys(out []byte) error {
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := checkJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("runtime JSON contains more than one top-level value")
		}
		return err
	}
	return nil
}

func checkJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("runtime JSON object key is not a string")
			}
			folded := foldJSONKey(key)
			if _, duplicate := seen[folded]; duplicate {
				return errors.New("runtime JSON object contains a duplicate key")
			}
			seen[folded] = struct{}{}
			if err := checkJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("runtime JSON object is not terminated")
		}
	case '[':
		for dec.More() {
			if err := checkJSONValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("runtime JSON array is not terminated")
		}
	default:
		return errors.New("runtime JSON contains an unexpected delimiter")
	}
	return nil
}

// foldJSONKey mirrors encoding/json's case-insensitive struct-field matching:
// every rune is reduced to the smallest member of its Unicode simple-fold
// set. Keys that would target the same Go field therefore collide here even
// when their spelling or case differs.
func foldJSONKey(key string) string {
	var out strings.Builder
	out.Grow(len(key))
	for _, r := range key {
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				r = next
				break
			}
			r = next
		}
		out.WriteRune(r)
	}
	return out.String()
}

// toMount maps one CLI mount to the seam vocabulary. The type field is a
// single-key object; "volume" carries the volume name, "virtiofs" is a host
// bind. Anything else — including a malformed multi-key object — maps to an
// invalid MountType verbatim, so verification fails closed on it.
func (m cliMount) toMount() Mount {
	out := Mount{Target: m.Destination}
	var sawRO, sawRW bool
	for _, o := range m.Options {
		switch o {
		case "ro":
			sawRO = true
		case "rw":
			sawRW = true
		}
	}
	// A mount reporting both ro and rw proves neither access, so ro's mere
	// presence must not read as read-only. Record the conflict and let the
	// allowlist checks fail closed on it (sameMounts for the writer, an explicit
	// case for the exporter).
	out.ReadOnly = sawRO && !sawRW
	out.AccessConflict = sawRO && sawRW
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

// allowlistFieldsPresent reports whether every security-relevant pre-start
// inspect field was actually present in the JSON. A drifted or partial report
// (renamed/omitted keys) must not read as an explicit clean report.
func (c cliContainer) allowlistFieldsPresent() bool {
	return c.Configuration.Image != nil &&
		c.Configuration.Image.Reference != "" &&
		c.Configuration.InitProcess.Executable != "" &&
		c.Configuration.InitProcess.Arguments != nil &&
		c.Configuration.InitProcess.WorkingDirectory != nil &&
		c.Configuration.InitProcess.Environment != nil &&
		c.Configuration.SSH != nil &&
		c.Configuration.PublishedPorts != nil &&
		c.Configuration.PublishedSockets != nil &&
		c.Configuration.Networks != nil
}

func (c cliContainer) toReport() InspectReport {
	rep := InspectReport{
		ID:                      c.ID,
		State:                   ContainerState(c.Status.State),
		CreationDate:            c.Configuration.CreationDate,
		AllowlistFieldsObserved: c.allowlistFieldsPresent(),
		LabelsObserved:          c.Configuration.Labels != nil,
		NetworksObserved:        c.Configuration.Networks != nil,
	}
	if c.Configuration.Image != nil {
		rep.ImageReference = c.Configuration.Image.Reference
	}
	if c.Configuration.InitProcess.Executable != "" {
		rep.Command = append(rep.Command, c.Configuration.InitProcess.Executable)
	}
	if c.Configuration.InitProcess.Arguments != nil {
		rep.Command = append(rep.Command, (*c.Configuration.InitProcess.Arguments)...)
	}
	if c.Configuration.InitProcess.WorkingDirectory != nil {
		rep.WorkingDirectory = *c.Configuration.InitProcess.WorkingDirectory
	}
	if c.Configuration.InitProcess.Environment != nil {
		rep.Env = append(rep.Env, (*c.Configuration.InitProcess.Environment)...)
	}
	if c.Configuration.SSH != nil {
		rep.SSH = *c.Configuration.SSH
	}
	if c.Configuration.Labels != nil {
		for k, value := range *c.Configuration.Labels {
			rep.Labels = append(rep.Labels, Label{Key: k, Value: value})
		}
		sort.Slice(rep.Labels, func(i, j int) bool { return rep.Labels[i].Key < rep.Labels[j].Key })
	}
	for _, m := range c.Configuration.Mounts {
		rep.Mounts = append(rep.Mounts, m.toMount())
	}
	if c.Configuration.PublishedSockets != nil {
		for _, p := range *c.Configuration.PublishedSockets {
			rep.PublishedSockets = append(rep.PublishedSockets, string(p))
		}
	}
	if c.Configuration.PublishedPorts != nil {
		for _, p := range *c.Configuration.PublishedPorts {
			rep.PublishedPorts = append(rep.PublishedPorts, string(p))
		}
	}
	if c.Configuration.Networks != nil {
		rep.NetworkAttachmentCount = len(*c.Configuration.Networks)
	}
	return rep
}

// decodeInspect decodes `container inspect` output: an array with exactly
// one element for a single-ID query.
func decodeInspect(out []byte, id string) (InspectReport, error) {
	if err := rejectDuplicateJSONKeys(out); err != nil {
		return InspectReport{}, fmt.Errorf("decode inspect output for %q: invalid JSON structure", id)
	}
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
		// The observed id is untrusted CLI output that could carry a copied
		// credential in a foreign object's name; keep the reason categorical
		// so it cannot reach ConformanceFailure.Reason as credential material.
		return InspectReport{}, fmt.Errorf("inspect %q returned a report with a mismatched identity", id)
	}
	// The image, command, mounts, environment, SSH, and publications check 4 reads all live
	// under configuration, so its identity must match too: a report cannot
	// carry the requested top-level id but another object's configuration.
	if ctrs[0].Configuration.ID != id {
		return InspectReport{}, fmt.Errorf("inspect %q returned a configuration with a mismatched identity", id)
	}
	// Preserve allowlist-field presence in the report instead of rejecting it
	// here. Check 4 requires the complete exporter shape; teardown ownership
	// recovery needs only the independently verified identity and labels and
	// must still work for a partially created container.
	return ctrs[0].toReport(), nil
}

func decodeContainerList(out []byte) ([]ContainerSummary, error) {
	if err := rejectDuplicateJSONKeys(out); err != nil {
		return nil, errors.New("decode container list: invalid JSON structure")
	}
	var ctrs []cliContainer
	if err := json.Unmarshal(out, &ctrs); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	if ctrs == nil {
		return nil, errors.New("decode container list: output is null, want an array")
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
			// Both ids are untrusted CLI output; report the entry position, not
			// the observed names, so a copied credential in a foreign row's
			// identity cannot reach ConformanceFailure.Reason.
			return nil, fmt.Errorf("container list entry %d id disagrees with its configuration id", i)
		}
		labels := []Label(nil)
		if c.Configuration.Labels != nil {
			labels = make([]Label, 0, len(*c.Configuration.Labels))
			for k, val := range *c.Configuration.Labels {
				labels = append(labels, Label{Key: k, Value: val})
			}
		}
		sort.Slice(labels, func(a, b int) bool { return labels[a].Key < labels[b].Key })
		summaries[i] = ContainerSummary{
			ID: c.ID, State: ContainerState(c.Status.State), Labels: labels,
			LabelsObserved: c.Configuration.Labels != nil,
			CreationDate:   c.Configuration.CreationDate,
		}
	}
	return summaries, nil
}

// decodeVolumeInspect decodes `container volume inspect` output: an array
// with exactly one element for a single-name query, in the same entry shape
// as the volume listing. The identity rules mirror decodeInspect: the report
// is evidence only when both its top-level id and configuration name equal
// the requested name, so ownership recovery cannot act on another volume's
// labels or fingerprint.
func decodeVolumeInspect(out []byte, name string) (VolumeSummary, error) {
	if err := rejectDuplicateJSONKeys(out); err != nil {
		return VolumeSummary{}, fmt.Errorf("decode volume inspect output for %q: invalid JSON structure", name)
	}
	var vols []cliVolumeEntry
	if err := json.Unmarshal(out, &vols); err != nil {
		return VolumeSummary{}, fmt.Errorf("decode volume inspect output for %q: %w", name, err)
	}
	if len(vols) != 1 {
		return VolumeSummary{}, fmt.Errorf("volume inspect %q returned %d volumes, want 1", name, len(vols))
	}
	if vols[0].ID != name {
		// The observed id is untrusted CLI output that could carry a copied
		// credential in a foreign object's name; keep the reason categorical
		// so it cannot reach ConformanceFailure.Reason as credential material.
		return VolumeSummary{}, fmt.Errorf("volume inspect %q returned a report with a mismatched identity", name)
	}
	if vols[0].Configuration.Name != name {
		return VolumeSummary{}, fmt.Errorf("volume inspect %q returned a configuration with a mismatched name", name)
	}
	v := vols[0]
	labels := []Label(nil)
	if v.Configuration.Labels != nil {
		labels = make([]Label, 0, len(*v.Configuration.Labels))
		for k, val := range *v.Configuration.Labels {
			labels = append(labels, Label{Key: k, Value: val})
		}
	}
	sort.Slice(labels, func(a, b int) bool { return labels[a].Key < labels[b].Key })
	return VolumeSummary{
		Name: v.Configuration.Name, Labels: labels,
		LabelsObserved: v.Configuration.Labels != nil,
		CreationDate:   v.Configuration.CreationDate,
	}, nil
}

func decodeVolumeList(out []byte) ([]VolumeSummary, error) {
	if err := rejectDuplicateJSONKeys(out); err != nil {
		return nil, errors.New("decode volume list: invalid JSON structure")
	}
	var vols []cliVolumeEntry
	if err := json.Unmarshal(out, &vols); err != nil {
		return nil, fmt.Errorf("decode volume list: %w", err)
	}
	if vols == nil {
		return nil, errors.New("decode volume list: output is null, want an array")
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
			return nil, fmt.Errorf("volume list entry %d id disagrees with its configuration name", i)
		}
		labels := []Label(nil)
		if v.Configuration.Labels != nil {
			labels = make([]Label, 0, len(*v.Configuration.Labels))
			for k, val := range *v.Configuration.Labels {
				labels = append(labels, Label{Key: k, Value: val})
			}
		}
		sort.Slice(labels, func(a, b int) bool { return labels[a].Key < labels[b].Key })
		summaries[i] = VolumeSummary{
			Name: v.Configuration.Name, Labels: labels,
			LabelsObserved: v.Configuration.Labels != nil,
			CreationDate:   v.Configuration.CreationDate,
		}
	}
	return summaries, nil
}
