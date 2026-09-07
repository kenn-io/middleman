package gitealike

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

type replaceLabelsCall struct {
	number   int
	labelIDs []int64
}

type fakeLabelTransport struct {
	*fakeTransport
	labels       [][]LabelDTO
	labelErr     error
	labelPages   []int
	replaced     []LabelDTO
	replaceErr   error
	replaceCalls []replaceLabelsCall
}

func (t *fakeLabelTransport) ListRepoLabels(
	_ context.Context,
	_ platform.RepoRef,
	opts PageOptions,
) ([]LabelDTO, Page, error) {
	t.labelPages = append(t.labelPages, opts.Page)
	if t.labelErr != nil {
		return nil, Page{}, t.labelErr
	}
	return pageFor(t.labels, opts.Page)
}

func (t *fakeLabelTransport) ReplaceIssueLabels(
	_ context.Context,
	_ platform.RepoRef,
	number int,
	labelIDs []int64,
) ([]LabelDTO, error) {
	t.replaceCalls = append(t.replaceCalls, replaceLabelsCall{number: number, labelIDs: labelIDs})
	if t.replaceErr != nil {
		return nil, t.replaceErr
	}
	return t.replaced, nil
}

func labelTestRef() platform.RepoRef {
	return platform.RepoRef{
		Platform: platform.KindForgejo,
		Host:     "codeberg.org",
		Owner:    "acme",
		Name:     "widget",
		RepoPath: "acme/widget",
	}
}

func TestProviderCapabilitiesAdvertiseLabelsOnlyWithLabelTransport(t *testing.T) {
	tests := []struct {
		name          string
		transport     Transport
		opts          []Option
		readLabels    bool
		labelMutation bool
	}{
		{
			name:      "plain transport never advertises labels",
			transport: &fakeTransport{},
			opts:      []Option{WithMutations()},
		},
		{
			name:       "label transport without mutations reads only",
			transport:  &fakeLabelTransport{fakeTransport: &fakeTransport{}},
			readLabels: true,
		},
		{
			name:          "label transport with mutations reads and writes",
			transport:     &fakeLabelTransport{fakeTransport: &fakeTransport{}},
			opts:          []Option{WithMutations()},
			readLabels:    true,
			labelMutation: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			provider := NewProvider(platform.KindForgejo, "codeberg.org", tt.transport, tt.opts...)
			caps := provider.Capabilities()
			assert.Equal(tt.readLabels, caps.ReadLabels)
			assert.Equal(tt.labelMutation, caps.LabelMutation)
		})
	}
}

func TestProviderListLabelsCollectsPagesAndNormalizes(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	transport := &fakeLabelTransport{
		fakeTransport: &fakeTransport{},
		labels: [][]LabelDTO{
			{{ID: 1, Name: "bug", Description: "Something is broken", Color: "d73a4a"}},
			{{ID: 2, Name: "triage", Color: "fbca04"}},
		},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport)

	catalog, err := provider.ListLabels(t.Context(), labelTestRef())
	require.NoError(err)
	require.Len(catalog.Labels, 2)
	assert.Equal([]int{1, 2}, transport.labelPages)
	assert.Equal("bug", catalog.Labels[0].Name)
	assert.Equal("Something is broken", catalog.Labels[0].Description)
	assert.Equal("d73a4a", catalog.Labels[0].Color)
	assert.Equal(int64(1), catalog.Labels[0].PlatformID)
	assert.Equal("1", catalog.Labels[0].PlatformExternalID)
	assert.Equal("triage", catalog.Labels[1].Name)
}

func TestProviderListLabelsWithoutLabelTransportIsUnsupported(t *testing.T) {
	provider := NewProvider(platform.KindForgejo, "codeberg.org", &fakeTransport{})

	_, err := provider.ListLabels(t.Context(), labelTestRef())

	Require.ErrorIs(t, err, platform.ErrUnsupportedCapability)
}

