package ward

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubRuntime is the inert Runtime for tests that never drive the
// lifecycle; handoff tests use the scripted fakeRuntime instead.
type stubRuntime struct{}

var _ Runtime = stubRuntime{}

func (stubRuntime) CreateNetwork(context.Context, string, []Label) error { return nil }
func (stubRuntime) DeleteNetwork(context.Context, string) error          { return nil }
func (stubRuntime) ListNetworks(context.Context) ([]NetworkSummary, error) {
	return nil, nil
}

func (stubRuntime) InspectNetwork(context.Context, string) (NetworkReport, error) {
	return NetworkReport{}, nil
}
func (stubRuntime) CreateVolume(context.Context, string, int64, []Label) error { return nil }
func (stubRuntime) DeleteVolume(context.Context, string) error                 { return nil }
func (stubRuntime) ListVolumes(context.Context) ([]VolumeSummary, error)       { return nil, nil }
func (stubRuntime) InspectVolume(context.Context, string) (VolumeSummary, error) {
	return VolumeSummary{}, nil
}
func (stubRuntime) CreateContainer(context.Context, ContainerSpec) error { return nil }
func (stubRuntime) StartContainer(context.Context, string) error         { return nil }
func (stubRuntime) StopContainer(context.Context, string) error          { return nil }
func (stubRuntime) Inspect(context.Context, string) (InspectReport, error) {
	return InspectReport{}, nil
}
func (stubRuntime) DeleteContainer(context.Context, string) error              { return nil }
func (stubRuntime) ListContainers(context.Context) ([]ContainerSummary, error) { return nil, nil }
func (stubRuntime) ExportRootFS(context.Context, string, io.Writer, int64) error {
	return nil
}
func (stubRuntime) CopyIntoContainer(context.Context, string, string, string) error { return nil }

// fakeCtr is one container the fakeRuntime tracks.
type fakeCtr struct {
	spec     ContainerSpec
	started  bool
	stopped  bool
	inspects int // inspects observed since start
	// created is the opaque creation fingerprint the fake reports; a
	// replacement gets a fresh value, like the real runtime's creationDate.
	created string
}

// fakeVol is one volume the fakeRuntime tracks.
type fakeVol struct {
	labels  []Label
	created string
}

type fakeNetwork struct {
	labels  []Label
	created string
}

