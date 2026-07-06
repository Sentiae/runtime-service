package oci

import (
	"context"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// MaterializerAdapter adapts *Materializer to the usecase.ImageMaterializer
// port so the FleetProvision use case depends only on its own interface.
type MaterializerAdapter struct {
	m *Materializer
}

var _ usecase.ImageMaterializer = (*MaterializerAdapter)(nil)

// NewMaterializerAdapter wraps a Materializer.
func NewMaterializerAdapter(m *Materializer) *MaterializerAdapter {
	return &MaterializerAdapter{m: m}
}

// Materialize satisfies usecase.ImageMaterializer.
func (a *MaterializerAdapter) Materialize(ctx context.Context, in usecase.ImageMaterializeInput) (usecase.ImageMaterializeOutput, error) {
	res, err := a.m.Materialize(ctx, MaterializeRequest{
		Image: ImageRef{
			Registry:   in.Registry,
			Repository: in.Repository,
			Digest:     in.Digest,
			ChangeID:   in.ChangeID,
		},
		WorkDir: in.WorkDir,
		EnvVars: in.EnvVars,
		Mode:    in.Mode,
		TestCmd: in.TestCommand,
		Port:    in.Port,
	})
	if err != nil {
		return usecase.ImageMaterializeOutput{}, err
	}
	return usecase.ImageMaterializeOutput{RootfsPath: res.RootfsPath}, nil
}
