package ward

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
)

// stubRuntime is the inert Runtime for tests that never drive the
// lifecycle; handoff tests use the scripted fakeRuntime instead.
type stubRuntime struct{}

var _ Runtime = stubRuntime{}

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

// fakeRuntime is the scripted Runtime driving the lifecycle tests: default
// behavior models Apple container 1.1.0 (a created-but-never-started
// container reports stopped; a started one reports running for
// runningInspects polls, then stopped), records every call in order, and
// per-method override hooks induce each conformance violation.
type fakeRuntime struct {
	t  *testing.T
	mu sync.Mutex

	calls []string
	vols  map[string]*fakeVol
	ctrs  map[string]*fakeCtr
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

	onCreateVolume    func(name string) error
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
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	return &fakeRuntime{
		t:               t,
		vols:            map[string]*fakeVol{},
		ctrs:            map[string]*fakeCtr{},
		runningInspects: map[string]int{},
		exportTarPath:   buildTar(t, fixtureArchive(t)),
	}
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
	}
	rep.ImageReference, rep.ImageDigest, _ = strings.Cut(c.spec.Image, "@")
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
	if _, ok := f.ctrs[id]; !ok {
		return fmt.Errorf("container %q not found", id)
	}
	src, err := os.Open(f.exportTarPath)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only test fixture
	_, err = io.Copy(dest, src)
	return err
}