// fakeRuntime is the scripted Runtime driving the lifecycle tests: default
// behavior models Apple container 1.1.0 (a created-but-never-started
// container reports stopped; a started one reports running for
// runningInspects polls, then stopped), records every call in order, and
// per-method override hooks induce each conformance violation.
type fakeRuntime struct {
	t  *testing.T
	mu sync.Mutex

	calls  []string
	nets   map[string]*fakeNetwork
	vols   map[string]*fakeVol
	ctrs   map[string]*fakeCtr
	copies []fakeCopy
	// volBase is the base SHA a volume holds, as the simulated seeder placed
	// it. Only the sentinel copy sets it, mirroring the real seeder: the tree
	// reaches the volume when the guest's own command runs, not when the host's
	// copy returns.
	volBase map[string]string
	// volTree is the tree digest the simulated seeder placed alongside it.
	volTree map[string]string
	// snapshotFiles holds the files the Codex-review snapshot seeder placed on a
	// volume, keyed by volume then basename, so the snapshot observer can report
	// their sha256 digests.
	snapshotFiles map[string]map[string][]byte
	// instructionState holds the exact CLAUDE.md bytes the instruction seeder
	// placed in each volume. A present map entry with nil content represents
	// the admitted empty overlay.
	instructionState map[string][]byte
	// stateManifest records config roots the state seeder prepared. Volumes
	// absent from this map remain freshly empty.
	stateManifest map[string]stateManifestKind
	// staged is the host directory most recently staged into each container.
	staged map[string]string
	// baseProofPath is where the observer's proof lands in its rootfs; it
	// mirrors Config.BaseProofPath, which the fake cannot see.
	baseProofPath string
	// credProofPath mirrors Config.CredProofPath the same way; a container
	// whose command writes it is a credential-store observer.
	credProofPath string
	// writerOutcomeProofPath mirrors the credential-free marker observer's
	// rootfs proof path.
	writerOutcomeProofPath string
	// writerStatus is the launcher status synthesized for the workspace
	// marker. Zero is the successful default.
	writerStatus int
	// credState is each credential volume's simulated store content; the
	// synthesized credential proof digests it, so a test mutates the store by
	// changing the state string (e.g. from an agent onStart hook).
	credState map[string]string
	// observerProof rewrites the synthesized proof bytes before they are
	// archived, so a test can corrupt, truncate, or replay one.
	observerProof func(id string, proof []byte) []byte
	// copySeedsNothing models the reference runtime's most dangerous
	// behaviour: a copy whose destination lies inside a mounted volume writes
	// nothing and still reports success. The call is recorded and returns nil;
	// no volume changes.
	copySeedsNothing bool
	// seq feeds nextCreated so every object the fake makes carries a distinct
	// opaque creation fingerprint.
	seq int

	// runningInspects is how many post-start Inspects report running before
	// stopped, per container name; unset means 1.
	runningInspects map[string]int
	// exportTarPath is the archive ExportRootFS copies to its destination.
	exportTarPath string
	// blockDelete, when set, makes DeleteContainer of that id block until its
	// context is done (modeling a wedged runtime call under teardown's
	// bounded deadline).
	blockDelete string
	// blockInspect, when set, makes post-start Inspect of that id block until
	// its context is done (modeling a wedged observation call under the
	// writer/exporter timeout; pre-start allowlist inspection remains usable).
	blockInspect string
	// blockStart, when set, makes StartContainer of that id block until its
	// context is done (modeling a runtime that wedges launching the VM before
	// StartContainer returns, under the overall handoff budget).
	blockStart string
	// createThenFail, when set to a container name, makes CreateContainer add
	// the container to the runtime but then return an error, modeling an
	// ambiguous create (the object exists though the call reported failure).
	createThenFail string
	// afterAmbiguousContainerCreate runs after createThenFail has inserted the
	// container but before CreateContainer returns its error. Tests use it to
	// cancel the caller context in that exact ambiguity window.
	afterAmbiguousContainerCreate func()
	// createVolumeThenFail makes CreateVolume add the volume and then return
	// an error, modeling an ambiguous post-create failure.
	createVolumeThenFail bool
	// createNetworkThenFail makes CreateNetwork add the network and then
	// return an error, modeling the same ambiguous mutation boundary.
	createNetworkThenFail bool

	onCreateVolume    func(name string) error
	onCreateNetwork   func(name string) error
	onDeleteNetwork   func(name string) (skipRemoval bool, err error)
	onInspectNetwork  func(name string, n NetworkReport) (NetworkReport, error)
	onListNetworks    func(list []NetworkSummary) ([]NetworkSummary, error)
	onDeleteVolume    func(name string) (skipRemoval bool, err error)
	onInspectVolume   func(name string, v VolumeSummary) (VolumeSummary, error)
	onCreateContainer func(spec ContainerSpec) error
	onStart           func(id string) error
	onStop            func(id string) error
	onInspect         func(id string, rep InspectReport) (InspectReport, error)
	onDeleteContainer func(id string) (skipRemoval bool, err error)
	onListContainers  func(list []ContainerSummary) ([]ContainerSummary, error)
	onListVolumes     func(list []VolumeSummary) ([]VolumeSummary, error)
	onExport          func(id string, dest io.Writer) error
	// onCopyIntoContainer overrides the copy outcome. Returning nil without the
	// fake recording anything models the runtime's real and most dangerous
	// behaviour: a copy into a mounted volume writes nothing and still reports
	// success.
	onCopyIntoContainer func(id, hostDir, targetDir string) error
}

