package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

type routeRecordingClient struct {
	Client
	marker         string
	calls          []string
	snapshot       *platformgithub.RateLimitSnapshot
	snapshotErr    error
	snapshotSource tokenauth.Source
	snapshotToken  string
	userSource     tokenauth.Source
	userToken      string
}

func (c *routeRecordingClient) GetRepository(
	_ context.Context, owner, repo string,
) (*gh.Repository, error) {
	c.calls = append(c.calls, "get:"+owner+"/"+repo)
	return &gh.Repository{Name: new(c.marker)}, nil
}

func (c *routeRecordingClient) ListInventoryIssuesPage(
	_ context.Context, owner, repo, sortBy, cursor, since string,
) ([]*gh.Issue, string, bool, error) {
	c.calls = append(c.calls, "inventory-issues:"+owner+"/"+repo+":"+sortBy+":"+cursor+":"+since)
	return []*gh.Issue{{Title: new(c.marker)}}, "next-" + c.marker, false, nil
}

func (c *routeRecordingClient) ListInventoryPullRequestsPage(
	_ context.Context, owner, repo, sortBy string, page int,
) ([]*gh.PullRequest, bool, error) {
	c.calls = append(c.calls, "inventory-pulls:"+owner+"/"+repo+":"+sortBy+":"+strconv.Itoa(page))
	return []*gh.PullRequest{{Title: new(c.marker)}}, true, nil
}

func (c *routeRecordingClient) GetMarkdownImage(
	_ context.Context, owner, repo, sourceURL string,
) (platform.MarkdownImage, error) {
	c.calls = append(c.calls, "markdown-image:"+owner+"/"+repo+":"+sourceURL)
	return platform.MarkdownImage{
		Content: []byte(c.marker), ContentType: "image/png",
	}, nil
}

func (c *routeRecordingClient) GetUser(
	ctx context.Context, login string,
) (*gh.User, error) {
	c.calls = append(c.calls, "user:"+login)
	if c.userSource != nil {
		token, err := c.userSource.Token(ctx)
		if err != nil {
			return nil, err
		}
		c.userToken = token
	}
	name := c.marker + " display"
	return &gh.User{Login: new(login), Name: new(name)}, nil
}

func (c *routeRecordingClient) ListRepositoriesByOwner(
	_ context.Context, owner string,
) ([]*gh.Repository, error) {
	c.calls = append(c.calls, "list-owner:"+owner)
	return []*gh.Repository{{Name: new(c.marker)}}, nil
}

func (c *routeRecordingClient) CreateIssue(
	_ context.Context, owner, repo, _, _ string,
) (*gh.Issue, error) {
	c.calls = append(c.calls, "create-issue:"+owner+"/"+repo)
	return &gh.Issue{Title: new(c.marker)}, nil
}

func (c *routeRecordingClient) ListRepoLabels(
	_ context.Context, owner, repo string,
) ([]*gh.Label, error) {
	c.calls = append(c.calls, "list-labels:"+owner+"/"+repo)
	return []*gh.Label{{Name: new(c.marker)}}, nil
}

func (c *routeRecordingClient) ReplaceIssueLabels(
	_ context.Context, owner, repo string, _ int, _ []string,
) ([]*gh.Label, error) {
	c.calls = append(c.calls, "labels:"+owner+"/"+repo)
	return []*gh.Label{{Name: new(c.marker)}}, nil
}

func (c *routeRecordingClient) AuthenticatedViewerLogin(context.Context) (string, error) {
	c.calls = append(c.calls, "viewer")
	return c.marker, nil
}

func (c *routeRecordingClient) AuthenticatedViewerCacheKey() string {
	return "viewer:" + c.marker
}

func (c *routeRecordingClient) GetNotificationThread(
	_ context.Context, threadID string,
) (NotificationThread, error) {
	c.calls = append(c.calls, "thread:"+threadID)
	return NotificationThread{ID: threadID}, nil
}

func (c *routeRecordingClient) MarkNotificationThreadRead(
	_ context.Context, threadID string,
) error {
	c.calls = append(c.calls, "mark-read:"+threadID)
	return nil
}

func (c *routeRecordingClient) GetRateLimitSnapshot(ctx context.Context) (*platformgithub.RateLimitSnapshot, error) {
	c.calls = append(c.calls, "snapshot")
	if c.snapshotSource != nil {
		token, err := c.snapshotSource.Token(ctx)
		if err != nil {
			return nil, err
		}
		c.snapshotToken = token
	}
	if c.snapshotErr != nil {
		return nil, c.snapshotErr
	}
	if c.snapshot != nil {
		return c.snapshot, nil
	}
	return &platformgithub.RateLimitSnapshot{}, nil
}

func TestGitHubProviderPreservesRepositoryAwareNotificationRouting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fallback := &routeRecordingClient{marker: "fallback"}
	owner := &routeRecordingClient{marker: "owner"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: fallback},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: owner},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	provider := newTestGitHubProvider(t, "github.com", routed)

	getter, ok := any(provider).(routedNotificationThreadGetter)
	require.True(ok, "the platform wrapper must preserve repository-aware thread reads")
	_, err = getter.GetNotificationThreadForRepo(t.Context(), "acme", "widget", "thread-1")
	require.NoError(err)
	marker, ok := any(provider).(routedNotificationReadMarker)
	require.True(ok, "the platform wrapper must preserve repository-aware read markers")
	require.NoError(marker.MarkNotificationThreadReadForRepo(
		t.Context(), "acme", "widget", "thread-1",
	))

	assert.Equal([]string{"thread:thread-1", "mark-read:thread-1"}, owner.calls)
	assert.Empty(fallback.calls)
}

func TestGitHubProviderRoutesMarkdownImagesByRepositoryCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const source = "https://github.com/user-attachments/assets/1111"
	fallback := &routeRecordingClient{marker: "fallback"}
	owner := &routeRecordingClient{marker: "owner"}
	exact := &routeRecordingClient{marker: "exact"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: fallback},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: owner},
		&Route{
			Key:    RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			Client: exact,
		},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	provider := newTestGitHubProvider(t, "github.com", routed)

	require.True(provider.Capabilities().ReadMarkdownImages,
		"a routed GitHub host must keep private markdown image previews")

	image, err := provider.GetMarkdownImage(t.Context(), platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}, source)
	require.NoError(err)

	assert.Equal("exact", string(image.Content))
	assert.Equal([]string{"markdown-image:acme/widget:" + source}, exact.calls)
	assert.Empty(owner.calls)
	assert.Empty(fallback.calls)
}

