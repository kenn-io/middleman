package github

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

// RouteKey identifies one configuration-bounded GitHub authorization route.
type RouteKey struct {
	Host  string
	Owner string
	Name  string
}

// Route holds the independent authorization client and GraphQL fetcher for a
// configured credential route, plus the identities whose capacity they use.
type Route struct {
	Key RouteKey
	// CredentialKey names the token source behind Client. Routes sharing a
	// credential share one source, which distinguishes many routes on one
	// credential from the "same account, different token" case where a broken
	// route must not stop the credential from refreshing through a healthy
	// one. Empty means unknown, and each route is then treated as distinct.
	CredentialKey string
	// WriteCredentialKey names the token source behind WriteSnapshotClient.
	// Mutations skip App candidates, so two routes on different Apps that fall
	// back to one PAT share a write credential even though their read chains
	// differ; the read key cannot stand in for it.
	WriteCredentialKey  string
	Client              Client
	DiscoveryClient     Client
	WriteSnapshotClient Client
	Fetcher             *GraphQLFetcher
	ReadIdentity        IdentityKey
	WriteIdentity       IdentityKey
	// ArchiveKey and ArchiveClient are optional. When present, requests
	// carrying the archive budget marker use the dedicated App route and its
	// independent quota instead of the ordinary sync route.
	ArchiveKey           RouteKey
	ArchiveClient        Client
	ArchiveFetcher       *GraphQLFetcher
	ArchiveCredentialKey string
	ArchiveReadIdentity  IdentityKey
}

// MissingRouteError reports that no configured route can serve an owner. It
// intentionally contains only host and repository identity, never credential
// material.
type MissingRouteError struct {
	Host  string
	Owner string
	Name  string
}

func (e *MissingRouteError) Error() string {
	target := strings.TrimSpace(e.Owner)
	if strings.TrimSpace(e.Name) != "" {
		target += "/" + strings.TrimSpace(e.Name)
	}
	if target == "" {
		target = "ownerless request"
	}
	return fmt.Sprintf("no GitHub credential route for %s on %s", target, e.Host)
}

// HostRouter selects exact-repository, owner, or fallback credential routes on
// one GitHub host.
type HostRouter struct {
	host            string
	fallback        *Route
	owners          map[string]*Route
	repos           map[string]*Route
	discoveryOwners map[string]Client
	archiveFallback *Route
	archiveOwners   map[string]*Route
	archiveRepos    map[string]*Route

	aliasMu     sync.RWMutex
	repoAliases map[string]repoCredentialAlias
}

// repoCredentialAlias maps a provider-resolved route back to the configured
// route whose credential it should use, recording which repository (by
// stable provider ID) created the mapping.
type repoCredentialAlias struct {
	configured     RouteKey
	providerRepoID string
}

func NewHostRouter(host string, routes ...*Route) (*HostRouter, error) {
	router := &HostRouter{
		host:            normalizedPlatformHost(host),
		owners:          make(map[string]*Route),
		repos:           make(map[string]*Route),
		discoveryOwners: make(map[string]Client),
		archiveOwners:   make(map[string]*Route),
		archiveRepos:    make(map[string]*Route),
	}
	for _, route := range routes {
		if route == nil {
			continue
		}
		routeHost := strings.TrimSpace(route.Key.Host)
		if routeHost == "" {
			routeHost = router.host
		} else {
			routeHost = normalizedPlatformHost(routeHost)
		}
		if routeHost != router.host {
			return nil, fmt.Errorf(
				"GitHub route host %s does not match router host %s",
				routeHost, router.host,
			)
		}
		if route.DiscoveryClient != nil {
			if strings.TrimSpace(route.Key.Owner) == "" {
				return nil, fmt.Errorf("GitHub discovery route on %s requires an owner", router.host)
			}
			key := ownerRouteMapKey(route.Key.Owner)
			if router.discoveryOwners[key] != nil {
				return nil, fmt.Errorf(
					"duplicate GitHub discovery route for %s on %s",
					route.Key.Owner, router.host,
				)
			}
			router.discoveryOwners[key] = route.DiscoveryClient
		}
		switch {
		case strings.TrimSpace(route.Key.Owner) == "":
			if router.fallback != nil {
				return nil, fmt.Errorf("duplicate GitHub fallback route for %s", router.host)
			}
			router.fallback = route
		case strings.TrimSpace(route.Key.Name) == "":
			key := ownerRouteMapKey(route.Key.Owner)
			if router.owners[key] != nil {
				return nil, fmt.Errorf("duplicate GitHub owner route for %s on %s", route.Key.Owner, router.host)
			}
			router.owners[key] = route
		default:
			key := repoRouteMapKey(route.Key.Owner, route.Key.Name)
			if router.repos[key] != nil {
				return nil, fmt.Errorf("duplicate GitHub repository route for %s/%s on %s", route.Key.Owner, route.Key.Name, router.host)
			}
			router.repos[key] = route
		}
		if route.ArchiveClient != nil {
			archiveKey := route.ArchiveKey
			if strings.TrimSpace(archiveKey.Host) == "" {
				archiveKey.Host = router.host
			}
			if normalizedPlatformHost(archiveKey.Host) != router.host {
				return nil, fmt.Errorf(
					"GitHub archive route host %s does not match router host %s",
					normalizedPlatformHost(archiveKey.Host), router.host,
				)
			}
			switch {
			case strings.TrimSpace(archiveKey.Owner) == "":
				if router.archiveFallback == nil {
					router.archiveFallback = route
				}
			case strings.TrimSpace(archiveKey.Name) == "":
				key := ownerRouteMapKey(archiveKey.Owner)
				if router.archiveOwners[key] == nil {
					router.archiveOwners[key] = route
				}
			default:
				key := repoRouteMapKey(archiveKey.Owner, archiveKey.Name)
				if router.archiveRepos[key] == nil {
					router.archiveRepos[key] = route
				}
			}
		}
	}
	return router, nil
}