// fakeCopy is one host-to-container copy the fake observed.
type fakeCopy struct {
	id, hostDir, targetDir string
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	return &fakeRuntime{ //nolint:gosec // credProofPath is a proof-file path, not a credential
		t:                      t,
		nets:                   map[string]*fakeNetwork{},
		vols:                   map[string]*fakeVol{},
		ctrs:                   map[string]*fakeCtr{},
		volBase:                map[string]string{},
		volTree:                map[string]string{},
		snapshotFiles:          map[string]map[string][]byte{},
		instructionState:       map[string][]byte{},
		stateManifest:          map[string]stateManifestKind{},
		staged:                 map[string]string{},
		baseProofPath:          "/handoff-base.txt",
		credProofPath:          "/handoff-cred.txt",
		writerOutcomeProofPath: writerOutcomeProofPath,
		credState:              map[string]string{},
		runningInspects:        map[string]int{},
		exportTarPath:          buildTar(t, fixtureArchive(t)),
	}
}

func (f *fakeRuntime) CreateNetwork(ctx context.Context, name string, labels []Label) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-network %s", name)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onCreateNetwork != nil {
		if err := f.onCreateNetwork(name); err != nil {
			return err
		}
	}
	if _, duplicate := f.nets[name]; duplicate {
		return fmt.Errorf("network %q already exists", name)
	}
	f.nets[name] = &fakeNetwork{labels: labels, created: f.nextCreated()}
	if f.createNetworkThenFail {
		return errors.New("ambiguous network create failure")
	}
	return nil
}

func (f *fakeRuntime) DeleteNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete-network %s", name)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onDeleteNetwork != nil {
		skip, err := f.onDeleteNetwork(name)
		if err != nil || skip {
			return err
		}
	}
	if _, ok := f.nets[name]; !ok {
		return fmt.Errorf("network %q not found", name)
	}
	delete(f.nets, name)
	return nil
}

func (f *fakeRuntime) ListNetworks(ctx context.Context) ([]NetworkSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list-networks")
	if err := f.checkCtx(ctx); err != nil {
		return nil, err
	}
	out := make([]NetworkSummary, 0, len(f.nets))
	for name, network := range f.nets {
		out = append(out, NetworkSummary{Name: name, Mode: NetworkHostOnly, Labels: network.labels, LabelsObserved: true, CreationDate: network.created})
	}
	if f.onListNetworks != nil {
		return f.onListNetworks(out)
	}
	return out, nil
}

func (f *fakeRuntime) InspectNetwork(ctx context.Context, name string) (NetworkReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("inspect-network %s", name)
	if err := f.checkCtx(ctx); err != nil {
		return NetworkReport{}, err
	}
	network, ok := f.nets[name]
	if !ok {
		return NetworkReport{}, fmt.Errorf("network %q not found", name)
	}
	report := NetworkReport{
		NetworkSummary: NetworkSummary{Name: name, Mode: NetworkHostOnly, Labels: network.labels, LabelsObserved: true, CreationDate: network.created},
		IPv4Gateway:    "127.0.0.1",
		IPv4Subnet:     "127.0.0.0/24",
	}
	if f.onInspectNetwork != nil {
		return f.onInspectNetwork(name, report)
	}
	return report, nil
}

// rwVolume returns the container's read-write volume mount source, if it has
// exactly one; the seeder is the only role that does.
func (c *fakeCtr) rwVolume() (string, bool) {
	for _, m := range c.spec.Mounts {
		if m.Type == MountVolume && !m.ReadOnly {
			return m.Source, true
		}
	}
	return "", false
}

// observedVolume returns the volume a base observer reads, if this container
// is one. The observer and the exporter both hold the workspace read-only, so
// the mount alone cannot tell them apart; what distinguishes the observer is
// that its command writes the proof file the gate will look for.
func (c *fakeCtr) observedVolume(proofPath string) (string, bool) {
	writesProof := false
	for _, arg := range c.spec.Command {
		if strings.Contains(arg, proofPath) {
			writesProof = true
			break
		}
	}
	if !writesProof {
		return "", false
	}
	for _, m := range c.spec.Mounts {
		if m.Type == MountVolume && m.ReadOnly {
			return m.Source, true
		}
	}
	return "", false
}

// mountShadows reports whether target lies at or under one of the container's
// mounts, where the runtime discards a copy while still reporting success.
func (c *fakeCtr) mountShadows(target string) bool {
	for _, m := range c.spec.Mounts {
		if target == m.Target || strings.HasPrefix(target, m.Target+"/") {
			return true
		}
	}
	return false
}