// Display-name enrichment runs inside repository sync, so it must use the
// repository's credential. An owner-scoped or App-only configuration has no
// host fallback route, and billing the lookup to a fallback identity would also
// spend the wrong budget and skip repository provider-work accounting.
func TestDisplayNameLookupUsesRepositoryCredentialWithoutHostFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	owner := &routeRecordingClient{marker: "owner"}
	exact := &routeRecordingClient{marker: "exact"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: owner},
		&Route{
			Key:    RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			Client: exact,
		},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	syncer := NewSyncer(
		map[string]Client{"github.com": routed}, openTestDB(t), nil, nil,
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	name, ok := syncer.resolveDisplayName(t.Context(), routed, RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget",
	}, "octocat")

	require.True(ok, "an owner/App-only configuration still enriches authors")
	assert.Equal("exact display", name)
	assert.Equal([]string{"user:octocat"}, exact.calls)
	assert.Empty(owner.calls,
		"the repository's own credential pays for the lookup")
}

// An App-backed repository route must mint its installation token for the
// user lookup. `/users/{login}` carries no owner in its path, so the transport
// derives none and the App candidate is skipped for the PAT unless the caller
// supplies owner context — silently spending the user's budget for a read the
// route's tracker bills to the installation.
func TestRepositoryRoutedUserLookupUsesAppInstallationToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("USER_LOOKUP_PAT", "user-token")
	source := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{
			Platform: "github", Host: "github.com", Scope: "repo:acme/widget",
		},
		Candidates: []tokenauth.Candidate{
			{
				Kind: tokenauth.SourceKindGitHubApp, Host: "github.com",
				AppID: 1, InstallationID: 2, InstallationAccount: "acme",
			},
			{Kind: tokenauth.SourceKindEnv, EnvName: "USER_LOOKUP_PAT"},
		},
	}, tokenauth.Options{GitHubApp: func(
		context.Context, tokenauth.Candidate,
	) (string, time.Time, error) {
		return "installation-token", time.Now().Add(time.Hour), nil
	}})
	exact := &routeRecordingClient{marker: "exact", userSource: source}
	router, err := NewHostRouter("github.com", &Route{
		Key:    RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
		Client: exact,
		ReadIdentity: IdentityKey{
			Host: "github.com", Principal: "installation:2",
		},
	})
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)

	user, err := routed.GetUserForRepo(t.Context(), "acme", "widget", "octocat")
	require.NoError(err)

	require.NotNil(user)
	assert.Equal("installation-token", exact.userToken,
		"the installation the route bills is the one that pays for the read")
}

func TestSyncerRateSnapshotScopesAppRouteToOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("SNAPSHOT_PAT", "user-token")
	source := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.com", Scope: "owner:acme"},
		Candidates: []tokenauth.Candidate{
			{
				Kind: tokenauth.SourceKindGitHubApp, Host: "github.com",
				AppID: 1, InstallationID: 2, InstallationAccount: "acme",
			},
			{Kind: tokenauth.SourceKindEnv, EnvName: "SNAPSHOT_PAT"},
		},
	}, tokenauth.Options{GitHubApp: func(
		context.Context, tokenauth.Candidate,
	) (string, time.Time, error) {
		return "installation-token", time.Now().Add(time.Hour), nil
	}})
	database := openTestDB(t)
	tracker := NewRateTracker(database, "github.com", "installation:2", "rest")
	client := &routeRecordingClient{
		snapshotSource: source,
		snapshot:       &platformgithub.RateLimitSnapshot{Core: &Rate{Limit: 5000, Remaining: 4000}},
	}
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		ReadIdentity: IdentityKey{Host: "github.com", Principal: "installation:2"},
	})
	require.NoError(err)
	syncer := &Syncer{
		clients: registryFromGitHubClients(map[string]Client{"github.com": client}),
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "installation:2"): tracker,
		},
		rateLimitSnapshotRefresh: make(map[string]time.Time),
	}

	syncer.RefreshRateLimitSnapshots(t.Context())

	assert.Equal("installation-token", client.snapshotToken)
	assert.Equal(4000, tracker.Remaining())
}

func (c *routeRecordingClient) InstallationAuthenticationActive() bool {
	return c.marker == "fallback"
}

func (c *routeRecordingClient) ListNotifications(
	_ context.Context, _ NotificationListOptions,
) ([]NotificationThread, bool, error) {
	c.calls = append(c.calls, "notifications")
	return nil, false, nil
}

func TestHostRouterSelectsExactOwnerAndFallbackRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fallback := &Route{
		Key:          RouteKey{Host: "github.com"},
		ReadIdentity: IdentityKey{Host: "github.com", Principal: "user:1"},
	}
	owner := &Route{
		Key:          RouteKey{Host: "github.com", Owner: "Acme"},
		ReadIdentity: IdentityKey{Host: "github.com", Principal: "user:2"},
	}
	exact := &Route{
		Key:          RouteKey{Host: "github.com", Owner: "Acme", Name: "Widget"},
		ReadIdentity: IdentityKey{Host: "github.com", Principal: "user:3"},
	}
	router, err := NewHostRouter("github.com", fallback, owner, exact)
	require.NoError(err)

	got, err := router.RouteForRepo("ACME", "WIDGET")
	require.NoError(err)
	assert.Same(exact, got)
	got, err = router.RouteForRepo("acme", "other")
	require.NoError(err)
	assert.Same(owner, got)
	got, err = router.RouteForOwner("unknown")
	require.NoError(err)
	assert.Same(fallback, got)
	got, err = router.Fallback()
	require.NoError(err)
	assert.Same(fallback, got)

	identity, err := router.ReadIdentityForRepo("acme", "widget")
	require.NoError(err)
	assert.Equal("user:3", identity.Principal)
}

func TestHostRouterRejectsConflictingRouteHostWithoutMutation(t *testing.T) {
	route := &Route{Key: RouteKey{Host: "github.com", Owner: "acme"}}

	_, err := NewHostRouter("ghe.example.com", route)

	require.Error(t, err)
	assert.Equal(t, "github.com", route.Key.Host)
}

func TestHostRouterReturnsSafeMissingRouteError(t *testing.T) {
	require := require.New(t)
	router, err := NewHostRouter("ghe.example.com", nil)
	require.NoError(err)

	_, err = router.RouteForOwner("private-org")

	require.Error(err)
	require.ErrorContains(err, "ghe.example.com")
	require.ErrorContains(err, "private-org")
}

// routedNativeStackClient is a route client that also serves the preview stack
// surface, so the test can prove routing reaches it instead of silently losing
// it to a failed type assertion.
type routedNativeStackClient struct {
	*routeRecordingClient
	hints map[int]*platformgithub.NativeStackHint
	page  platformgithub.NativeStackPage
}

func (c *routedNativeStackClient) ListOpenPullRequestsWithNativeStackHints(
	_ context.Context, owner, repo string,
) ([]*gh.PullRequest, map[int]*platformgithub.NativeStackHint, error) {
	c.calls = append(c.calls, "native-hints:"+owner+"/"+repo)
	return nil, c.hints, nil
}

func (c *routedNativeStackClient) ListNativeStacksPage(
	_ context.Context, owner, repo string, page int,
) (platformgithub.NativeStackPage, error) {
	c.calls = append(c.calls, fmt.Sprintf("native-page:%s/%s:%d", owner, repo, page))
	return c.page, nil
}

