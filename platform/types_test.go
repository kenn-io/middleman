package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryFeatureEnabled(t *testing.T) {
	t.Parallel()
	issuesEnabled := false
	mergeRequestsEnabled := true
	repo := Repository{Features: RepositoryFeatures{
		IssuesEnabled:        &issuesEnabled,
		MergeRequestsEnabled: &mergeRequestsEnabled,
	}}

	tests := []struct {
		name    string
		feature string
		enabled bool
		known   bool
	}{
		{name: "issues disabled", feature: RepositoryFeatureIssues, known: true},
		{name: "merge requests enabled", feature: RepositoryFeatureMergeRequests, enabled: true, known: true},
		{name: "unsupported feature", feature: "releases"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			enabled, known := repo.FeatureEnabled(tt.feature)
			assert.Equal(t, tt.enabled, enabled)
			assert.Equal(t, tt.known, known)
		})
	}

	_, known := (Repository{}).FeatureEnabled(RepositoryFeatureIssues)
	assert.False(t, known)
}

func TestArchiveCapabilitiesSupport(t *testing.T) {
	caps := ArchiveCapabilities{
		HistoricalIssues:        true,
		HistoricalMergeRequests: false,
		OrdinaryComments:        true,
		SubmittedReviews:        false,
		InlineReviewComments:    true,
	}
	tests := []struct {
		name       string
		capability ArchiveCapability
		want       ArchiveCapabilitySupport
	}{
		{
			name:       "historical issues",
			capability: ArchiveCapabilityHistoricalIssues,
			want:       ArchiveCapabilitySupported,
		},
		{
			name:       "historical merge requests",
			capability: ArchiveCapabilityHistoricalMergeRequests,
			want:       ArchiveCapabilityUnsupported,
		},
		{
			name:       "ordinary comments",
			capability: ArchiveCapabilityOrdinaryComments,
			want:       ArchiveCapabilitySupported,
		},
		{
			name:       "submitted reviews",
			capability: ArchiveCapabilitySubmittedReviews,
			want:       ArchiveCapabilityUnsupported,
		},
		{
			name:       "inline review comments",
			capability: ArchiveCapabilityInlineReviewComments,
			want:       ArchiveCapabilitySupported,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := caps.Support(tc.capability)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestArchiveCapabilitiesRejectUnknownCapability(t *testing.T) {
	caps := ArchiveCapabilities{}
	unknown := ArchiveCapability("unknown")

	_, err := caps.Support(unknown)
	require.ErrorIs(t, err, ErrInvalidArgument)

	err = caps.Require(KindGitLab, "gitlab.example.com", unknown)
	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	assert := assert.New(t)
	assert.Equal(ErrCodeInvalidArgument, platformErr.Code)
	assert.Equal("archive_capability", platformErr.Field)
}

func TestArchiveCapabilitiesRequireUnsupportedCapability(t *testing.T) {
	err := (ArchiveCapabilities{}).Require(
		KindGitLab,
		"gitlab.example.com",
		ArchiveCapabilitySubmittedReviews,
	)
	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrUnsupportedCapability)
	assert := assert.New(t)
	assert.Equal(ErrCodeUnsupportedCapability, platformErr.Code)
	assert.Equal(KindGitLab, platformErr.Provider)
	assert.Equal("gitlab.example.com", platformErr.PlatformHost)
	assert.Equal(string(ArchiveCapabilitySubmittedReviews), platformErr.Capability)
}

func TestArchiveCapabilitiesHasHistoricalInventory(t *testing.T) {
	tests := []struct {
		name string
		caps ArchiveCapabilities
		want bool
	}{
		{name: "zero value"},
		{
			name: "historical issues",
			caps: ArchiveCapabilities{HistoricalIssues: true},
			want: true,
		},
		{
			name: "historical merge requests",
			caps: ArchiveCapabilities{HistoricalMergeRequests: true},
			want: true,
		},
		{
			name: "both inventories",
			caps: ArchiveCapabilities{
				HistoricalIssues:        true,
				HistoricalMergeRequests: true,
			},
			want: true,
		},
		{
			name: "non-inventory capabilities",
			caps: ArchiveCapabilities{
				OrdinaryComments:     true,
				SubmittedReviews:     true,
				InlineReviewComments: true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.caps.HasHistoricalInventory())
		})
	}
}

func TestValidatePageAcceptsBoundedTerminationShapes(t *testing.T) {
	tests := []struct {
		name string
		page Page[int]
	}{
		{
			name: "next cursor with items",
			page: Page[int]{Items: []int{0}, NextCursor: "opaque:cursor:value"},
		},
		{
			name: "opaque whitespace cursor",
			page: Page[int]{Items: []int{1}, NextCursor: " \t"},
		},
		{
			name: "exhausted empty page",
			page: Page[int]{Exhausted: true},
		},
		{
			name: "exhausted page with items",
			page: Page[int]{Items: []int{1}, Exhausted: true},
		},
		{
			name: "filtered progress with advancing cursor",
			page: Page[int]{NextCursor: "next", ProgressOnly: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, ValidatePage(KindGitLab, "gitlab.example.com", "", tc.page))
		})
	}
}

func TestValidatePageRejectsInvalidTerminationShapes(t *testing.T) {
	tests := []struct {
		name string
		page Page[int]
	}{
		{
			name: "cursor and exhaustion",
			page: Page[int]{Items: []int{1}, NextCursor: "next", Exhausted: true},
		},
		{
			name: "neither cursor nor exhaustion",
			page: Page[int]{Items: []int{1}},
		},
		{
			name: "empty page without filtered progress",
			page: Page[int]{NextCursor: "next"},
		},
		{
			name: "progress page with items",
			page: Page[int]{Items: []int{1}, NextCursor: "next", ProgressOnly: true},
		},
		{
			name: "exhausted progress page",
			page: Page[int]{Exhausted: true, ProgressOnly: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePage(KindGitLab, "gitlab.example.com", "", tc.page)
			assertArchiveContractError(t, err, "archive_page")
		})
	}
}

func TestValidatePageRejectsRepeatedOpaqueCursor(t *testing.T) {
	for _, cursor := range []string{"opaque:cursor", " \t"} {
		t.Run(cursor, func(t *testing.T) {
			err := ValidatePage(
				KindGitLab,
				"gitlab.example.com",
				cursor,
				Page[int]{Items: []int{1}, NextCursor: cursor},
			)
			assertArchiveContractError(t, err, "archive_page_cursor")
		})
	}
}

func assertArchiveContractError(t *testing.T, err error, field string) {
	t.Helper()
	var platformErr *Error
	require.ErrorAs(t, err, &platformErr)
	require.ErrorIs(t, err, ErrProviderContract)
	require.NotErrorIs(t, err, ErrInvalidRepoRef)
	assert := assert.New(t)
	assert.Equal(ErrCodeProviderContract, platformErr.Code)
	assert.Equal(KindGitLab, platformErr.Provider)
	assert.Equal("gitlab.example.com", platformErr.PlatformHost)
	assert.Equal(field, platformErr.Field)
}
