package pullapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server/httpapi"
)

type setLabelsOutput = httpapi.BodyOutput[httpapi.ItemLabelsResponse]

type setPullLabelsInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         httpapi.SetLabelsRequest
}

func (s *Handler) setPullLabels(
	ctx context.Context,
	input *setPullLabelsInput,
) (*setLabelsOutput, error) {
	repo, mr, names, err := s.resolveRequestedLabelNames(
		ctx,
		input.Provider,
		input.PlatformHost,
		input.Owner,
		input.Name,
		input.Number,
		input.Body.LabelNames(),
	)
	if err != nil {
		return nil, err
	}

	if s.syncer == nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityLabelMutation)
	}
	mutator, err := s.syncer.LabelMutator(repoProviderKind(*repo), repoProviderHost(*repo))
	if err != nil {
		return nil, unsupportedCapabilityProblem(*repo, capabilityLabelMutation)
	}
	providerLabels, err := mutator.SetMergeRequestLabels(
		ctx, platformRepoRefFromDB(*repo), input.Number, names,
	)
	if err != nil {
		return nil, httpapi.ProviderCallProblemWithDetail(
			err,
			string(repoProviderKind(*repo)), repoProviderHost(*repo),
			"provider API error: "+err.Error(),
		)
	}
	labels := platformdb.DBLabels(providerLabels, time.Now().UTC())
	if err := s.db.ReplaceMergeRequestLabels(ctx, repo.ID, mr.ID, labels); err != nil {
		return nil, httpapi.Internal("save pull labels failed")
	}
	// Re-read the stored rows: the label store merges provider responses
	// with the repo label catalog, so providers that return bare names
	// (GitLab) still yield color and description here.
	stored, err := s.visibleMergeRequest(ctx, repo.ID, input.Number)
	if err != nil || stored == nil {
		return nil, httpapi.Internal("get pull failed")
	}
	return &setLabelsOutput{Body: httpapi.ItemLabelsResponse{Labels: stored.Labels}}, nil
}

func (s *Handler) resolveRequestedLabelNames(
	ctx context.Context,
	provider string,
	platformHost string,
	owner string,
	name string,
	number int,
	names []string,
) (*db.Repo, *db.MergeRequest, []string, error) {
	repo, err := s.lookupRepoByProviderRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, nil, nil, providerRouteLookupError(err)
	}
	caps := s.capabilitiesForRepo(*repo)
	if !capabilityEnabled(caps, capabilityReadLabels) {
		return nil, nil, nil, unsupportedCapabilityProblem(*repo, capabilityReadLabels)
	}
	if !capabilityEnabled(caps, capabilityLabelMutation) {
		return nil, nil, nil, unsupportedCapabilityProblem(*repo, capabilityLabelMutation)
	}
	mr, err := s.requireVisibleMergeRequest(ctx, repo, number)
	if err != nil {
		return nil, nil, nil, err
	}
	if names == nil {
		return nil, nil, nil, httpapi.Validation("body.labels", "labels must be an array")
	}

	catalog, freshness, err := s.db.ListRepoLabelCatalog(ctx, repo.ID)
	if err != nil {
		return nil, nil, nil, httpapi.Internal("list repo labels failed")
	}
	if labelCatalogStale(freshness, time.Now().UTC()) && s.syncer != nil {
		_ = s.syncer.RefreshRepoLabelCatalog(ctx, *repo)
		catalog, _, err = s.db.ListRepoLabelCatalog(ctx, repo.ID)
		if err != nil {
			return nil, nil, nil, httpapi.Internal("list repo labels failed")
		}
	}
	catalogByName := make(map[string]struct{}, len(catalog))
	for _, label := range catalog {
		catalogByName[label.Name] = struct{}{}
	}

	seen := make(map[string]struct{}, len(names))
	resolved := make([]string, 0, len(names))
	for _, raw := range names {
		labelName := strings.TrimSpace(raw)
		if labelName == "" {
			return nil, nil, nil, httpapi.Validation("body.labels", "label names must not be empty")
		}
		if _, ok := seen[labelName]; ok {
			return nil, nil, nil, httpapi.Validation(
				"body.labels", fmt.Sprintf("duplicate label %q", labelName),
			)
		}
		if _, ok := catalogByName[labelName]; !ok {
			return nil, nil, nil, httpapi.NewProblem(
				http.StatusBadRequest,
				httpapi.CodeValidationError,
				fmt.Sprintf("label %q is not in the repository label catalog", labelName),
				map[string]any{"field": "body.labels", "label": labelName},
			)
		}
		seen[labelName] = struct{}{}
		resolved = append(resolved, labelName)
	}
	return repo, mr, resolved, nil
}

func labelCatalogStale(freshness db.LabelCatalogFreshness, now time.Time) bool {
	if freshness.CheckedAt == nil {
		return true
	}
	return freshness.CheckedAt.Before(now.Add(-10 * time.Minute))
}