func (c *fakeCtr) ownershipToken() string {
	for _, l := range c.spec.Labels {
		if l.Key == ownershipLabelKey {
			return l.Value
		}
	}
	return ""
}

// credProofFor renders the proof the pinned credential observer would write.
func credProofFor(nonce, digest string) []byte {
	return fmt.Appendf(nil, "%s=%s\n%s=%s\n",
		credProofNonceKey, nonce, credProofTreeKey, digest)
}

// credStateDigest hashes the simulated store content into the proof's
// digest shape; distinct states yield distinct digests, which is all the
// mutation attestation compares.
func credStateDigest(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// baseProofFor renders the proof the pinned observer image would write for a
// workspace holding sha.
func baseProofFor(nonce, sha, tree string) []byte {
	return fmt.Appendf(nil, "%s=%s\n%s=present\n%s=yes\n%s=%s\n%s=clean\n%s=absent\n%s=absent\n%s=%s\n",
		baseProofNonceKey, nonce, baseProofGitDirKey, baseProofDetachedKey,
		baseProofSHAKey, sha, baseProofWorktreeKey, baseProofReplacementsKey, baseProofIrregularKey,
		baseProofTreeKey, tree)
}

// baseProofForAbsentGitDir renders the proof the observer writes over a
// workspace that was never seeded: the observation still happens and is still
// reported, it just reports nothing there.
func baseProofForAbsentGitDir(nonce string) []byte {
	return fmt.Appendf(nil, "%s=%s\n%s=absent\n%s=no\n%s=none\n%s=error\n%s=error\n%s=absent\n%s=none\n",
		baseProofNonceKey, nonce, baseProofGitDirKey, baseProofDetachedKey,
		baseProofSHAKey, baseProofWorktreeKey, baseProofReplacementsKey, baseProofIrregularKey,
		baseProofTreeKey)
}

// snapshotProofFor renders the proof the Codex-review snapshot observer would
// write: valid only when the volume holds exactly the two named files, with
// their sha256 digests. A missing, extra, or renamed entry reports invalid.
func snapshotProofFor(nonce string, files map[string][]byte) []byte {
	auth, hasAuth := files[codexReviewSnapshotAuthName]
	instr, hasInstr := files[codexReviewSnapshotInstrName]
	if !hasAuth || !hasInstr || len(files) != 2 {
		return fmt.Appendf(nil, "nonce=%s\nvalid=invalid\nauth=sha256:\ninstr=sha256:\n", nonce)
	}
	authSum := sha256.Sum256(auth)
	instrSum := sha256.Sum256(instr)
	return fmt.Appendf(nil, "nonce=%s\nvalid=valid\nauth=sha256:%x\ninstr=sha256:%x\n", nonce, authSum, instrSum)
}

// writeProofTar streams a one-entry archive carrying proof at absolutePath,
// the shape the gate extracts from an observer's exported rootfs.
func writeProofTar(w io.Writer, absolutePath string, proof []byte) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(absolutePath, "/"),
		Typeflag: tar.TypeReg,
		Mode:     0o600,
		Size:     int64(len(proof)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(proof); err != nil {
		return err
	}
	return tw.Close()
}

// nextCreated mints a distinct opaque creation fingerprint. Callers hold mu.
func (f *fakeRuntime) nextCreated() string {
	f.seq++
	return fmt.Sprintf("fake-created-%d", f.seq)
}

var _ Runtime = (*fakeRuntime)(nil)

func (f *fakeRuntime) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// callIndex returns the position of the first recorded call equal to s, or
// -1 when it never happened.
func (f *fakeRuntime) callIndex(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c == s {
			return i
		}
	}
	return -1
}

// checkCtx models the real CLIRuntime, whose exec.CommandContext calls fail
// once the context is cancelled. It lets TestHandoffCancelled prove teardown
// runs under context.WithoutCancel: without that detachment, teardown's
// runtime calls would see the cancelled context and fail here.
func (f *fakeRuntime) checkCtx(ctx context.Context) error { return ctx.Err() }