func TestRoutedClientServesNativeStacksThroughSelectedRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	hints := map[int]*platformgithub.NativeStackHint{
		101: {Number: 42, Size: 2, Position: 1, BaseRef: "main"},
	}
	ownerClient := &routedNativeStackClient{
		routeRecordingClient: &routeRecordingClient{marker: "owner"},
		hints:                hints,
		page: platformgithub.NativeStackPage{Stacks: []platformgithub.NativeStack{{
			ID: 9001, Number: 42, BaseRef: "main", Open: true,
		}}},
	}
	fallbackClient := &routeRecordingClient{marker: "fallback"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: fallbackClient},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: ownerClient},
	)
	require.NoError(err)
	client, err := NewRoutedClient(router)
	require.NoError(err)

	native, ok := any(client).(platformgithub.NativeStackClient)
	require.True(ok, "a routed client must expose the preview stack surface")
	_, gotHints, err := native.ListOpenPullRequestsWithNativeStackHints(t.Context(), "acme", "widget")
	require.NoError(err)
	assert.Equal(hints, gotHints)
	page, err := native.ListNativeStacksPage(t.Context(), "acme", "widget", 1)
	require.NoError(err)
	require.Len(page.Stacks, 1)
	assert.Equal(42, page.Stacks[0].Number)
	assert.Equal(
		[]string{"native-hints:acme/widget", "native-page:acme/widget:1"},
		ownerClient.calls,
	)

	// A route whose client has no preview surface reports an unsupported
	// capability rather than pretending the repository has no stacks.
	_, err = native.ListNativeStacksPage(t.Context(), "other", "repo", 1)
	require.Error(err)
	assert.Contains(err.Error(), "read_native_stacks")
}

func TestRoutedClientDelegatesByRepositoryOwnerAndFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fallbackClient := &routeRecordingClient{marker: "fallback"}
	ownerClient := &routeRecordingClient{marker: "owner"}
	exactClient := &routeRecordingClient{marker: "exact"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: fallbackClient},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: ownerClient},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme", Name: "widget"}, Client: exactClient},
	)
	require.NoError(err)
	client, err := NewRoutedClient(router)
	require.NoError(err)

	repo, err := client.GetRepository(t.Context(), "ACME", "WIDGET")
	require.NoError(err)
	assert.Equal("exact", repo.GetName())
	repo, err = client.GetRepository(t.Context(), "acme", "other")
	require.NoError(err)
	assert.Equal("owner", repo.GetName())
	repo, err = client.GetRepository(t.Context(), "other", "repo")
	require.NoError(err)
	assert.Equal("fallback", repo.GetName())

	repos, err := client.ListRepositoriesByOwner(t.Context(), "Acme")
	require.NoError(err)
	assert.Equal("owner", repos[0].GetName())
	issue, err := client.CreateIssue(t.Context(), "acme", "other", "title", "body")
	require.NoError(err)
	assert.Equal("owner", issue.GetTitle())
	beforeNotifications := len(fallbackClient.calls)
	_, _, err = client.ListNotifications(t.Context(), NotificationListOptions{})
	require.NoError(err)
	assert.Equal("notifications", fallbackClient.calls[beforeNotifications])
	beforeOwnerNotifications := len(ownerClient.calls)
	_, _, err = client.ListNotifications(t.Context(), NotificationListOptions{
		RepoOwner: "acme", RepoName: "other",
	})
	require.NoError(err)
	assert.Equal("notifications", ownerClient.calls[beforeOwnerNotifications])
	labels, err := client.ReplaceIssueLabels(
		t.Context(), "acme", "other", 1, []string{"bug"},
	)
	require.NoError(err)
	assert.Equal("owner", labels[0].GetName())
	viewer, err := client.AuthenticatedViewerLogin(t.Context())
	require.NoError(err)
	assert.Equal("fallback", viewer)
	assert.Equal("viewer:fallback", client.AuthenticatedViewerCacheKey())
	routedViewer, err := client.AuthenticatedViewerLoginForRepo(
		t.Context(), "acme", "other",
	)
	require.NoError(err)
	assert.Equal("owner", routedViewer)
	assert.Equal(
		"viewer:owner",
		client.AuthenticatedViewerCacheKeyForRepo("acme", "other"),
	)
	thread, err := client.GetNotificationThread(t.Context(), "123")
	require.NoError(err)
	assert.Equal("123", thread.ID)
	routedThread, err := client.GetNotificationThreadForRepo(
		t.Context(), "acme", "other", "456",
	)
	require.NoError(err)
	assert.Equal("456", routedThread.ID)
	require.NoError(client.MarkNotificationThreadReadForRepo(
		t.Context(), "acme", "other", "456",
	))
	assert.Contains(ownerClient.calls, "thread:456")
	assert.Contains(ownerClient.calls, "mark-read:456")
	_, err = client.GetRateLimitSnapshot(t.Context())
	require.NoError(err)
	assert.True(client.InstallationAuthenticationActive())
}

func TestRoutedClientDiscoveryMergesPATAndSelectedAppResults(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	repoWithID := func(id int64, name string) *gh.Repository {
		return &gh.Repository{ID: &id, Name: &name}
	}
	listClient := func(repos ...*gh.Repository) *mockClient {
		return &mockClient{listReposByOwnerFn: func(context.Context, string) ([]*gh.Repository, error) {
			return repos, nil
		}}
	}
	newAppRoute := func(discovery Client) *Route {
		return &Route{
			Key:             RouteKey{Host: "github.com", Owner: "acme", Name: "covered"},
			Client:          &routeRecordingClient{marker: "app"},
			DiscoveryClient: discovery,
		}
	}
	repoNames := func(repos []*gh.Repository) []string {
		names := make([]string, 0, len(repos))
		for _, repo := range repos {
			names = append(names, repo.GetName())
		}
		return names
	}

	// A PAT sees repositories the selected installation does not cover and
	// vice versa: discovery must return the union, deduplicating overlap.
	router, err := NewHostRouter("github.com",
		newAppRoute(listClient(repoWithID(1, "covered"), repoWithID(2, "app-only"))),
		&Route{
			Key:    RouteKey{Host: "github.com", Owner: "acme"},
			Client: listClient(repoWithID(1, "covered"), repoWithID(3, "pat-only")),
		},
	)
	require.NoError(err)
	client, err := NewRoutedClient(router)
	require.NoError(err)
	repos, err := client.ListRepositoriesByOwner(t.Context(), "Acme")
	require.NoError(err)
	assert.Equal([]string{"covered", "pat-only", "app-only"}, repoNames(repos))

	// The same union applies over a host fallback PAT.
	router, err = NewHostRouter("github.com",
		newAppRoute(listClient(repoWithID(2, "app-only"))),
		&Route{
			Key:    RouteKey{Host: "github.com"},
			Client: listClient(repoWithID(3, "pat-only")),
		},
	)
	require.NoError(err)
	client, err = NewRoutedClient(router)
	require.NoError(err)
	repos, err = client.ListRepositoriesByOwner(t.Context(), "acme")
	require.NoError(err)
	assert.Equal([]string{"pat-only", "app-only"}, repoNames(repos))

	// A failure of either configured source fails discovery instead of
	// silently narrowing the result set.
	router, err = NewHostRouter("github.com",
		newAppRoute(&mockClient{listReposByOwnerFn: func(context.Context, string) ([]*gh.Repository, error) {
			return nil, errors.New("installation token expired")
		}}),
		&Route{
			Key:    RouteKey{Host: "github.com", Owner: "acme"},
			Client: listClient(repoWithID(3, "pat-only")),
		},
	)
	require.NoError(err)
	client, err = NewRoutedClient(router)
	require.NoError(err)
	_, err = client.ListRepositoriesByOwner(t.Context(), "acme")
	require.ErrorContains(err, "installation token expired")

	// With no PAT route at all, the selected-App discovery client serves the
	// owner alone.
	router, err = NewHostRouter("github.com",
		newAppRoute(listClient(repoWithID(2, "app-only"))),
	)
	require.NoError(err)
	client, err = NewRoutedClient(router)
	require.NoError(err)
	repos, err = client.ListRepositoriesByOwner(t.Context(), "acme")
	require.NoError(err)
	assert.Equal([]string{"app-only"}, repoNames(repos))

	// Owners with no route and no discovery client still fail closed.
	_, err = client.ListRepositoriesByOwner(t.Context(), "other")
	var missing *MissingRouteError
	require.ErrorAs(err, &missing)
}