func ownerRouteMapKey(owner string) string {
	return strings.ToLower(strings.TrimSpace(owner))
}

func repoRouteMapKey(owner, name string) string {
	return ownerRouteMapKey(owner) + "\x00" +
		strings.ToLower(strings.TrimSpace(name))
}

func (r *HostRouter) Fallback() (*Route, error) {
	if r != nil && r.fallback != nil {
		return r.fallback, nil
	}
	host := "github.com"
	if r != nil {
		host = r.host
	}
	return nil, &MissingRouteError{Host: host}
}

func (r *HostRouter) RouteForOwner(owner string) (*Route, error) {
	if r != nil {
		if route := r.owners[ownerRouteMapKey(owner)]; route != nil {
			return route, nil
		}
		if r.fallback != nil {
			return r.fallback, nil
		}
		return nil, &MissingRouteError{Host: r.host, Owner: owner}
	}
	return nil, &MissingRouteError{Host: "github.com", Owner: owner}
}

func (r *HostRouter) routeForOwnerContext(
	ctx context.Context, owner string,
) (*Route, error) {
	if IsArchiveSyncBudgetContext(ctx) && r != nil {
		if route := r.archiveOwners[ownerRouteMapKey(owner)]; route != nil &&
			routeArchiveCovers(route, owner, "") {
			return route, nil
		}
		if routeArchiveCovers(r.archiveFallback, owner, "") {
			return r.archiveFallback, nil
		}
	}
	return r.RouteForOwner(owner)
}

func (r *HostRouter) RouteForRepo(owner, name string) (*Route, error) {
	if r != nil {
		if route := r.repos[repoRouteMapKey(owner, name)]; route != nil {
			return route, nil
		}
		if configured, ok := r.repoCredentialAliasTarget(owner, name); ok {
			key := repoRouteMapKey(configured.Owner, configured.Name)
			if route := r.repos[key]; route != nil {
				return route, nil
			}
			// Fall through to owner and fallback resolution on the
			// configured identity, not the resolved one.
			owner = configured.Owner
		}
	}
	route, err := r.RouteForOwner(owner)
	if err != nil {
		if missing, ok := err.(*MissingRouteError); ok {
			missing.Name = name
		}
	}
	return route, err
}

func (r *HostRouter) repoCredentialAliasTarget(owner, name string) (RouteKey, bool) {
	r.aliasMu.RLock()
	defer r.aliasMu.RUnlock()
	target, ok := r.repoAliases[repoRouteMapKey(owner, name)]
	return target.configured, ok
}

func (r *HostRouter) ReadIdentityForRepo(owner, name string) (IdentityKey, error) {
	route, err := r.RouteForRepo(owner, name)
	if err != nil {
		return IdentityKey{}, err
	}
	return route.ReadIdentity, nil
}

// ArchiveIdentityForRepo returns the dedicated archive identity when one is
// configured, falling back to the normal read identity otherwise.
func (r *HostRouter) ArchiveIdentityForRepo(owner, name string) (IdentityKey, error) {
	route, err := r.routeForRepoMode(owner, name, true)
	if err != nil {
		return IdentityKey{}, err
	}
	if route.ArchiveReadIdentity.Principal != "" {
		return route.ArchiveReadIdentity, nil
	}
	return route.ReadIdentity, nil
}

// FetcherForRepo returns the GraphQL fetcher selected for a repository and
// request context. Archive-marked requests use the archive installation's
// fetcher when the route provides one.
func (r *HostRouter) FetcherForRepo(
	ctx context.Context, owner, name string,
) (*GraphQLFetcher, error) {
	route, err := r.routeForRepoMode(owner, name, IsArchiveSyncBudgetContext(ctx))
	if err != nil {
		return nil, err
	}
	if IsArchiveSyncBudgetContext(ctx) && route.ArchiveFetcher != nil {
		return route.ArchiveFetcher, nil
	}
	return route.Fetcher, nil
}