func (f *fakeRuntime) CreateVolume(ctx context.Context, name string, _ int64, labels []Label) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-volume %s", name)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onCreateVolume != nil {
		if err := f.onCreateVolume(name); err != nil {
			return err
		}
	}
	if _, dup := f.vols[name]; dup {
		return fmt.Errorf("volume %q already exists", name)
	}
	f.vols[name] = &fakeVol{labels: labels, created: f.nextCreated()}
	if f.createVolumeThenFail {
		return fmt.Errorf("create of volume %q reported failure after the volume was made", name)
	}
	return nil
}

func (f *fakeRuntime) DeleteVolume(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete-volume %s", name)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onDeleteVolume != nil {
		skip, err := f.onDeleteVolume(name)
		if err != nil || skip {
			return err
		}
	}
	if _, ok := f.vols[name]; !ok {
		return fmt.Errorf("volume %q not found", name)
	}
	delete(f.vols, name)
	return nil
}

func (f *fakeRuntime) ListVolumes(ctx context.Context) ([]VolumeSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list-volumes")
	if err := f.checkCtx(ctx); err != nil {
		return nil, err
	}
	out := make([]VolumeSummary, 0, len(f.vols))
	for name, v := range f.vols {
		out = append(out, VolumeSummary{Name: name, Labels: v.labels, LabelsObserved: true, CreationDate: v.created})
	}
	if f.onListVolumes != nil {
		return f.onListVolumes(out)
	}
	return out, nil
}

func (f *fakeRuntime) InspectVolume(ctx context.Context, name string) (VolumeSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("inspect-volume %s", name)
	if err := f.checkCtx(ctx); err != nil {
		return VolumeSummary{}, err
	}
	v, ok := f.vols[name]
	var sum VolumeSummary
	if ok {
		sum = VolumeSummary{Name: name, Labels: v.labels, LabelsObserved: true, CreationDate: v.created}
	}
	if f.onInspectVolume != nil {
		return f.onInspectVolume(name, sum)
	}
	if !ok {
		return VolumeSummary{}, fmt.Errorf("volume %q not found", name)
	}
	return sum, nil
}

func (f *fakeRuntime) CreateContainer(ctx context.Context, spec ContainerSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-container %s", spec.Name)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onCreateContainer != nil {
		if err := f.onCreateContainer(spec); err != nil {
			return err
		}
	}
	if _, dup := f.ctrs[spec.Name]; dup {
		return fmt.Errorf("container %q already exists", spec.Name)
	}
	f.ctrs[spec.Name] = &fakeCtr{spec: spec, created: f.nextCreated()}
	if f.createThenFail == spec.Name {
		if f.afterAmbiguousContainerCreate != nil {
			f.afterAmbiguousContainerCreate()
		}
		// The object now exists, but the call reports failure (ambiguous
		// create): teardown must reap it by listing, not by a create flag.
		return fmt.Errorf("create of %q reported failure after the container was made", spec.Name)
	}
	return nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	f.record("start-container %s", id)
	if err := f.checkCtx(ctx); err != nil {
		f.mu.Unlock()
		return err
	}
	if f.blockStart == id {
		// Wedge inside the call: release the lock (teardown must still run) and
		// block until the overall handoff budget cancels the context.
		f.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	defer f.mu.Unlock()
	if f.onStart != nil {
		if err := f.onStart(id); err != nil {
			return err
		}
	}
	c, ok := f.ctrs[id]
	if !ok {
		return fmt.Errorf("container %q not found", id)
	}
	c.started = true
	if strings.HasSuffix(id, "-cfg-seed") {
		if volume, ok := c.rwVolume(); ok {
			if c.spec.Mounts[0].Target == claudeConfigRootVolumeTarget {
				f.stateManifest[volume] = stateManifestConfigRoot
			} else {
				f.stateManifest[volume] = stateManifestEmpty
			}
		}
	}
	return nil
}

func (f *fakeRuntime) StopContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop-container %s", id)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onStop != nil {
		if err := f.onStop(id); err != nil {
			return err
		}
	}
	c, ok := f.ctrs[id]
	if !ok {
		return fmt.Errorf("container %q not found", id)
	}
	c.stopped = true
	return nil
}