func TestRoutedClientPreservesAndRoutesArchiveInventory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fallbackClient := &routeRecordingClient{marker: "fallback"}
	ownerClient := &routeRecordingClient{marker: "owner"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: fallbackClient},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: ownerClient},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	paged, ok := any(routed).(platformgithub.InventoryAPI)
	require.True(ok, "routed GitHub clients must preserve archive inventory support")
	if !ok {
		return
	}
	provider := newTestGitHubProvider(t, "github.com", routed)
	assert.Equal(platform.ArchiveCapabilities{
		HistoricalIssues: true, HistoricalMergeRequests: true,
		OrdinaryComments: true, SubmittedReviews: true, InlineReviewComments: true,
	}, provider.Capabilities().Archive)

	issues, next, exhausted, err := paged.ListInventoryIssuesPage(
		t.Context(), "acme", "widget", "created", "after-1", "since-1",
	)
	require.NoError(err)
	require.Len(issues, 1)
	assert.Equal("owner", issues[0].GetTitle())
	assert.Equal("next-owner", next)
	assert.False(exhausted)
	pulls, hasMore, err := paged.ListInventoryPullRequestsPage(
		t.Context(), "acme", "widget", "updated", 3,
	)
	require.NoError(err)
	require.Len(pulls, 1)
	assert.Equal("owner", pulls[0].GetTitle())
	assert.True(hasMore)
	assert.Equal([]string{
		"inventory-issues:acme/widget:created:after-1:since-1",
		"inventory-pulls:acme/widget:updated:3",
	}, ownerClient.calls)
	assert.Empty(fallbackClient.calls)

	unsupportedRouter, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: &mockClient{}},
	)
	require.NoError(err)
	unsupported, err := NewRoutedClient(unsupportedRouter)
	require.NoError(err)
	unsupportedPaged, ok := any(unsupported).(platformgithub.InventoryAPI)
	require.True(ok)
	if !ok {
		return
	}
	_, _, _, err = unsupportedPaged.ListInventoryIssuesPage(
		t.Context(), "acme", "widget", "created", "", "",
	)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	var platformErr *platform.Error
	require.ErrorAs(err, &platformErr)
	assert.Equal(
		string(platform.ArchiveCapabilityHistoricalIssues), platformErr.Capability,
	)
}

func TestRoutedClientUsesDedicatedArchiveRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	normal := &routeRecordingClient{marker: "normal"}
	archive := &routeRecordingClient{marker: "archive"}
	router, err := NewHostRouter("github.com", &Route{
		Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: normal,
		Fetcher:       &GraphQLFetcher{},
		ArchiveKey:    RouteKey{Host: "github.com", Owner: "acme"},
		ArchiveClient: archive, ArchiveFetcher: &GraphQLFetcher{},
		ArchiveReadIdentity: IdentityKey{Host: "github.com", Principal: "installation:2"},
	})
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	_, err = routed.GetRepository(t.Context(), "acme", "widget")
	require.NoError(err)
	_, err = routed.GetRepository(WithArchiveSyncBudget(t.Context()), "acme", "widget")
	require.NoError(err)
	assert.Equal([]string{"get:acme/widget"}, normal.calls)
	assert.Equal([]string{"get:acme/widget"}, archive.calls)

	assert.Equal(
		IdentityKey{Host: "github.com", Principal: "installation:2"},
		mustArchiveIdentity(t, router, "acme", "widget"),
	)
}

func TestRoutedClientUsesDedicatedArchiveRouteForOwnerDiscovery(t *testing.T) {
	require := require.New(t)
	normal := &routeRecordingClient{marker: "normal"}
	archive := &routeRecordingClient{marker: "archive"}
	router, err := NewHostRouter("github.com", &Route{
		Key:           RouteKey{Host: "github.com", Owner: "acme"},
		Client:        normal,
		ArchiveKey:    RouteKey{Host: "github.com", Owner: "acme"},
		ArchiveClient: archive,
	})
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)

	repos, err := routed.ListRepositoriesByOwner(
		WithArchiveSyncBudget(t.Context()), "acme",
	)
	require.NoError(err)
	require.Len(repos, 1)
	assert.Equal(t, "archive", repos[0].GetName())
	assert.Empty(t, normal.calls,
		"archive owner discovery must not spend the ordinary route")
}

func TestNewHostRouterDeduplicatesSharedArchiveRoutes(t *testing.T) {
	require := require.New(t)
	archive := &routeRecordingClient{marker: "archive"}
	router, err := NewHostRouter(
		"github.com",
		&Route{
			Key:           RouteKey{Host: "github.com", Owner: "acme"},
			Client:        &routeRecordingClient{marker: "owner"},
			ArchiveKey:    RouteKey{Host: "github.com", Owner: "acme"},
			ArchiveClient: archive,
		},
		&Route{
			Key:           RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			Client:        &routeRecordingClient{marker: "repo"},
			ArchiveKey:    RouteKey{Host: "github.com", Owner: "acme"},
			ArchiveClient: archive,
		},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	_, err = routed.GetRepository(
		WithArchiveSyncBudget(t.Context()), "acme", "widget",
	)
	require.NoError(err)
	require.Len(archive.calls, 1)
}

func mustArchiveIdentity(t *testing.T, router *HostRouter, owner, name string) IdentityKey {
	t.Helper()
	require := require.New(t)
	identity, err := router.ArchiveIdentityForRepo(owner, name)
	require.NoError(err)
	return identity
}

func TestRoutedClientWithoutFallbackRejectsOwnerlessAPIs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ownerClient := &routeRecordingClient{marker: "owner"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Client: ownerClient},
	)
	require.NoError(err)
	client, err := NewRoutedClient(router)
	require.NoError(err)

	repo, err := client.GetRepository(t.Context(), "acme", "widget")
	require.NoError(err)
	assert.Equal("owner", repo.GetName())
	_, _, err = client.ListNotifications(t.Context(), NotificationListOptions{})
	require.Error(err)
	require.ErrorContains(err, "ownerless request")
}