func TestProviderSetLabelsResolvesNamesToIDs(t *testing.T) {
	tests := []struct {
		name string
		set  func(*Provider, platform.RepoRef) ([]platform.Label, error)
	}{
		{
			name: "merge request",
			set: func(p *Provider, ref platform.RepoRef) ([]platform.Label, error) {
				return p.SetMergeRequestLabels(t.Context(), ref, 7, []string{"bug", "triage"})
			},
		},
		{
			name: "issue",
			set: func(p *Provider, ref platform.RepoRef) ([]platform.Label, error) {
				return p.SetIssueLabels(t.Context(), ref, 7, []string{"bug", "triage"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := Require.New(t)
			assert := assert.New(t)
			transport := &fakeLabelTransport{
				fakeTransport: &fakeTransport{},
				labels: [][]LabelDTO{{
					{ID: 11, Name: "bug", Color: "d73a4a"},
					{ID: 12, Name: "triage", Color: "fbca04"},
					{ID: 13, Name: "wontfix", Color: "ffffff"},
				}},
				replaced: []LabelDTO{
					{ID: 11, Name: "bug", Color: "d73a4a"},
					{ID: 12, Name: "triage", Color: "fbca04"},
				},
			}
			provider := NewProvider(platform.KindForgejo, "codeberg.org", transport, WithMutations())

			labels, err := tt.set(provider, labelTestRef())
			require.NoError(err)
			require.Len(transport.replaceCalls, 1)
			assert.Equal(7, transport.replaceCalls[0].number)
			assert.Equal([]int64{11, 12}, transport.replaceCalls[0].labelIDs)
			require.Len(labels, 2)
			assert.Equal("bug", labels[0].Name)
			assert.Equal("triage", labels[1].Name)
		})
	}
}

func TestProviderSetLabelsClearsWithEmptyNames(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	transport := &fakeLabelTransport{
		fakeTransport: &fakeTransport{},
		labels:        [][]LabelDTO{{{ID: 11, Name: "bug"}}},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport, WithMutations())

	labels, err := provider.SetMergeRequestLabels(t.Context(), labelTestRef(), 7, nil)
	require.NoError(err)
	assert.Empty(labels)
	require.Len(transport.replaceCalls, 1)
	assert.Empty(transport.replaceCalls[0].labelIDs)
}

func TestProviderSetLabelsFailsWhenNameMissingUpstream(t *testing.T) {
	require := Require.New(t)
	assert := assert.New(t)
	transport := &fakeLabelTransport{
		fakeTransport: &fakeTransport{},
		labels:        [][]LabelDTO{{{ID: 11, Name: "bug"}}},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport, WithMutations())

	_, err := provider.SetIssueLabels(t.Context(), labelTestRef(), 7, []string{"ghost"})

	require.ErrorIs(err, platform.ErrNotFound)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(platform.KindForgejo, platformErr.Provider)
	assert.Equal("codeberg.org", platformErr.PlatformHost)
	assert.Contains(platformErr.Error(), `label "ghost" not found`)
	assert.Empty(transport.replaceCalls)
}

func TestProviderSetLabelsWithoutMutationsIsUnsupported(t *testing.T) {
	transport := &fakeLabelTransport{
		fakeTransport: &fakeTransport{},
		labels:        [][]LabelDTO{{{ID: 11, Name: "bug"}}},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport)

	_, err := provider.SetMergeRequestLabels(t.Context(), labelTestRef(), 7, []string{"bug"})

	Require.ErrorIs(t, err, platform.ErrUnsupportedCapability)
	assert.Empty(t, transport.replaceCalls)
}

func TestProviderSetLabelsMapsTransportErrors(t *testing.T) {
	require := Require.New(t)
	transport := &fakeLabelTransport{
		fakeTransport: &fakeTransport{},
		labels:        [][]LabelDTO{{{ID: 11, Name: "bug"}}},
		replaceErr:    &HTTPError{StatusCode: 403},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport, WithMutations())

	_, err := provider.SetIssueLabels(t.Context(), labelTestRef(), 7, []string{"bug"})

	require.ErrorIs(err, platform.ErrPermissionDenied)
}
