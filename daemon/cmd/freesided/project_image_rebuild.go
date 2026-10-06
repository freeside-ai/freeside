package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// newProjectImageRebuild composes the builder the publication lane rebuilds a
// project image with (plan §5.7), or nil when the operator supplied no
// -base-build-ref: the lane then blocks a dependency change it cannot rebuild
// for.
//
// The builder holds no token source. A rebuild reads the candidate from the
// lane's own checkout and never reaches the forge, so it carries no GitHub
// authority at all.
func newProjectImageRebuild(cfg claudeDriverConfig, st *store.Store) (*engine.ProjectImageRebuild, error) {
	if cfg.Rebuild.BaseBuildRef == "" {
		return nil, nil
	}
	scratch := filepath.Join(cfg.SeedRoot, "project-image-rebuild")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("create project-image rebuild scratch: %w", err)
	}
	builder, err := projectimage.New(projectimage.Options{
		ContainerPath: cfg.ContainerBin, TempDir: scratch, Log: os.Stderr,
		Record: func(ctx context.Context, image domain.ProjectImage) error {
			return st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				return tx.RecordProjectImage(ctx, image)
			})
		},
		LookupRecordedRef: func(ctx context.Context, ref string) (bool, error) {
			recorded := false
			err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				recorded, err = tx.ProjectImageRefRecorded(ctx, domain.ImageRef(ref))
				return err
			})
			return recorded, err
		},
	})
	if err != nil {
		return nil, fmt.Errorf("compose project-image rebuild: %w", err)
	}
	return &engine.ProjectImageRebuild{Builder: builder, Inputs: cfg.Rebuild}, nil
}