// state computes a container's currently observable state without recording
// an inspect.
func (f *fakeRuntime) state(c *fakeCtr, name string) ContainerState {
	if !c.started || c.stopped {
		return StateStopped
	}
	running := 1
	if n, ok := f.runningInspects[name]; ok {
		running = n
	}
	if c.inspects > running {
		return StateStopped
	}
	return StateRunning
}

func (f *fakeRuntime) Inspect(ctx context.Context, id string) (InspectReport, error) {
	f.mu.Lock()
	f.record("inspect %s", id)
	if err := f.checkCtx(ctx); err != nil {
		f.mu.Unlock()
		return InspectReport{}, err
	}
	c, ok := f.ctrs[id]
	if !ok {
		f.mu.Unlock()
		return InspectReport{}, fmt.Errorf("container %q not found", id)
	}
	block := f.blockInspect == id && c.started
	if block {
		f.mu.Unlock()
		<-ctx.Done()
		return InspectReport{}, ctx.Err()
	}
	defer f.mu.Unlock()
	if c.started && !c.stopped {
		c.inspects++
	}
	rep := InspectReport{
		ID:                      id,
		Command:                 append([]string(nil), c.spec.Command...),
		WorkingDirectory:        "/",
		State:                   f.state(c, id),
		CreationDate:            c.created,
		AllowlistFieldsObserved: true,
		Mounts:                  append([]Mount(nil), c.spec.Mounts...),
		Env:                     append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, c.spec.Env...),
		Labels:                  append([]Label(nil), c.spec.Labels...),
		LabelsObserved:          true,
		NetworksObserved:        true,
	}
	if c.spec.Network != "" {
		rep.Networks = []string{c.spec.Network}
	} else if !c.spec.NetworkDisabled {
		rep.Networks = []string{"default"}
	}
	rep.NetworkAttachmentCount = len(rep.Networks)
	// Apple container 1.1.0 reports the full pinned reference (name@digest);
	// the tag, if any, is dropped and the descriptor's resolved digest is a
	// different value the report does not carry.
	rep.ImageReference = c.spec.Image
	if f.onInspect != nil {
		return f.onInspect(id, rep)
	}
	return rep, nil
}

func (f *fakeRuntime) DeleteContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	f.record("delete-container %s", id)
	block := f.blockDelete == id
	f.mu.Unlock()
	if block {
		// Model a wedged runtime call: block until the (teardown-bounded)
		// context expires. Held outside the lock so teardown can proceed.
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onDeleteContainer != nil {
		skip, err := f.onDeleteContainer(id)
		if err != nil || skip {
			return err
		}
	}
	c, ok := f.ctrs[id]
	if !ok {
		return fmt.Errorf("container %q not found", id)
	}
	if f.state(c, id) == StateRunning {
		return fmt.Errorf("container %q is running", id)
	}
	delete(f.ctrs, id)
	// A deleted container's in-flight seeding stage is gone with it, so a later
	// same-named seeder (e.g. a relaunch after recovery) stages afresh rather
	// than resuming this one's already-removed host directory.
	delete(f.staged, id)
	return nil
}

func (f *fakeRuntime) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list-containers")
	if err := f.checkCtx(ctx); err != nil {
		return nil, err
	}
	out := make([]ContainerSummary, 0, len(f.ctrs))
	for name, c := range f.ctrs {
		out = append(out, ContainerSummary{
			ID: name, State: f.state(c, name), Labels: append([]Label(nil), c.spec.Labels...), LabelsObserved: true,
			CreationDate: c.created,
		})
	}
	if f.onListContainers != nil {
		return f.onListContainers(out)
	}
	return out, nil
}

