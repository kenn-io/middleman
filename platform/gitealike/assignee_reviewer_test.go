package gitealike

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
)

// reviewRequestFakeTransport extends fakeTransport with the optional
// review-request surface plus controllable pull request and review
// read-backs.
type reviewRequestFakeTransport struct {
	*fakeTransport
	currentPR      PullRequestDTO
	reviews        [][]ReviewDTO
	reviewRequests []string
}

func (t *reviewRequestFakeTransport) GetPullRequest(context.Context, platform.RepoRef, int) (PullRequestDTO, error) {
	return t.currentPR, nil
}

func (t *reviewRequestFakeTransport) ListPullRequestReviews(_ context.Context, _ platform.RepoRef, _ int, opts PageOptions) ([]ReviewDTO, Page, error) {
	return pageFor(t.reviews, opts.Page)
}

func (t *reviewRequestFakeTransport) CreateReviewRequests(_ context.Context, _ platform.RepoRef, _ int, reviewers []string) error {
	t.reviewRequests = append(t.reviewRequests, "create:"+joinNames(reviewers))
	return nil
}

func (t *reviewRequestFakeTransport) DeleteReviewRequests(_ context.Context, _ platform.RepoRef, _ int, reviewers []string) error {
	t.reviewRequests = append(t.reviewRequests, "delete:"+joinNames(reviewers))
	return nil
}

func joinNames(names []string) string {
	var out strings.Builder
	for i, name := range names {
		if i > 0 {
			out.WriteString(",")
		}
		out.WriteString(name)
	}
	return out.String()
}

func TestProviderCapabilitiesAdvertiseReviewerMutationWithReviewRequestTransport(t *testing.T) {
	assert := assert.New(t)
	provider := NewProvider(
		platform.KindGitea,
		"gitea.example.com",
		&reviewRequestFakeTransport{fakeTransport: &fakeTransport{}},
		WithMutations(),
	)

	caps := provider.Capabilities()
	assert.True(caps.AssigneeMutation)
	assert.True(caps.ReviewerMutation)
}

func TestProviderSetAssigneesReplacesSetThroughEditOptions(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	ref := platform.RepoRef{Platform: platform.KindGitea, Host: "gitea.example.com", Owner: "acme", Name: "widget"}
	transport := &fakeTransport{
		pr: PullRequestDTO{
			Index:     7,
			Assignees: []UserDTO{{UserName: "alice"}, {UserName: "bob"}},
		},
		issue: IssueDTO{
			Index:     8,
			Assignees: []UserDTO{{UserName: "carol"}},
		},
	}
	provider := NewProvider(platform.KindGitea, "gitea.example.com", transport, WithMutations())

	prAssignees, err := provider.SetMergeRequestAssignees(context.Background(), ref, 7, []string{"alice", "bob"})
	require.NoError(err)
	assert.Equal([]string{"alice", "bob"}, prAssignees)

	issueAssignees, err := provider.SetIssueAssignees(context.Background(), ref, 8, []string{"carol"})
	require.NoError(err)
	assert.Equal([]string{"carol"}, issueAssignees)

	assert.Equal([]string{"edit_pull:", "edit_issue:"}, transport.mutationCalls)
}

func TestProviderSetAssigneesWithoutMutationsReturnsUnsupportedCapability(t *testing.T) {
	require := Require.New(t)
	ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "codeberg.org", Owner: "acme", Name: "widget"}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", &fakeTransport{})

	_, err := provider.SetMergeRequestAssignees(context.Background(), ref, 7, []string{"alice"})
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	require.Equal(platform.ErrCodeUnsupportedCapability, platformErr.Code)
}