func TestSyncerFetcherForSelectsRepositoryRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fallbackFetcher := &GraphQLFetcher{}
	ownerFetcher := &GraphQLFetcher{}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Fetcher: fallbackFetcher},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme"}, Fetcher: ownerFetcher},
	)
	require.NoError(err)
	syncer := &Syncer{
		routers: map[string]*HostRouter{"github.com": router},
	}

	assert.Same(ownerFetcher, syncer.fetcherFor(RepoRef{
		Owner: "ACME", Name: "widget", PlatformHost: "github.com",
	}))
	assert.Same(fallbackFetcher, syncer.fetcherFor(RepoRef{
		Owner: "other", Name: "widget", PlatformHost: "github.com",
	}))
}

func TestSyncerSelectsIdentityScopedTrackersAndBudgetsForRepo(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	user123 := IdentityKey{Host: "github.com", Principal: "user:123"}
	user456 := IdentityKey{Host: "github.com", Principal: "user:456"}
	installation := IdentityKey{Host: "github.com", Principal: "installation:789"}
	gql123 := NewRateTracker(database, "github.com", "user:123", "graphql")
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-a"}, Fetcher: &GraphQLFetcher{rateTracker: gql123}, ReadIdentity: user123, WriteIdentity: user123},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-b"}, Fetcher: &GraphQLFetcher{rateTracker: gql123}, ReadIdentity: user123, WriteIdentity: user123},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-c"}, ReadIdentity: user456, WriteIdentity: user456},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-d"}, ReadIdentity: installation, WriteIdentity: user123},
	)
	require.NoError(err)
	rest123 := NewRateTracker(database, "github.com", "user:123", "rest")
	rest456 := NewRateTracker(database, "github.com", "user:456", "rest")
	appREST := NewRateTracker(database, "github.com", "installation:789", "rest")
	budget123 := NewSyncBudget(100)
	budget456 := NewSyncBudget(100)
	appBudget := NewSyncBudget(100)
	syncer := &Syncer{
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"):         rest123,
			RateBucketKey("github", "github.com", "user:456"):         rest456,
			RateBucketKey("github", "github.com", "installation:789"): appREST,
		},
		writeRateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"): rest123,
		},
		writeGQLRateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"): gql123,
		},
		budgets: map[string]*SyncBudget{
			RateBucketKey("github", "github.com", "user:123"):         budget123,
			RateBucketKey("github", "github.com", "user:456"):         budget456,
			RateBucketKey("github", "github.com", "installation:789"): appBudget,
		},
	}
	repoA := RepoRef{Owner: "org-a", Name: "one", PlatformHost: "github.com"}
	repoB := RepoRef{Owner: "org-b", Name: "two", PlatformHost: "github.com"}
	repoC := RepoRef{Owner: "org-c", Name: "three", PlatformHost: "github.com"}
	repoD := RepoRef{Owner: "org-d", Name: "four", PlatformHost: "github.com"}

	gotA, ok := syncer.RateTrackerForRepo(repoA, "rest")
	require.True(ok)
	gotB, ok := syncer.RateTrackerForRepo(repoB, "rest")
	require.True(ok)
	gotC, ok := syncer.RateTrackerForRepo(repoC, "rest")
	require.True(ok)
	gotD, ok := syncer.RateTrackerForRepo(repoD, "rest")
	require.True(ok)
	assert.Same(rest123, gotA)
	assert.Same(rest123, gotB)
	assert.Same(rest456, gotC)
	assert.Same(appREST, gotD)
	writeD, ok := syncer.WriteRateTrackerForRepo(repoD, "rest")
	require.True(ok)
	assert.Same(rest123, writeD)
	writeGQLD, ok := syncer.WriteRateTrackerForRepo(repoD, "graphql")
	require.True(ok)
	assert.Same(gql123, writeGQLD)
	budgetA, ok := syncer.BudgetForRepo(repoA)
	require.True(ok)
	budgetB, ok := syncer.BudgetForRepo(repoB)
	require.True(ok)
	budgetC, ok := syncer.BudgetForRepo(repoC)
	require.True(ok)
	budgetD, ok := syncer.BudgetForRepo(repoD)
	require.True(ok)
	assert.Same(budget123, budgetA)
	assert.Same(budget123, budgetB)
	assert.Same(budget456, budgetC)
	assert.Same(appBudget, budgetD)
	gqlTrackers := syncer.GQLRateTrackers()
	assert.Same(gql123, gqlTrackers[gql123.BucketKey()])
}

func TestSyncerDoesNotFallBackToReadTrackerWithoutWriteIdentity(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:789"}
	router, err := NewHostRouter(
		"github.com",
		&Route{
			Key:          RouteKey{Host: "github.com", Owner: "org-app"},
			ReadIdentity: appIdentity,
		},
	)
	require.NoError(err)
	appREST := NewRateTracker(database, "github.com", "installation:789", "rest")
	syncer := &Syncer{
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "installation:789"): appREST,
		},
	}
	repo := RepoRef{Owner: "org-app", Name: "one", PlatformHost: "github.com"}

	_, ok := syncer.WriteRateTrackerForRepo(repo, "rest")
	require.False(ok)
	_, ok = syncer.WriteIdentityForRepo(repo)
	require.False(ok)
}

func TestSyncerNotificationAdmissionRejectsMissingWriteIdentity(t *testing.T) {
	require := require.New(t)
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:789"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-app"}, ReadIdentity: appIdentity},
	)
	require.NoError(err)
	syncer := &Syncer{routers: map[string]*HostRouter{"github.com": router}}

	err = syncer.ensureNotificationBudget(
		RepoRef{Owner: "org-app", Name: "one", PlatformHost: "github.com"},
		&routeRecordingClient{}, 1,
	)
	require.Error(err)
	require.ErrorContains(err, "no startup-resolved write identity")
}

func TestSyncerNotificationAdmissionUsesRepositoryWriteIdentity(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	user123 := IdentityKey{Host: "github.com", Principal: "user:123"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: &routeRecordingClient{marker: "fallback"}},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-a"}, Client: &routeRecordingClient{marker: "owner"}, ReadIdentity: IdentityKey{Host: "github.com", Principal: "installation:789"}, WriteIdentity: user123},
	)
	require.NoError(err)
	writeRT := NewRateTracker(database, "github.com", "user:123", "rest")
	writeBudget := NewSyncBudget(1)
	writeBudgetWindow, spent := writeBudget.TrySpend(1)
	require.True(spent)
	syncer := &Syncer{
		routers: map[string]*HostRouter{"github.com": router},
		writeRateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"): writeRT,
		},
		budgets: map[string]*SyncBudget{
			RateBucketKey("github", "github.com", "user:123"): writeBudget,
		},
	}

	err = syncer.ensureNotificationBudget(
		RepoRef{Owner: "org-a", Name: "one", PlatformHost: "github.com"},
		&routeRecordingClient{}, 1,
	)
	require.Error(err)
	require.ErrorContains(err, "sync budget exhausted")

	writeBudget.Refund(writeBudgetWindow, 1)
	writeRT.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 0, Reset: time.Now().Add(time.Hour),
	})
	err = syncer.ensureNotificationBudget(
		RepoRef{Owner: "org-a", Name: "one", PlatformHost: "github.com"},
		&routeRecordingClient{marker: "fallback"}, 1,
	)
	require.Error(err)
	require.ErrorContains(err, "rate reserve exhausted")
}