// CopyIntoContainer models Apple container 1.1.0's copy: it refuses a
// container that is not running, which is why the gate must start the seeder
// rather than address a merely created one.
func (f *fakeRuntime) CopyIntoContainer(ctx context.Context, id, hostDir, targetDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("copy %s %s %s", id, hostDir, targetDir)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onCopyIntoContainer != nil {
		if err := f.onCopyIntoContainer(id, hostDir, targetDir); err != nil {
			return err
		}
	}
	c, ok := f.ctrs[id]
	if !ok {
		return fmt.Errorf("container %q not found", id)
	}
	if !c.started || c.stopped {
		return fmt.Errorf("invalidState: container %q is not running", id)
	}
	f.copies = append(f.copies, fakeCopy{id: id, hostDir: hostDir, targetDir: targetDir})
	// The runtime's silent discard, modeled structurally rather than by a flag:
	// a copy whose destination lies inside a mounted volume writes nothing and
	// still succeeds. Deriving it from the container's own mounts means a
	// regression that aims a copy into the workspace mount fails the fake the
	// same way it would fail production, instead of passing because the fake
	// only looked at call ordinality.
	if c.mountShadows(targetDir) || f.copySeedsNothing {
		return nil
	}

	// Simulate the guest side. The first copy stages a tree into the
	// container's own filesystem and changes no volume; the second is the
	// completion sentinel, and only then does the seeder's own command move the
	// tree onto the workspace. Modeling that ordering is the point: a gate that
	// seeded without signalling completion would pass a fake that copied on the
	// first call.
	if _, staged := f.staged[id]; !staged {
		f.staged[id] = hostDir
		return nil
	}
	src, ok := f.staged[id]
	if !ok {
		return nil
	}
	vol, ok := f.ctrs[id].rwVolume()
	if !ok {
		// No read-write volume: the copy landed in the rootfs and nothing
		// reaches a workspace, exactly as the runtime behaves.
		return nil
	}
	// The Codex-review snapshot seeder moves exactly two named files onto its
	// volume. Record whatever the staged tree actually holds so an altered or
	// partial copy fails the observer's exact-two-files attestation.
	if c.spec.Mounts[0].Target == codexReviewSnapshotSeedTarget {
		files := map[string][]byte{}
		entries, dirErr := os.ReadDir(src)
		if dirErr == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					files[entry.Name()+"/"] = nil
					continue
				}
				body, readErr := os.ReadFile(filepath.Join(src, entry.Name())) //nolint:gosec // test fixture path
				if readErr == nil {
					files[entry.Name()] = append([]byte(nil), body...)
				}
			}
		}
		f.snapshotFiles[vol] = files
		return nil
	}
	head, err := os.ReadFile(filepath.Join(src, ".git", "HEAD")) //nolint:gosec // test fixture path
	if err != nil {
		body, readErr := os.ReadFile(filepath.Join(src, instructionFileName)) //nolint:gosec // test fixture path
		switch {
		case readErr == nil:
			f.instructionState[vol] = make([]byte, len(body))
			copy(f.instructionState[vol], body)
		case errors.Is(readErr, os.ErrNotExist):
			f.instructionState[vol] = nil
		}
		return nil
	}
	f.volBase[vol] = strings.TrimSpace(string(head))
	// The volume receives the staged tree, so the digest the observer reports
	// is the digest of what was staged. Computing it from the fixture rather
	// than echoing the host's expectation is what lets an altered or partial
	// copy fail the attestation in a test.
	f.volTree[vol] = digestOfDir(f.t, src)
	return nil
}

// digestOfDir computes the tree digest the observer would report for a
// directory, using the same helpers the host applies to the seed source.
func digestOfDir(t *testing.T, root string) string {
	t.Helper()
	var lines, execPaths, dirPaths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			dirPaths = append(dirPaths, findPath(rel))
			return nil
		}
		sum, sumErr := fileSHA256(p)
		if sumErr != nil {
			return sumErr
		}
		lines = append(lines, sum+"  ./"+filepath.ToSlash(rel))
		fi, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if fi.Mode().Perm()&0o100 != 0 {
			execPaths = append(execPaths, "./"+filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("digest fixture tree %s: %v", root, err)
	}
	return treeDigest(lines, execPaths, dirPaths)
}