func (r *HostRouter) routeForRepoMode(owner, name string, archive bool) (*Route, error) {
	if !archive {
		return r.RouteForRepo(owner, name)
	}
	if r != nil {
		if route := r.archiveRepos[repoRouteMapKey(owner, name)]; route != nil && routeArchiveCovers(route, owner, name) {
			return route, nil
		}
		if route := r.archiveOwners[ownerRouteMapKey(owner)]; route != nil && routeArchiveCovers(route, owner, name) {
			return route, nil
		}
		if routeArchiveCovers(r.archiveFallback, owner, name) {
			return r.archiveFallback, nil
		}
	}
	// A normal route may be selected through a repository credential alias
	// after GitHub transfers the repository to another owner. Its archive App
	// route is scoped to the old owner/repository and must not follow that
	// alias unless the archive key still covers the resolved identity.
	route, err := r.RouteForRepo(owner, name)
	if err != nil || routeArchiveCovers(route, owner, name) {
		return route, err
	}
	return withoutArchiveRoute(route), err
}

func routeArchiveCovers(route *Route, owner, name string) bool {
	if route == nil || route.ArchiveClient == nil {
		return false
	}
	key := route.ArchiveKey
	if key.Owner != "" && !strings.EqualFold(key.Owner, owner) {
		return false
	}
	return key.Name == "" || strings.EqualFold(key.Name, name)
}

func withoutArchiveRoute(route *Route) *Route {
	if route == nil {
		return nil
	}
	copy := *route
	copy.ArchiveKey = RouteKey{}
	copy.ArchiveClient = nil
	copy.ArchiveFetcher = nil
	copy.ArchiveCredentialKey = ""
	copy.ArchiveReadIdentity = IdentityKey{}
	return &copy
}

func (r *HostRouter) WriteIdentityForRepo(owner, name string) (IdentityKey, error) {
	route, err := r.RouteForRepo(owner, name)
	if err != nil {
		return IdentityKey{}, err
	}
	return route.WriteIdentity, nil
}

// Routes returns each configured route once. The returned slice is detached
// from the router maps; route objects themselves are daemon-lifetime values.
func (r *HostRouter) Routes() []*Route {
	if r == nil {
		return nil
	}
	out := make([]*Route, 0, 1+len(r.owners)+len(r.repos))
	if r.fallback != nil {
		out = append(out, r.fallback)
	}
	ownerKeys := make([]string, 0, len(r.owners))
	for key := range r.owners {
		ownerKeys = append(ownerKeys, key)
	}
	sort.Strings(ownerKeys)
	for _, key := range ownerKeys {
		out = append(out, r.owners[key])
	}
	repoKeys := make([]string, 0, len(r.repos))
	for key := range r.repos {
		repoKeys = append(repoKeys, key)
	}
	sort.Strings(repoKeys)
	for _, key := range repoKeys {
		out = append(out, r.repos[key])
	}
	return out
}

// RoutedClient preserves the host-level Client contract while selecting a
// configuration-derived authorization route for owner-scoped operations.
type RoutedClient struct {
	Client
	routes *HostRouter
}

var (
	_ Client                              = (*RoutedClient)(nil)
	_ platformgithub.ViewerAPI            = (*RoutedClient)(nil)
	_ platformgithub.ViewerCacheKeyAPI    = (*RoutedClient)(nil)
	_ notificationThreadGetter            = (*RoutedClient)(nil)
	_ notificationReadRateReserveBypasser = (*RoutedClient)(nil)
	_ rateLimitSnapshotter                = (*RoutedClient)(nil)
	_ platformgithub.LabelAPI             = (*RoutedClient)(nil)
	_ platformgithub.AssigneeAPI          = (*RoutedClient)(nil)
	_ platformgithub.ReviewerAPI          = (*RoutedClient)(nil)
	_ platformgithub.InventoryAPI         = (*RoutedClient)(nil)
	_ markdownImageClient                 = (*RoutedClient)(nil)
	_ repoUserClient                      = (*RoutedClient)(nil)
	_ platformgithub.NativeStackClient    = (*RoutedClient)(nil)
)

func NewRoutedClient(routes *HostRouter) (*RoutedClient, error) {
	if routes == nil {
		return nil, &MissingRouteError{Host: "github.com"}
	}
	var fallback Client
	if route, err := routes.Fallback(); err == nil {
		fallback = route.Client
	}
	return &RoutedClient{Client: fallback, routes: routes}, nil
}

func (c *RoutedClient) routeForRepo(owner, repo string) (Client, error) {
	return c.routeForRepoContext(context.Background(), owner, repo)
}

func (c *RoutedClient) routeForRepoContext(ctx context.Context, owner, repo string) (Client, error) {
	route, err := c.routes.routeForRepoMode(owner, repo, IsArchiveSyncBudgetContext(ctx))
	if err != nil {
		return nil, err
	}
	client := route.Client
	if IsArchiveSyncBudgetContext(ctx) && route.ArchiveClient != nil {
		client = route.ArchiveClient
	}
	if client == nil {
		return nil, fmt.Errorf("GitHub route for %s/%s on %s has no client", owner, repo, c.routes.host)
	}
	return client, nil
}

