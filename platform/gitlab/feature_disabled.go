package gitlab

import (
	"context"
	"errors"
	"net/http"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

func (c *Client) repositoryFeatureError(
	ctx context.Context,
	ref platform.RepoRef,
	feature string,
	capability string,
	err error,
) error {
	classified, _ := c.classifyRepositoryFeatureError(ctx, ref, feature, capability, err)
	return classified
}

func (c *Client) repositoryItemLookupError(
	ctx context.Context,
	ref platform.RepoRef,
	feature string,
	capability string,
	err error,
) error {
	classified, repositoryExists := c.classifyRepositoryFeatureError(
		ctx, ref, feature, capability, err,
	)
	if repositoryExists && errors.Is(classified, platform.ErrNotFound) &&
		!errors.Is(classified, platform.ErrRepositoryFeatureDisabled) {
		return errors.Join(platform.ErrLookupNotPresent, classified)
	}
	return classified
}

func (c *Client) classifyRepositoryFeatureError(
	ctx context.Context,
	ref platform.RepoRef,
	feature string,
	capability string,
	err error,
) (error, bool) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return c.mapGitLabError(capability, err), false
	}

	var responseErr *gitlab.ErrorResponse
	if !errors.As(err, &responseErr) || responseErr == nil ||
		(!responseErr.HasStatusCode(http.StatusForbidden) &&
			!responseErr.HasStatusCode(http.StatusNotFound) &&
			!responseErr.HasStatusCode(http.StatusGone)) {
		return c.mapGitLabError(capability, err), false
	}

	repository, lookupErr := c.GetRepository(ctx, ref)
	if lookupErr != nil {
		return c.mapGitLabError(capability, err), false
	}
	enabled, known := repository.FeatureEnabled(feature)
	if known && !enabled {
		return platform.RepositoryFeatureDisabled(platform.KindGitLab, c.host, feature, err), true
	}
	return c.mapGitLabError(capability, err), true
}