func (f *fakeRuntime) ExportRootFS(ctx context.Context, id string, dest io.Writer, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("export %s", id)
	if err := f.checkCtx(ctx); err != nil {
		return err
	}
	if f.onExport != nil {
		if err := f.onExport(id, dest); err != nil {
			return err
		}
	}
	c, ok := f.ctrs[id]
	if !ok {
		return fmt.Errorf("container %q not found", id)
	}
	// The base observer's rootfs carries its proof, not an export. It always
	// carries one: the real observer's script has no `set -e` precisely so
	// every branch reaches the proof write, and an unseeded workspace is
	// reported as git_dir=absent rather than as a missing file. Falling through
	// to the export fixture here would make the "copy seeded nothing" case
	// exercise the missing-file branch instead of the one production takes.
	if vol, isObserver := c.observedVolume(f.baseProofPath); isObserver {
		proof := baseProofForAbsentGitDir(c.ownershipToken())
		if sha, seeded := f.volBase[vol]; seeded {
			proof = baseProofFor(c.ownershipToken(), sha, f.volTree[vol])
		}
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, f.baseProofPath, proof)
	}
	if vol, isObserver := c.observedVolume(codexWorkspaceProofPath); isObserver {
		proof := baseProofForAbsentGitDir(c.ownershipToken())
		if sha, seeded := f.volBase[vol]; seeded {
			proof = baseProofFor(c.ownershipToken(), sha, f.volTree[vol])
		}
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, codexWorkspaceProofPath, proof)
	}
	if _, isShadowObserver := c.observedVolume(codexShadowProofPath); isShadowObserver {
		proof := []byte(fmt.Sprintf(
			"nonce=%s\nempty=yes\ntree=%s\n", c.ownershipToken(), emptyCodexShadowDigest,
		))
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, codexShadowProofPath, proof)
	}
	if vol, isSnapshotObserver := c.observedVolume(codexReviewSnapshotProofPath); isSnapshotObserver {
		proof := snapshotProofFor(c.ownershipToken(), f.snapshotFiles[vol])
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, codexReviewSnapshotProofPath, proof)
	}
	// A credential-store observer's rootfs carries its digest proof: the
	// simulated store content is credState (empty state digests too, like an
	// empty volume), so a test mutates the store by changing the string.
	if vol, isCredObserver := c.observedVolume(f.credProofPath); isCredObserver {
		proof := credProofFor(c.ownershipToken(), credStateDigest(f.credState[vol]))
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, f.credProofPath, proof)
	}
	if vol, isInstructionObserver := c.observedVolume(instructionProofPath); isInstructionObserver {
		body, seeded := f.instructionState[vol]
		proof := instructionProofFor(c.ownershipToken(), body, seeded)
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, instructionProofPath, proof)
	}
	if vol, isStateObserver := c.observedVolume(stateProofPath); isStateObserver {
		kind := stateManifestEmpty
		for _, arg := range c.spec.Command {
			if strings.Contains(arg, "'config_root'") {
				kind = stateManifestConfigRoot
			}
		}
		valid := kind == stateManifestEmpty
		if kind == stateManifestConfigRoot {
			valid = f.stateManifest[vol] == stateManifestConfigRoot
		}
		proof := stateProofFor(c.ownershipToken(), kind, valid)
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, stateProofPath, proof)
	}
	if _, isWriterObserver := c.observedVolume(f.writerOutcomeProofPath); isWriterObserver {
		proof := fmt.Appendf(nil, "%s %d\n", c.ownershipToken(), f.writerStatus)
		if f.observerProof != nil {
			proof = f.observerProof(id, proof)
		}
		return writeProofTar(dest, f.writerOutcomeProofPath, proof)
	}
	src, err := os.Open(f.exportTarPath)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only test fixture
	_, err = io.Copy(dest, src)
	return err
}

func instructionProofFor(nonce string, body []byte, seeded bool) []byte {
	present, digest, contents := "no", "none", "dirty"
	if seeded {
		contents = "clean"
		if body != nil {
			sum := sha256.Sum256(body)
			present = "yes"
			digest = fmt.Sprintf("%x", sum)
		}
	}
	return []byte(fmt.Sprintf(
		"nonce=%s\npresent=%s\ndigest=%s\ncontents=%s\n",
		nonce, present, digest, contents,
	))
}

func stateProofFor(
	nonce string,
	kind stateManifestKind,
	valid bool,
) []byte {
	contents := "invalid"
	if valid {
		contents = "valid"
	}
	return fmt.Appendf(
		nil,
		"nonce=%s\nkind=%s\ncontents=%s\ndigest=%s\n",
		nonce,
		kind,
		contents,
		credStateDigest(string(kind)+":"+contents),
	)
}