func (c *RoutedClient) pageClientForRepo(
	ctx context.Context, owner, repo string, capability platform.ArchiveCapability,
) (platformgithub.InventoryAPI, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	paged, ok := client.(platformgithub.InventoryAPI)
	if !ok {
		return nil, platform.UnsupportedCapability(
			platform.KindGitHub, c.routes.host, string(capability),
		)
	}
	return paged, nil
}

func (c *RoutedClient) ListInventoryIssuesPage(
	ctx context.Context,
	owner string,
	repo string,
	sortBy string,
	cursor string,
	since string,
) ([]*gh.Issue, string, bool, error) {
	paged, err := c.pageClientForRepo(
		ctx, owner, repo, platform.ArchiveCapabilityHistoricalIssues,
	)
	if err != nil {
		return nil, "", false, err
	}
	return paged.ListInventoryIssuesPage(ctx, owner, repo, sortBy, cursor, since)
}

func (c *RoutedClient) ListInventoryPullRequestsPage(
	ctx context.Context,
	owner string,
	repo string,
	sortBy string,
	page int,
) ([]*gh.PullRequest, bool, error) {
	paged, err := c.pageClientForRepo(
		ctx, owner, repo, platform.ArchiveCapabilityHistoricalMergeRequests,
	)
	if err != nil {
		return nil, false, err
	}
	return paged.ListInventoryPullRequestsPage(ctx, owner, repo, sortBy, page)
}

// GetMarkdownImage fetches a private attachment with the credential that owns
// the repository the image is rendered in. Without this method the embedded
// Client interface hides GetMarkdownImage, so the provider's capability probe
// reports ReadMarkdownImages as unsupported and every routed GitHub host loses
// private image previews.
func (c *RoutedClient) GetMarkdownImage(
	ctx context.Context,
	owner, repo, sourceURL string,
) (platform.MarkdownImage, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return platform.MarkdownImage{}, err
	}
	reader, ok := client.(markdownImageClient)
	if !ok {
		return platform.MarkdownImage{}, platform.UnsupportedCapability(
			platform.KindGitHub, c.routes.host, "read_markdown_images",
		)
	}
	return reader.GetMarkdownImage(ctx, owner, repo, sourceURL)
}

// ListOpenPullRequestsWithNativeStackHints and ListNativeStacksPage carry the
// preview stack surface through routing. The embedded Client interface does not
// include NativeStackClient, so without these methods every routed GitHub host
// — which is every production host — fails the optional type assertion and the
// native stack preview silently never runs.
func (c *RoutedClient) ListOpenPullRequestsWithNativeStackHints(
	ctx context.Context,
	owner, repo string,
) ([]*gh.PullRequest, map[int]*platformgithub.NativeStackHint, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, nil, err
	}
	native, ok := client.(platformgithub.NativeStackClient)
	if !ok {
		prs, err := client.ListOpenPullRequests(ctx, owner, repo)
		return prs, nil, err
	}
	return native.ListOpenPullRequestsWithNativeStackHints(ctx, owner, repo)
}

func (c *RoutedClient) ListNativeStacksPage(
	ctx context.Context,
	owner, repo string,
	page int,
) (platformgithub.NativeStackPage, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return platformgithub.NativeStackPage{}, err
	}
	native, ok := client.(platformgithub.NativeStackClient)
	if !ok {
		return platformgithub.NativeStackPage{}, platform.UnsupportedCapability(
			platform.KindGitHub, c.routes.host, "read_native_stacks",
		)
	}
	return native.ListNativeStacksPage(ctx, owner, repo, page)
}

func (c *RoutedClient) fallbackClient() (Client, error) {
	route, err := c.routes.Fallback()
	if err != nil {
		return nil, err
	}
	if route.Client == nil {
		return nil, fmt.Errorf("GitHub fallback route for %s has no client", c.routes.host)
	}
	return route.Client, nil
}

func (c *RoutedClient) routeForOwnerContext(ctx context.Context, owner string) (Client, error) {
	route, err := c.routes.routeForOwnerContext(ctx, owner)
	if err != nil {
		return nil, err
	}
	if IsArchiveSyncBudgetContext(ctx) && route.ArchiveClient != nil {
		return route.ArchiveClient, nil
	}
	if route.Client == nil {
		return nil, fmt.Errorf("GitHub route for %s on %s has no client", owner, c.routes.host)
	}
	return route.Client, nil
}