func TestProviderReviewerMutationsReadBackFromPullRequestField(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	ref := platform.RepoRef{Platform: platform.KindGitea, Host: "gitea.example.com", Owner: "acme", Name: "widget"}
	transport := &reviewRequestFakeTransport{
		fakeTransport: &fakeTransport{},
		// Gitea-style read-back: the pull request carries a
		// provider-confirmed requested-reviewer set.
		currentPR: PullRequestDTO{
			Index:              7,
			RequestedReviewers: []UserDTO{{UserName: "alice"}, {UserName: "bob"}},
		},
	}
	provider := NewProvider(platform.KindGitea, "gitea.example.com", transport, WithMutations())

	requested, err := provider.RequestMergeRequestReviewers(context.Background(), ref, 7, []string{"bob"})
	require.NoError(err)
	assert.Equal([]string{"alice", "bob"}, requested)

	removed, err := provider.RemoveMergeRequestReviewers(context.Background(), ref, 7, []string{"carol"})
	require.NoError(err)
	assert.Equal([]string{"alice", "bob"}, removed)

	assert.Equal([]string{"create:bob", "delete:carol"}, transport.reviewRequests)
}

func TestProviderRequestReviewersWithEmptyListReadsWithoutMutating(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	ref := platform.RepoRef{Platform: platform.KindGitea, Host: "gitea.example.com", Owner: "acme", Name: "widget"}
	transport := &reviewRequestFakeTransport{
		fakeTransport: &fakeTransport{},
		currentPR: PullRequestDTO{
			Index:              7,
			RequestedReviewers: []UserDTO{{UserName: "carol"}},
		},
	}
	provider := NewProvider(platform.KindGitea, "gitea.example.com", transport, WithMutations())

	current, err := provider.RequestMergeRequestReviewers(context.Background(), ref, 7, nil)
	require.NoError(err)
	assert.Equal([]string{"carol"}, current)
	assert.Empty(transport.reviewRequests, "an empty request must not call the review-request endpoint")
}

func TestProviderReviewerMutationsDeriveFromReviewsWhenFieldUnknown(t *testing.T) {
	assert := assert.New(t)
	require := Require.New(t)
	ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "codeberg.org", Owner: "acme", Name: "widget"}
	transport := &reviewRequestFakeTransport{
		fakeTransport: &fakeTransport{},
		// Forgejo-style read-back: the SDK pull request lacks the
		// requested-reviewers field, so pending requests come from
		// REQUEST_REVIEW review rows.
		currentPR: PullRequestDTO{Index: 7},
		reviews: [][]ReviewDTO{{
			{ID: 1, User: UserDTO{UserName: "alice"}, State: "REQUEST_REVIEW"},
			{ID: 2, User: UserDTO{UserName: "bob"}, State: "APPROVED"},
			{ID: 3, User: UserDTO{UserName: "alice"}, State: "REQUEST_REVIEW"},
		}},
	}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", transport, WithMutations())

	requested, err := provider.RequestMergeRequestReviewers(context.Background(), ref, 7, []string{"alice"})
	require.NoError(err)
	assert.Equal([]string{"alice"}, requested)
}

func TestProviderReviewerMutationsWithoutTransportReturnUnsupportedCapability(t *testing.T) {
	require := Require.New(t)
	ref := platform.RepoRef{Platform: platform.KindForgejo, Host: "codeberg.org", Owner: "acme", Name: "widget"}
	provider := NewProvider(platform.KindForgejo, "codeberg.org", &fakeTransport{}, WithMutations())

	_, err := provider.RequestMergeRequestReviewers(context.Background(), ref, 7, []string{"alice"})
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	require.Equal(platform.ErrCodeUnsupportedCapability, platformErr.Code)
	require.Equal("reviewer_mutation", platformErr.Capability)
}

func TestNormalizePullRequestMapsAssigneesAndRequestedReviewers(t *testing.T) {
	assert := assert.New(t)
	ref := platform.RepoRef{Platform: platform.KindGitea, Host: "gitea.example.com", Owner: "acme", Name: "widget"}

	known := NormalizePullRequest(ref, PullRequestDTO{
		Index:              7,
		Assignees:          []UserDTO{{UserName: "alice"}},
		RequestedReviewers: []UserDTO{},
	})
	assert.Equal([]string{"alice"}, known.Assignees)
	assert.NotNil(known.RequestedReviewers)
	assert.Empty(known.RequestedReviewers)

	unknown := NormalizePullRequest(ref, PullRequestDTO{Index: 8})
	assert.Nil(unknown.Assignees)
	assert.Nil(unknown.RequestedReviewers)
}
