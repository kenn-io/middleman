package gitlab

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

func (c *Client) ListLabels(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.LabelCatalog, error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.LabelCatalog{}, err
	}
	opt := &gitlab.ListLabelsOptions{Page: 1, PerPage: defaultPageSize}

	var out []platform.Label
	for {
		labels, resp, err := c.api.Labels.ListLabels(pid, opt, gitlab.WithContext(ctx))
		if err != nil {
			return platform.LabelCatalog{}, c.mapGitLabError("read_labels", err)
		}
		for _, label := range labels {
			if label == nil {
				continue
			}
			out = append(out, platform.Label{
				Repo:               normalizedRef,
				PlatformID:         label.ID,
				PlatformExternalID: strconv.FormatInt(label.ID, 10),
				Name:               label.Name,
				Description:        label.Description,
				Color:              label.Color,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			return platform.LabelCatalog{Labels: out}, nil
		}
		opt.Page = resp.NextPage
	}
}

func (c *Client) SetMergeRequestLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	if err := c.validateAssignableLabelNames(names); err != nil {
		return nil, err
	}
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	labels := assignableLabelOptions(names)
	mr, _, err := c.api.MergeRequests.UpdateMergeRequest(
		pid,
		int64(number),
		&gitlab.UpdateMergeRequestOptions{Labels: &labels},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return nil, c.mapGitLabError("label_mutation", err)
	}
	return normalizeLabelNames(normalizedRef, mr.Labels), nil
}

func (c *Client) SetIssueLabels(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	names []string,
) ([]platform.Label, error) {
	if err := c.validateAssignableLabelNames(names); err != nil {
		return nil, err
	}
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	labels := assignableLabelOptions(names)
	issue, _, err := c.api.Issues.UpdateIssue(
		pid,
		int64(number),
		&gitlab.UpdateIssueOptions{Labels: &labels},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return nil, c.mapGitLabError("label_mutation", err)
	}
	return normalizeLabelNames(normalizedRef, issue.Labels), nil
}

// assignableLabelOptions always returns a non-nil LabelOptions: the SDK
// marshals nil to JSON null (labels untouched) while an empty value
// marshals to "" which clears every label on the merge request or issue.
func assignableLabelOptions(names []string) gitlab.LabelOptions {
	return append(gitlab.LabelOptions{}, names...)
}

// validateAssignableLabelNames rejects label names GitLab's labels
// parameter cannot express: the SDK comma-joins names, so a name
// containing a comma would be split and assigned as multiple labels.
func (c *Client) validateAssignableLabelNames(names []string) error {
	for _, name := range names {
		if strings.Contains(name, ",") {
			return &platform.Error{
				Code:         platform.ErrCodeInvalidArgument,
				Provider:     platform.KindGitLab,
				PlatformHost: c.host,
				Capability:   "label_mutation",
				Field:        "labels",
				Err: fmt.Errorf(
					"label %q contains a comma; GitLab's labels parameter is comma-separated and cannot assign it",
					name,
				),
			}
		}
	}
	return nil
}

var _ platform.LabelReader = (*Client)(nil)
var _ platform.LabelMutator = (*Client)(nil)