// listRepositoriesByOwnerAcrossRoutes enumerates an owner's repositories
// across every credential that can see them. A PAT route lists everything the
// PAT can access but may lack access to repositories a selected-installation
// App covers; the App discovery client lists only its selection. Neither may
// shadow the other, so both configured sources are queried and merged, and a
// failure of either fails discovery rather than silently narrowing it.
func (c *RoutedClient) listRepositoriesByOwnerAcrossRoutes(
	ctx context.Context, owner string,
) ([]*gh.Repository, error) {
	var discovery Client
	if !IsArchiveSyncBudgetContext(ctx) && c != nil && c.routes != nil {
		discovery = c.routes.discoveryOwners[ownerRouteMapKey(owner)]
	}
	routed, routeErr := c.routeForOwnerContext(ctx, owner)
	if routeErr != nil {
		var missing *MissingRouteError
		if errors.As(routeErr, &missing) && discovery != nil {
			return discovery.ListRepositoriesByOwner(ctx, owner)
		}
		return nil, routeErr
	}
	repos, err := routed.ListRepositoriesByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	if discovery == nil || discovery == routed {
		return repos, nil
	}
	appRepos, err := discovery.ListRepositoriesByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	return mergeRepositoryLists(repos, appRepos), nil
}

func mergeRepositoryLists(lists ...[]*gh.Repository) []*gh.Repository {
	total := 0
	for _, list := range lists {
		total += len(list)
	}
	seenIDs := make(map[int64]struct{}, total)
	seenNames := make(map[string]struct{}, total)
	merged := make([]*gh.Repository, 0, total)
	for _, list := range lists {
		for _, repo := range list {
			if repo == nil {
				continue
			}
			if id := repo.GetID(); id != 0 {
				if _, ok := seenIDs[id]; ok {
					continue
				}
				seenIDs[id] = struct{}{}
			} else {
				name := strings.ToLower(strings.TrimSpace(repo.GetFullName()))
				if name == "" {
					name = strings.ToLower(strings.TrimSpace(
						repo.GetOwner().GetLogin() + "/" + repo.GetName(),
					))
				}
				if _, ok := seenNames[name]; ok {
					continue
				}
				seenNames[name] = struct{}{}
			}
			merged = append(merged, repo)
		}
	}
	return merged
}

func (c *RoutedClient) AuthenticatedViewerLoginForRepo(
	ctx context.Context, owner, name string,
) (string, error) {
	client, err := c.routeForRepoContext(ctx, owner, name)
	if err != nil {
		return "", err
	}
	viewer, ok := client.(platformgithub.ViewerAPI)
	if !ok {
		return "", fmt.Errorf("GitHub route for %s/%s does not resolve authenticated viewer", owner, name)
	}
	return viewer.AuthenticatedViewerLogin(ctx)
}

func (c *RoutedClient) AuthenticatedViewerCacheKeyForRepo(owner, name string) string {
	client, err := c.routeForRepo(owner, name)
	if err != nil {
		return ""
	}
	viewer, ok := client.(platformgithub.ViewerCacheKeyAPI)
	if !ok {
		return ""
	}
	return viewer.AuthenticatedViewerCacheKey()
}

func (c *RoutedClient) AuthenticatedViewerLogin(ctx context.Context) (string, error) {
	client, err := c.fallbackClient()
	if err != nil {
		return "", err
	}
	viewer, ok := client.(platformgithub.ViewerAPI)
	if !ok {
		return "", fmt.Errorf("GitHub fallback route for %s does not resolve authenticated viewer", c.routes.host)
	}
	return viewer.AuthenticatedViewerLogin(ctx)
}

func (c *RoutedClient) AuthenticatedViewerCacheKey() string {
	client, err := c.fallbackClient()
	if err != nil {
		return ""
	}
	viewer, ok := client.(platformgithub.ViewerCacheKeyAPI)
	if !ok {
		return ""
	}
	return viewer.AuthenticatedViewerCacheKey()
}

func (c *RoutedClient) GetNotificationThreadForRepo(
	ctx context.Context, owner, name, threadID string,
) (NotificationThread, error) {
	client, err := c.routeForRepoContext(ctx, owner, name)
	if err != nil {
		return NotificationThread{}, err
	}
	getter, ok := client.(notificationThreadGetter)
	if !ok {
		return NotificationThread{}, fmt.Errorf("GitHub route for %s/%s does not fetch notification threads", owner, name)
	}
	return getter.GetNotificationThread(ctx, threadID)
}

func (c *RoutedClient) GetNotificationThread(ctx context.Context, threadID string) (NotificationThread, error) {
	client, err := c.fallbackClient()
	if err != nil {
		return NotificationThread{}, err
	}
	getter, ok := client.(notificationThreadGetter)
	if !ok {
		return NotificationThread{}, fmt.Errorf("GitHub fallback route for %s does not fetch notification threads", c.routes.host)
	}
	return getter.GetNotificationThread(ctx, threadID)
}

func (c *RoutedClient) GetRateLimitSnapshot(ctx context.Context) (*platformgithub.RateLimitSnapshot, error) {
	client, err := c.fallbackClient()
	if err != nil {
		return nil, err
	}
	snapshotter, ok := client.(rateLimitSnapshotter)
	if !ok {
		return nil, fmt.Errorf("GitHub fallback route for %s does not fetch rate limit snapshots", c.routes.host)
	}
	return snapshotter.GetRateLimitSnapshot(ctx)
}

