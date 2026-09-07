package archive

import (
	"context"

	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/platform"
)

type Controller interface {
	Start(context.Context, []platform.RepoRef) ([]Status, error)
	StartAll(context.Context) ([]Status, error)
	Pause(context.Context, []platform.RepoRef) ([]Status, error)
	PauseAll(context.Context) ([]Status, error)
	Status(context.Context, []platform.RepoRef) ([]Status, error)
	Report(context.Context, ReportOptions) (report.Model, error)
}