func TestSyncerNotificationIdentityAdmissionUsesSplitCredentials(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	readIdentity := IdentityKey{
		Host: "github.com", Principal: "installation:789",
	}
	writeIdentity := IdentityKey{Host: "github.com", Principal: "user:123"}
	router, err := NewHostRouter("github.com", &Route{
		Key:          RouteKey{Host: "github.com", Owner: "org-a"},
		ReadIdentity: readIdentity, WriteIdentity: writeIdentity,
	})
	require.NoError(err)
	readBudget := NewSyncBudget(0)
	writeBudget := NewSyncBudget(10)
	syncer := &Syncer{
		routers: map[string]*HostRouter{"github.com": router},
		budgets: map[string]*SyncBudget{
			RateBucketKey("github", "github.com", "installation:789"): readBudget,
			RateBucketKey("github", "github.com", "user:123"):         writeBudget,
		},
	}
	repo := RepoRef{Owner: "org-a", Name: "one", PlatformHost: "github.com"}

	err = syncer.ensureNotificationIdentityBudget(
		repo, &routeRecordingClient{},
	)
	require.ErrorContains(err, "sync budget exhausted")

	readBudget = NewSyncBudget(10)
	writeBudget = NewSyncBudget(0)
	syncer.budgets = map[string]*SyncBudget{
		RateBucketKey("github", "github.com", "installation:789"): readBudget,
		RateBucketKey("github", "github.com", "user:123"):         writeBudget,
	}
	err = syncer.ensureNotificationIdentityBudget(
		repo, &routeRecordingClient{},
	)
	require.ErrorContains(err, "sync budget exhausted")

	readBudget = NewSyncBudget(10)
	writeBudget = NewSyncBudget(10)
	syncer.budgets = map[string]*SyncBudget{
		RateBucketKey("github", "github.com", "installation:789"): readBudget,
		RateBucketKey("github", "github.com", "user:123"):         writeBudget,
	}
	readTracker := NewRateTracker(
		database, "github.com", "installation:789", "rest",
	)
	readTracker.UpdateFromRate(Rate{
		Limit: 5000, Remaining: 0, Reset: time.Now().Add(time.Hour),
	})
	syncer.rateTrackers = map[string]*RateTracker{
		RateBucketKey("github", "github.com", "installation:789"): readTracker,
	}
	err = syncer.ensureNotificationIdentityBudget(
		repo, &routeRecordingClient{},
	)
	require.ErrorContains(err, "rate reserve exhausted")

	syncer.rateTrackers = nil
	registry := NewQuotaRegistry()
	registry.UpdateSnapshot(readIdentity, QuotaResourceREST, Rate{
		Limit: 5000, Remaining: RateReserveBuffer,
		Reset: time.Now().Add(time.Hour),
	})
	syncer.SetQuotaRegistry(registry)
	err = syncer.ensureNotificationIdentityBudget(
		repo, &routeRecordingClient{},
	)
	require.ErrorContains(err, "read rate reserve exhausted")
}

func TestSyncerRefreshesRateSnapshotsPerIdentityRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	user123 := IdentityKey{Host: "github.com", Principal: "user:123"}
	user456 := IdentityKey{Host: "github.com", Principal: "user:456"}
	rest123 := NewRateTracker(database, "github.com", "user:123", "rest")
	gql123 := NewRateTracker(database, "github.com", "user:123", "graphql")
	rest456 := NewRateTracker(database, "github.com", "user:456", "rest")
	gql456 := NewRateTracker(database, "github.com", "user:456", "graphql")
	client123 := &routeRecordingClient{snapshot: &platformgithub.RateLimitSnapshot{
		Core:    &Rate{Limit: 5000, Remaining: 4100},
		GraphQL: &Rate{Limit: 5000, Remaining: 4200},
	}}
	client456 := &routeRecordingClient{snapshot: &platformgithub.RateLimitSnapshot{
		Core:    &Rate{Limit: 5000, Remaining: 3100},
		GraphQL: &Rate{Limit: 5000, Remaining: 3200},
	}}
	router, err := NewHostRouter(
		"github.com",
		&Route{
			Key:    RouteKey{Host: "github.com", Owner: "org-a"},
			Client: client123, Fetcher: &GraphQLFetcher{rateTracker: gql123},
			ReadIdentity: user123,
		},
		&Route{
			Key:    RouteKey{Host: "github.com", Owner: "org-c"},
			Client: client456, Fetcher: &GraphQLFetcher{rateTracker: gql456},
			ReadIdentity: user456,
		},
	)
	require.NoError(err)
	syncer := &Syncer{
		clients: registryFromGitHubClients(map[string]Client{"github.com": client123}),
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"): rest123,
			RateBucketKey("github", "github.com", "user:456"): rest456,
		},
		rateLimitSnapshotRefresh: make(map[string]time.Time),
	}

	syncer.RefreshRateLimitSnapshots(t.Context())

	assert.Equal(4100, rest123.Remaining())
	assert.Equal(4200, gql123.Remaining())
	assert.Equal(3100, rest456.Remaining())
	assert.Equal(3200, gql456.Remaining())
	assert.Equal([]string{"snapshot"}, client123.calls)
	assert.Equal([]string{"snapshot"}, client456.calls)
}

func TestSyncerRateSnapshotTriesHealthyRouteForSharedIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	identity := IdentityKey{Host: "github.com", Principal: "user:123"}
	rest := NewRateTracker(database, "github.com", "user:123", "rest")
	failed := &routeRecordingClient{snapshotErr: fmt.Errorf("expired token")}
	healthy := &routeRecordingClient{snapshot: &platformgithub.RateLimitSnapshot{
		Core: &Rate{Limit: 5000, Remaining: 4200},
	}}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: failed, ReadIdentity: identity},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-b"}, Client: healthy, ReadIdentity: identity},
	)
	require.NoError(err)
	syncer := &Syncer{
		clients: registryFromGitHubClients(map[string]Client{"github.com": healthy}),
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"): rest,
		},
		rateLimitSnapshotRefresh: make(map[string]time.Time),
	}

	syncer.RefreshRateLimitSnapshots(t.Context())

	assert.Equal(4200, rest.Remaining())
	assert.Equal([]string{"snapshot"}, failed.calls)
	assert.Equal([]string{"snapshot"}, healthy.calls)
}