func (c *RoutedClient) InstallationAuthenticationActive() bool {
	client, err := c.fallbackClient()
	if err != nil {
		return false
	}
	bypasser, ok := client.(notificationReadRateReserveBypasser)
	return ok && bypasser.InstallationAuthenticationActive()
}

func (c *RoutedClient) GetUser(ctx context.Context, login string) (*gh.User, error) {
	client, err := c.fallbackClient()
	if err != nil {
		return nil, err
	}
	return client.GetUser(ctx, login)
}

// GetUserForRepo resolves a login with the repository's credential. Author
// display-name enrichment runs inside repository sync, so routing it by
// repository keeps owner-only and App-only configurations — which have no host
// fallback route — able to read /users at all, and bills the request to the
// identity whose repository triggered it.
// The owner must be supplied explicitly: `/users/{login}` carries no owner in
// its path, so the transport derives none and an App candidate — which only
// mints for a matching installation account — would be skipped in favor of the
// PAT, spending the user's budget on a read billed to the installation.
func (c *RoutedClient) GetUserForRepo(
	ctx context.Context, owner, repo, login string,
) (*gh.User, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetUser(tokenauth.WithGitHubOwner(ctx, owner), login)
}

func (c *RoutedClient) ListNotifications(ctx context.Context, opts NotificationListOptions) ([]NotificationThread, bool, error) {
	var client Client
	var err error
	if opts.RepoOwner != "" && opts.RepoName != "" {
		client, err = c.routeForRepoContext(ctx, opts.RepoOwner, opts.RepoName)
	} else {
		client, err = c.fallbackClient()
	}
	if err != nil {
		return nil, false, err
	}
	return client.ListNotifications(ctx, opts)
}

func (c *RoutedClient) MarkNotificationThreadReadForRepo(
	ctx context.Context, owner, name, threadID string,
) error {
	client, err := c.routeForRepoContext(ctx, owner, name)
	if err != nil {
		return err
	}
	return client.MarkNotificationThreadRead(ctx, threadID)
}

func (c *RoutedClient) MarkNotificationThreadRead(ctx context.Context, threadID string) error {
	client, err := c.fallbackClient()
	if err != nil {
		return err
	}
	return client.MarkNotificationThreadRead(ctx, threadID)
}

