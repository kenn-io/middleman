package gitealike

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/forge/platform"
)

func (p *Provider) ClassifyRepositoryFeatureError(
	ctx context.Context,
	ref platform.RepoRef,
	feature string,
	err error,
) error {
	return p.repositoryFeatureError(ctx, ref, feature, err)
}

func (p *Provider) repositoryFeatureError(
	ctx context.Context,
	ref platform.RepoRef,
	feature string,
	err error,
) error {
	classified, _ := p.classifyRepositoryFeatureError(ctx, ref, feature, err)
	return classified
}

func (p *Provider) repositoryItemLookupError(
	ctx context.Context,
	ref platform.RepoRef,
	feature string,
	err error,
) error {
	classified, repositoryExists := p.classifyRepositoryFeatureError(ctx, ref, feature, err)
	if repositoryExists && errors.Is(classified, platform.ErrNotFound) &&
		!errors.Is(classified, platform.ErrRepositoryFeatureDisabled) {
		return errors.Join(platform.ErrLookupNotPresent, classified)
	}
	return classified
}

func (p *Provider) classifyRepositoryFeatureError(
	ctx context.Context,
	ref platform.RepoRef,
	feature string,
	err error,
) (error, bool) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return p.mapError(err), false
	}

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr == nil ||
		(httpErr.StatusCode != http.StatusForbidden &&
			httpErr.StatusCode != http.StatusNotFound &&
			httpErr.StatusCode != http.StatusGone) {
		return p.mapError(err), false
	}

	dto, lookupErr := p.transport.GetRepository(ctx, ref.Owner, ref.Name)
	if lookupErr != nil {
		return p.mapError(err), false
	}
	repository, normalizeErr := NormalizeRepository(p.kind, p.host, dto)
	if normalizeErr != nil {
		return p.mapError(err), false
	}
	enabled, known := repository.FeatureEnabled(feature)
	if known && !enabled {
		return platform.RepositoryFeatureDisabled(p.kind, p.host, feature, err), true
	}
	return p.mapError(err), true
}