func TestSyncerRefreshesWriteOnlyIdentityRateSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:789"}
	userIdentity := IdentityKey{Host: "github.com", Principal: "user:123"}
	appREST := NewRateTracker(database, "github.com", "installation:789", "rest")
	userREST := NewRateTracker(database, "github.com", "user:123", "rest")
	userGQL := NewRateTracker(database, "github.com", "user:123", "graphql")
	appClient := &routeRecordingClient{snapshot: &platformgithub.RateLimitSnapshot{
		Core: &Rate{Limit: 5000, Remaining: 4100},
	}}
	userClient := &routeRecordingClient{snapshot: &platformgithub.RateLimitSnapshot{
		Core:    &Rate{Limit: 5000, Remaining: 3100},
		GraphQL: &Rate{Limit: 5000, Remaining: 3200},
	}}
	router, err := NewHostRouter(
		"github.com",
		&Route{
			Key:    RouteKey{Host: "github.com", Owner: "org-d"},
			Client: appClient, ReadIdentity: appIdentity,
			WriteSnapshotClient: userClient, WriteIdentity: userIdentity,
		},
	)
	require.NoError(err)
	syncer := &Syncer{
		clients: registryFromGitHubClients(map[string]Client{"github.com": appClient}),
		routers: map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "installation:789"): appREST,
		},
		writeRateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"): userREST,
		},
		writeGQLRateTrackers: map[string]*RateTracker{
			RateBucketKey("github", "github.com", "user:123"): userGQL,
		},
		rateLimitSnapshotRefresh: make(map[string]time.Time),
	}

	syncer.RefreshRateLimitSnapshots(t.Context())

	assert.Equal(4100, appREST.Remaining())
	assert.Equal(3100, userREST.Remaining())
	assert.Equal(3200, userGQL.Remaining())
	assert.Equal([]string{"snapshot"}, appClient.calls)
	assert.Equal([]string{"snapshot"}, userClient.calls)
}

func TestSyncerSharedIdentityResetResetsEveryRouteBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	identity := IdentityKey{Host: "github.com", Principal: "user:123"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-a"}, ReadIdentity: identity},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-b"}, ReadIdentity: identity},
	)
	require.NoError(err)
	bucket := RateBucketKey("github", "github.com", "user:123")
	rt := NewRateTracker(database, "github.com", "user:123", "rest")
	budget := NewSyncBudget(100)
	syncer := NewSyncer(nil, database, nil, nil, time.Minute,
		map[string]*RateTracker{bucket: rt}, map[string]*SyncBudget{bucket: budget})
	syncer.SetGitHubRouters(map[string]*HostRouter{"github.com": router})
	initialReset := time.Now().Add(-time.Minute)
	rt.UpdateFromSnapshot(Rate{Limit: 5000, Remaining: 4900, Reset: initialReset})
	budget.Spend(20)

	rt.UpdateFromSnapshot(Rate{
		Limit: 5000, Remaining: 4900, Reset: initialReset.Add(time.Hour),
	})

	budgetA, ok := syncer.BudgetForRepo(RepoRef{Owner: "org-a", Name: "one", PlatformHost: "github.com"})
	require.True(ok)
	budgetB, ok := syncer.BudgetForRepo(RepoRef{Owner: "org-b", Name: "two", PlatformHost: "github.com"})
	require.True(ok)
	assert.Same(budgetA, budgetB)
	assert.Zero(budgetA.Spent())
}

func TestSyncerAppPauseDoesNotDelayPATIdentityOnSameHost(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	appIdentity := IdentityKey{Host: "github.com", Principal: "installation:789"}
	userIdentity := IdentityKey{Host: "github.com", Principal: "user:456"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-app"}, ReadIdentity: appIdentity},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-pat"}, ReadIdentity: userIdentity},
	)
	require.NoError(err)
	appBucket := RateBucketKey("github", "github.com", "installation:789")
	userBucket := RateBucketKey("github", "github.com", "user:456")
	appRT := NewRateTracker(database, "github.com", "installation:789", "rest")
	userRT := NewRateTracker(database, "github.com", "user:456", "rest")
	appRT.UpdateFromRate(Rate{Limit: 5000, Remaining: 0, Reset: time.Now().Add(time.Hour)})
	syncer := &Syncer{
		routers:      map[string]*HostRouter{"github.com": router},
		rateTrackers: map[string]*RateTracker{appBucket: appRT, userBucket: userRT},
	}

	eligible := syncer.repoEligibility([]RepoRef{
		{Owner: "org-app", Name: "one", PlatformHost: "github.com"},
		{Owner: "org-pat", Name: "two", PlatformHost: "github.com"},
	}, map[string]time.Time{})
	assert.False(eligible[appBucket])
	assert.True(eligible[userBucket])
}

func TestSyncerCadenceKeysAreIdentityScoped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	identity123 := IdentityKey{Host: "github.com", Principal: "user:123"}
	identity456 := IdentityKey{Host: "github.com", Principal: "user:456"}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-a"}, ReadIdentity: identity123},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-b"}, ReadIdentity: identity123},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-c"}, ReadIdentity: identity456},
	)
	require.NoError(err)
	syncer := &Syncer{routers: map[string]*HostRouter{"github.com": router}}

	keyA, err := syncer.bucketKeyForRepo(RepoRef{Owner: "org-a", Name: "one", PlatformHost: "github.com"}, false)
	require.NoError(err)
	keyB, err := syncer.bucketKeyForRepo(RepoRef{Owner: "org-b", Name: "two", PlatformHost: "github.com"}, false)
	require.NoError(err)
	keyC, err := syncer.bucketKeyForRepo(RepoRef{Owner: "org-c", Name: "three", PlatformHost: "github.com"}, false)
	require.NoError(err)
	assert.Equal(keyA, keyB)
	assert.NotEqual(keyA, keyC)

	syncer.nextSyncAfter = map[string]time.Time{keyA: time.Now().Add(time.Minute)}
	syncer.nextWatchSyncAfter = map[string]time.Time{keyC: time.Now().Add(time.Minute)}
	assert.Contains(syncer.nextSyncAfter, keyB)
	assert.NotContains(syncer.nextSyncAfter, keyC)
	assert.Contains(syncer.nextWatchSyncAfter, keyC)
	assert.NotContains(syncer.nextWatchSyncAfter, keyA)
}

func TestRoutedClientKeepsDistinctIdentityGoGitHubCachesIsolated(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	resetAt := time.Now().Add(time.Hour).Unix()
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := githubOwnerFromPath(r.URL.Path)
		calls[owner]++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		remaining := "4999"
		if owner == "org-a" {
			remaining = "0"
		}
		w.Header().Set("X-RateLimit-Remaining", remaining)
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
		_, _ = w.Write([]byte(`{"id":1,"name":"repo","owner":{"login":"` + owner + `"}}`))
	}))
	defer server.Close()

	clientA, err := NewClient(
		testTokenSource("pat-a"), "github.com",
		NewRateTracker(database, "github.com", "user:1", "rest"), nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)
	clientB, err := NewClient(
		testTokenSource("pat-b"), "github.com",
		NewRateTracker(database, "github.com", "user:2", "rest"), nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-a"}, Client: clientA},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-b"}, Client: clientB},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)

	_, err = routed.GetRepository(t.Context(), "org-a", "repo")
	require.NoError(err)
	_, err = routed.GetRepository(t.Context(), "org-b", "repo")
	require.NoError(err)
	assert.Equal(1, calls["org-a"])
	assert.Equal(1, calls["org-b"])
}