func (c *RoutedClient) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListOpenPullRequests(ctx, owner, repo)
}
func (c *RoutedClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetPullRequest(ctx, owner, repo, number)
}
func (c *RoutedClient) ListRepositoriesByOwner(ctx context.Context, owner string) ([]*gh.Repository, error) {
	return c.listRepositoriesByOwnerAcrossRoutes(ctx, owner)
}
func (c *RoutedClient) ListReleases(ctx context.Context, owner, repo string, perPage int) ([]*gh.RepositoryRelease, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListReleases(ctx, owner, repo, perPage)
}
func (c *RoutedClient) ListTags(ctx context.Context, owner, repo string, perPage int) ([]*gh.RepositoryTag, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListTags(ctx, owner, repo, perPage)
}
func (c *RoutedClient) ListOpenIssues(ctx context.Context, owner, repo string) ([]*gh.Issue, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListOpenIssues(ctx, owner, repo)
}
func (c *RoutedClient) GetIssue(ctx context.Context, owner, repo string, number int) (*gh.Issue, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetIssue(ctx, owner, repo, number)
}
func (c *RoutedClient) CreateIssue(ctx context.Context, owner, repo, title, body string) (*gh.Issue, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.CreateIssue(ctx, owner, repo, title, body)
}
func (c *RoutedClient) ListIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListIssueComments(ctx, owner, repo, number)
}
func (c *RoutedClient) ListIssueCommentsIfChanged(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListIssueCommentsIfChanged(ctx, owner, repo, number)
}
func (c *RoutedClient) ListReviews(ctx context.Context, owner, repo string, number int) ([]*gh.PullRequestReview, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListReviews(ctx, owner, repo, number)
}
func (c *RoutedClient) ListPullRequestReviewThreads(ctx context.Context, owner, repo string, number int) ([]platformgithub.PullRequestReviewThread, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListPullRequestReviewThreads(ctx, owner, repo, number)
}
func (c *RoutedClient) ListCommits(ctx context.Context, owner, repo string, number int) ([]*gh.RepositoryCommit, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListCommits(ctx, owner, repo, number)
}
func (c *RoutedClient) ListPullRequestTimelineEvents(ctx context.Context, owner, repo string, number int) ([]platformgithub.PullRequestTimelineEvent, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListPullRequestTimelineEvents(ctx, owner, repo, number)
}
func (c *RoutedClient) ListForcePushEvents(ctx context.Context, owner, repo string, number int) ([]platformgithub.ForcePushEvent, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListForcePushEvents(ctx, owner, repo, number)
}
func (c *RoutedClient) GetCombinedStatus(ctx context.Context, owner, repo, ref string) (*gh.CombinedStatus, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetCombinedStatus(ctx, owner, repo, ref)
}
func (c *RoutedClient) ListCheckRunsForRef(ctx context.Context, owner, repo, ref string) ([]*gh.CheckRun, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListCheckRunsForRef(ctx, owner, repo, ref)
}
func (c *RoutedClient) ListWorkflowRunsForHeadSHA(ctx context.Context, owner, repo, sha string) ([]*gh.WorkflowRun, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ListWorkflowRunsForHeadSHA(ctx, owner, repo, sha)
}
func (c *RoutedClient) ApproveWorkflowRun(ctx context.Context, owner, repo string, runID int64) error {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.ApproveWorkflowRun(ctx, owner, repo, runID)
}
func (c *RoutedClient) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) (*gh.IssueComment, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.CreateIssueComment(ctx, owner, repo, number, body)
}
func (c *RoutedClient) EditIssueComment(ctx context.Context, owner, repo string, commentID int64, body string) (*gh.IssueComment, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.EditIssueComment(ctx, owner, repo, commentID, body)
}
func (c *RoutedClient) DeleteIssueComment(ctx context.Context, owner, repo string, commentID int64) error {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.DeleteIssueComment(ctx, owner, repo, commentID)
}
func (c *RoutedClient) CreatePullRequestReviewCommentReply(ctx context.Context, owner, repo string, number int, body string, commentID int64) (*gh.PullRequestComment, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.CreatePullRequestReviewCommentReply(ctx, owner, repo, number, body, commentID)
}
func (c *RoutedClient) GetRepository(ctx context.Context, owner, repo string) (*gh.Repository, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetRepository(ctx, owner, repo)
}
func (c *RoutedClient) CreateReview(ctx context.Context, owner, repo string, number int, event, body string) (*gh.PullRequestReview, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.CreateReview(ctx, owner, repo, number, event, body)
}
func (c *RoutedClient) CreateReviewWithComments(ctx context.Context, owner, repo string, number int, event, body, commitID string, comments []*gh.DraftReviewComment) (*gh.PullRequestReview, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.CreateReviewWithComments(ctx, owner, repo, number, event, body, commitID, comments)
}
func (c *RoutedClient) ApplyReviewSuggestions(ctx context.Context, owner, repo string, number int, input platform.ApplyReviewSuggestionsInput) (*platform.AppliedReviewSuggestions, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ApplyReviewSuggestions(ctx, owner, repo, number, input)
}
func (c *RoutedClient) DismissReview(ctx context.Context, owner, repo string, number int, reviewID int64, message string) (*gh.PullRequestReview, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.DismissReview(ctx, owner, repo, number, reviewID, message)
}
func (c *RoutedClient) MarkPullRequestReadyForReview(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.MarkPullRequestReadyForReview(ctx, owner, repo, number)
}
func (c *RoutedClient) ConvertPullRequestToDraft(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.ConvertPullRequestToDraft(ctx, owner, repo, number)
}
func (c *RoutedClient) MergePullRequest(ctx context.Context, owner, repo string, number int, title, message, method, expectedSHA string) (*gh.PullRequestMergeResult, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.MergePullRequest(ctx, owner, repo, number, title, message, method, expectedSHA)
}
func (c *RoutedClient) EditPullRequest(ctx context.Context, owner, repo string, number int, opts platformgithub.EditPullRequestOpts) (*gh.PullRequest, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.EditPullRequest(ctx, owner, repo, number, opts)
}
func (c *RoutedClient) EditIssue(ctx context.Context, owner, repo string, number int, state string) (*gh.Issue, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.EditIssue(ctx, owner, repo, number, state)
}
func (c *RoutedClient) EditIssueContent(ctx context.Context, owner, repo string, number int, title, body *string) (*gh.Issue, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.EditIssueContent(ctx, owner, repo, number, title, body)
}
func (c *RoutedClient) ListPullRequestsPage(ctx context.Context, owner, repo, state string, page int) ([]*gh.PullRequest, bool, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, false, err
	}
	return client.ListPullRequestsPage(ctx, owner, repo, state, page)
}
func (c *RoutedClient) ListIssuesPage(ctx context.Context, owner, repo, state string, page int) ([]*gh.Issue, bool, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, false, err
	}
	return client.ListIssuesPage(ctx, owner, repo, state, page)
}
func (c *RoutedClient) InvalidateListETagsForRepo(owner, repo string, endpoints ...string) {
	client, err := c.routeForRepo(owner, repo)
	if err == nil {
		client.InvalidateListETagsForRepo(owner, repo, endpoints...)
	}
}

func (c *RoutedClient) ListRepoLabels(ctx context.Context, owner, repo string) ([]*gh.Label, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	labels, ok := client.(platformgithub.LabelAPI)
	if !ok {
		return nil, fmt.Errorf("GitHub route for %s/%s does not support labels", owner, repo)
	}
	return labels.ListRepoLabels(ctx, owner, repo)
}

