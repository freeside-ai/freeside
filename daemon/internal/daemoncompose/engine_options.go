package daemoncompose

import (
	"context"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ProductionEngine is the one place a production composition (the Claude
// workflow and the scenario) turns its parts into engine options, so the two
// cannot drift apart unseen: a part one of them leaves out is an explicit
// zero value at its call site.
//
// Contract: the builder passes every value through unchanged and decides
// none of them. Required fields always become their option, nil or not, so
// the engine's own checks still fail startup on a missing part. Optional
// fields (SelectImage, Inference, HealthRefresh, Logger, UsageSink,
// HostVerification, Running) become their option only when non-nil, because each of
// those engine options rejects nil; a nil optional field means the part is
// not installed.
type ProductionEngine struct {
	// Required.
	Lock  *daemonlock.Lock
	Bound ward.LaunchBound
	// Backend, AdmissionFloor, Admission, and AdmissionNow together make
	// engine.WithAdmission.
	Backend            exec.RunnerBackend
	AdmissionFloor     []exec.Capability
	Admission          engine.AdmissionEnvironment
	AdmissionNow       func() time.Time
	Derive             engine.AdmissionDerivation
	DeliveryValidation func(context.Context, exec.StartSpec) error
	Specification      engine.SpecificationConfig
	Cancellation       engine.TaskCancellationRuntime
	Publication        engine.ProductionPublicationConfig

	// Optional: nil leaves the option out.
	SelectImage   engine.ImageSelection
	Inference     *inference.Client
	HealthRefresh func(context.Context) error
	Logger        *slog.Logger
	UsageSink     engine.ClaudeUsageSink
	// HostVerification, when set, is the macOS verification executor the
	// engine admits macos_host recipe commands onto.
	HostVerification *macverify.Backend
	// Running is the daemon's count of in-process work, which the restart
	// readiness read reports.
	Running *engine.RunningWork
}

// Options returns the engine Options in the order the Claude workflow has
// always applied them.
func (p ProductionEngine) Options() []engine.Option {
	options := []engine.Option{
		engine.WithDaemonLock(p.Lock),
		engine.WithMachineCapacity(p.Bound),
		engine.WithAdmission(p.Backend, p.AdmissionFloor, p.Admission, p.AdmissionNow),
		engine.WithAdmissionDerivation(p.Derive),
	}
	if p.SelectImage != nil {
		options = append(options, engine.WithAdmissionImageSelection(p.SelectImage))
	}
	if p.Inference != nil {
		options = append(options, engine.WithInference(p.Inference))
	}
	options = append(options,
		engine.WithProductionDeliveryValidation(p.DeliveryValidation),
		engine.WithSpecification(p.Specification),
		engine.WithTaskCancellationRuntime(p.Cancellation),
		engine.WithProductionPublication(p.Publication),
	)
	if p.HealthRefresh != nil {
		options = append(options, engine.WithUnattendedHealthRefresh(p.HealthRefresh))
	}
	if p.Logger != nil {
		options = append(options, engine.WithLogger(p.Logger))
	}
	if p.UsageSink != nil {
		options = append(options, engine.WithClaudeUsageSink(p.UsageSink))
	}
	if backend := p.HostVerification; backend != nil {
		options = append(options, engine.WithHostVerificationAdmission(backend,
			func() (verify.Room, error) {
				room, err := macverify.NewRoom(backend, macverify.RoomOptions{})
				if err != nil {
					return nil, err
				}
				return room, nil
			}))
	}
	if p.Running != nil {
		options = append(options, engine.WithRunningWork(p.Running))
	}
	return options
}