func TestRoutedClientsForSameIdentityUpdateSharedRateTracker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	resetAt := time.Now().Add(time.Hour).Unix()
	remaining := map[string]string{"org-a": "4998", "org-b": "4997"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := githubOwnerFromPath(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", remaining[owner])
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt, 10))
		_, _ = w.Write([]byte(`{"id":1,"name":"repo","owner":{"login":"` + owner + `"}}`))
	}))
	defer server.Close()

	shared := NewRateTracker(database, "github.com", "user:123", "rest")
	clientA, err := NewClient(
		testTokenSource("pat-a"), "github.com", shared, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)
	clientB, err := NewClient(
		testTokenSource("pat-b"), "github.com", shared, nil,
		WithBaseURLForTesting(server.URL),
	)
	require.NoError(err)
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-a"}, Client: clientA},
		&Route{Key: RouteKey{Host: "github.com", Owner: "org-b"}, Client: clientB},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)

	_, err = routed.GetRepository(t.Context(), "org-a", "repo")
	require.NoError(err)
	_, err = routed.GetRepository(t.Context(), "org-b", "repo")
	require.NoError(err)
	assert.Equal(2, shared.RequestsThisHour())
	assert.Equal(4997, shared.Remaining())
	assert.Equal(5000, shared.RateLimit())
}

func TestHostRouterKeepsAuthorizationRoutesSeparateFromSharedIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	identity := IdentityKey{Host: "github.com", Principal: "user:123"}
	routeA := &Route{
		Key:          RouteKey{Host: "github.com", Owner: "org-a"},
		Client:       &routeRecordingClient{marker: "pat-a"},
		ReadIdentity: identity,
	}
	routeB := &Route{
		Key:          RouteKey{Host: "github.com", Owner: "org-b"},
		Client:       &routeRecordingClient{marker: "pat-b"},
		ReadIdentity: identity,
	}
	router, err := NewHostRouter("github.com", nil, routeA, routeB)
	require.NoError(err)

	gotA, err := router.RouteForOwner("org-a")
	require.NoError(err)
	gotB, err := router.RouteForOwner("org-b")
	require.NoError(err)
	assert.NotSame(gotA.Client, gotB.Client)
	assert.Equal(gotA.ReadIdentity, gotB.ReadIdentity)
}

func TestHostRouterRepoCredentialAliasFollowsRename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	router, err := NewHostRouter("github.com",
		&Route{
			Key:          RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			ReadIdentity: IdentityKey{Host: "github.com", Principal: "widget-bot"},
		},
		&Route{
			Key:          RouteKey{Host: "github.com"},
			ReadIdentity: IdentityKey{Host: "github.com", Principal: "fallback-bot"},
		},
	)
	require.NoError(err)

	router.RegisterRepoCredentialAlias("acme", "gadget",
		RouteKey{Host: "github.com", Owner: "acme", Name: "widget"}, "R_1")
	identity, err := router.ReadIdentityForRepo("acme", "gadget")
	require.NoError(err)
	assert.Equal("widget-bot", identity.Principal)

	// A second rename that targets the first alias still lands on the
	// configured route.
	router.RegisterRepoCredentialAlias("acme", "gizmo",
		RouteKey{Host: "github.com", Owner: "acme", Name: "gadget"}, "R_1")
	identity, err = router.ReadIdentityForRepo("acme", "gizmo")
	require.NoError(err)
	assert.Equal("widget-bot", identity.Principal)
}

func TestHostRouterArchiveAliasFallsBackWhenArchiveAppLosesCoverage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	normal := &routeRecordingClient{marker: "normal"}
	archive := &routeRecordingClient{marker: "archive"}
	router, err := NewHostRouter("github.com",
		&Route{
			Key:           RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			Client:        normal,
			ArchiveKey:    RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			ArchiveClient: archive,
		},
	)
	require.NoError(err)
	router.RegisterRepoCredentialAlias("acme", "gadget",
		RouteKey{Host: "github.com", Owner: "acme", Name: "widget"}, "R_1")
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	_, err = routed.GetRepository(
		WithArchiveSyncBudget(t.Context()), "acme", "gadget",
	)
	require.NoError(err)
	assert.Equal([]string{"get:acme/gadget"}, normal.calls)
	assert.Empty(archive.calls,
		"an archive route scoped to the old repository must not follow its alias")
}

func TestRegisterConfiguredRepoCredentialAliasesRoutesRenamedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	router, err := NewHostRouter("github.com",
		&Route{
			Key:          RouteKey{Host: "github.com", Owner: "acme", Name: "widget"},
			ReadIdentity: IdentityKey{Host: "github.com", Principal: "widget-bot"},
		},
	)
	require.NoError(err)

	RegisterConfiguredRepoCredentialAliases(
		map[string]*HostRouter{"github.com": router},
		config.Repo{Owner: "acme", Name: "widget"},
		[]RepoRef{{
			Platform: platform.KindGitHub, PlatformHost: "github.com",
			Owner: "acme", Name: "gadget", PlatformExternalID: "R_1",
		}},
	)

	identity, err := router.ReadIdentityForRepo("acme", "gadget")
	require.NoError(err,
		"startup resolution to a renamed route must reuse the configured credential")
	assert.Equal("widget-bot", identity.Principal)
}

func TestPublishResolvedRepositoryClearsDisplacedCredentialAlias(t *testing.T) {
	require := require.New(t)
	router, err := NewHostRouter("github.com",
		&Route{
			Key:          RouteKey{Host: "github.com", Owner: "acme", Name: "old-widget"},
			ReadIdentity: IdentityKey{Host: "github.com", Principal: "old-widget-bot"},
		},
	)
	require.NoError(err)
	// The displaced repository R_old was renamed onto acme/widget earlier
	// and aliased credential selection back to its configured route.
	router.RegisterRepoCredentialAlias("acme", "widget",
		RouteKey{Host: "github.com", Owner: "acme", Name: "old-widget"}, "R_old")

	syncer := &Syncer{routers: map[string]*HostRouter{"github.com": router}}
	replacement := RepoRef{
		Platform: platform.KindGitHub, PlatformHost: "github.com",
		Owner: "acme", Name: "widget", PlatformExternalID: "R_new",
	}
	syncer.publishResolvedRepository(replacement, replacement, true)

	_, err = router.ReadIdentityForRepo("acme", "widget")
	require.Error(err,
		"a replacement repository must not inherit the displaced repository's credential alias")
}