func (c *RoutedClient) ReplaceIssueLabels(ctx context.Context, owner, repo string, number int, names []string) ([]*gh.Label, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	labels, ok := client.(platformgithub.LabelAPI)
	if !ok {
		return nil, fmt.Errorf("GitHub route for %s/%s does not support labels", owner, repo)
	}
	return labels.ReplaceIssueLabels(ctx, owner, repo, number, names)
}

func (c *RoutedClient) ReplaceIssueAssignees(ctx context.Context, owner, repo string, number int, usernames []string) (*gh.Issue, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	assignees, ok := client.(platformgithub.AssigneeAPI)
	if !ok {
		return nil, fmt.Errorf("GitHub route for %s/%s does not support assignees", owner, repo)
	}
	return assignees.ReplaceIssueAssignees(ctx, owner, repo, number, usernames)
}

func (c *RoutedClient) RequestPullRequestReviewers(ctx context.Context, owner, repo string, number int, usernames []string) (*gh.PullRequest, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	reviewers, ok := client.(platformgithub.ReviewerAPI)
	if !ok {
		return nil, fmt.Errorf("GitHub route for %s/%s does not support reviewers", owner, repo)
	}
	return reviewers.RequestPullRequestReviewers(ctx, owner, repo, number, usernames)
}

func (c *RoutedClient) RemovePullRequestReviewers(ctx context.Context, owner, repo string, number int, usernames []string) error {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return err
	}
	reviewers, ok := client.(platformgithub.ReviewerAPI)
	if !ok {
		return fmt.Errorf("GitHub route for %s/%s does not support reviewers", owner, repo)
	}
	return reviewers.RemovePullRequestReviewers(ctx, owner, repo, number, usernames)
}

func (c *RoutedClient) GetPullRequestIfChanged(ctx context.Context, owner, repo string, number int, etag string) (*gh.PullRequest, string, bool, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, "", false, err
	}
	conditional, ok := client.(conditionalPullRequestGetter)
	if !ok {
		pull, getErr := client.GetPullRequest(ctx, owner, repo, number)
		return pull, "", false, getErr
	}
	return conditional.GetPullRequestIfChanged(ctx, owner, repo, number, etag)
}

func (c *RoutedClient) GetIssueIfChanged(ctx context.Context, owner, repo string, number int, etag string) (*gh.Issue, string, bool, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, "", false, err
	}
	conditional, ok := client.(conditionalIssueGetter)
	if !ok {
		issue, getErr := client.GetIssue(ctx, owner, repo, number)
		return issue, "", false, getErr
	}
	return conditional.GetIssueIfChanged(ctx, owner, repo, number, etag)
}

func (c *RoutedClient) ListIssueTimelineEvents(ctx context.Context, owner, repo string, number int) ([]platformgithub.PullRequestTimelineEvent, error) {
	client, err := c.routeForRepoContext(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	lister, ok := client.(issueTimelineLister)
	if !ok {
		return nil, fmt.Errorf("GitHub route for %s/%s does not support issue timeline events", owner, repo)
	}
	return lister.ListIssueTimelineEvents(ctx, owner, repo, number)
}

// RegisterRepoCredentialAlias routes credential selection for a
// provider-resolved owner/name onto the configured repository identity it
// replaced. Provider APIs keep receiving the resolved names; only credential
// selection follows the configured route. Alias chains flatten, so repeated
// renames still resolve to the original configured identity. providerRepoID
// records which repository the alias belongs to, so a replacement repository
// reusing the route can displace it.
func (r *HostRouter) RegisterRepoCredentialAlias(
	owner, name string, configured RouteKey, providerRepoID string,
) {
	if r == nil {
		return
	}
	key := repoRouteMapKey(owner, name)
	r.aliasMu.Lock()
	defer r.aliasMu.Unlock()
	if target, ok := r.repoAliases[repoRouteMapKey(configured.Owner, configured.Name)]; ok {
		configured = target.configured
	}
	if key == repoRouteMapKey(configured.Owner, configured.Name) {
		return
	}
	if r.repoAliases == nil {
		r.repoAliases = make(map[string]repoCredentialAlias)
	}
	r.repoAliases[key] = repoCredentialAlias{
		configured:     configured,
		providerRepoID: providerRepoID,
	}
}

// ClearDisplacedRepoCredentialAlias removes a credential alias recorded by a
// different repository than the one now occupying the route, so a
// replacement repository cannot inherit the displaced repository's
// credential. An unknown occupant identity leaves the alias untouched.
func (r *HostRouter) ClearDisplacedRepoCredentialAlias(
	owner, name, providerRepoID string,
) {
	if r == nil || providerRepoID == "" {
		return
	}
	key := repoRouteMapKey(owner, name)
	r.aliasMu.Lock()
	defer r.aliasMu.Unlock()
	alias, ok := r.repoAliases[key]
	if !ok || alias.providerRepoID == providerRepoID {
		return
	}
	delete(r.repoAliases, key)
}
