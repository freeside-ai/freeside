package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// productionEngine is the one place a production composition (the Claude
// workflow and the scenario) turns its parts into engine options, so the two
// cannot drift apart unseen: a part one of them leaves out is an explicit
// zero value at its call site.
//
// Contract: the builder passes every value through unchanged and decides
// none of them. Required fields always become their option, nil or not, so
// the engine's own checks still fail startup on a missing part. Optional
// fields (SelectImage, Inference, HealthRefresh, Logger, UsageSink) become
// their option only when non-nil, because each of those engine options
// rejects nil; a nil optional field means the part is not installed.
//
// It uses no package-main helper, so it can move out of package main
// unchanged apart from exporting its names.
type productionEngine struct {
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
}

// options returns the engine options in the order the Claude workflow has
// always applied them.
func (p productionEngine) options() []engine.Option {
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
	return options
}
