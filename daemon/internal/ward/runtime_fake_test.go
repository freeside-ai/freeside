package ward

import "context"

// stubRuntime is the inert Runtime for tests that never drive the
// lifecycle; handoff tests use the scripted fakeRuntime instead.
type stubRuntime struct{}

var _ Runtime = stubRuntime{}

func (stubRuntime) CreateVolume(context.Context, string, int64, []Label) error { return nil }
func (stubRuntime) DeleteVolume(context.Context, string) error                 { return nil }
func (stubRuntime) ListVolumes(context.Context) ([]VolumeSummary, error)       { return nil, nil }
func (stubRuntime) CreateContainer(context.Context, ContainerSpec) error       { return nil }
func (stubRuntime) StartContainer(context.Context, string) error               { return nil }
func (stubRuntime) Inspect(context.Context, string) (InspectReport, error) {
	return InspectReport{}, nil
}
func (stubRuntime) DeleteContainer(context.Context, string) error              { return nil }
func (stubRuntime) ListContainers(context.Context) ([]ContainerSummary, error) { return nil, nil }
func (stubRuntime) ExportRootFS(context.Context, string, string) error         { return nil }
