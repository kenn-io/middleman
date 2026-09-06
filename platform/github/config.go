package github

import (
	"context"
	"errors"
	"net/http"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/platform"
)

// Authentication describes the caller's credential selection without owning it.
// Context supplies routing metadata for host-scoped reads and user-only writes.
type Authentication struct {
	Source             platform.CredentialSource
	InstallationActive func(owner string) bool
	CredentialKey      func() string
	Context            func(context.Context, string, bool) context.Context
}

// Progress is an optional caller-owned observation of a paginated request.
type Progress struct {
	Page func(records int, more bool)
	Done func()
}

func (p Progress) recordPage(records int, more bool) {
	if p.Page != nil {
		p.Page(records, more)
	}
}
func (p Progress) done() {
	if p.Done != nil {
		p.Done()
	}
}

// ClientConfig supplies transport and application policy explicitly. Constructing
// a Client performs no credential discovery, request, or background work.
type ClientConfig struct {
	Host                       string
	Read, Write, Notifications *http.Client
	MarkdownImages             *http.Client
	Clock                      func() time.Time
	Authentication             Authentication
	// APIBase, UploadBase and GraphQLEndpoint override transport endpoints only;
	// Host remains the provider identity.
	APIBase, UploadBase, GraphQLEndpoint  string
	ReadRate, WriteRate, NotificationRate platform.RateObserver
	GraphQLRate, WriteGraphQLRate         platform.RateObserver
	ViewerCacheTTL                        time.Duration
	ReadOnlyContext                       func(context.Context) bool
	GraphQLContext                        func(context.Context) context.Context
	InvalidateETags                       func(string, string, ...string)
	Progress                              func(owner, repository, kind string) Progress
	Warning                               func(string, ...any)
}

func NewClient(config ClientConfig) (*Client, error) {
	host := config.Host
	if host == "" {
		host = platform.DefaultGitHubHost
	}
	if config.Read == nil || config.Write == nil || config.Notifications == nil || config.Clock == nil {
		return nil, &platform.Error{Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitHub,
			PlatformHost: host, Err: errors.New("read, write and notification clients and a clock are required")}
	}
	base, uploads, graphQL := config.APIBase, config.UploadBase, config.GraphQLEndpoint
	if base == "" {
		if host == "github.com" {
			base = "https://api.github.com/"
		} else {
			base = "https://" + host + "/api/v3/"
		}
	}
	if uploads == "" {
		if host == "github.com" {
			uploads = "https://uploads.github.com/"
		} else {
			uploads = "https://" + host + "/api/uploads/"
		}
	}
	if graphQL == "" {
		if host == "github.com" {
			graphQL = "https://api.github.com/graphql"
		} else {
			graphQL = "https://" + host + "/api/graphql"
		}
	}
	newSDK := func(hc *http.Client) (*gh.Client, error) {
		return gh.NewClient(gh.WithHTTPClient(hc), gh.WithEnterpriseURLs(base, uploads))
	}
	read, err := newSDK(config.Read)
	if err != nil {
		return nil, err
	}
	write, err := newSDK(config.Write)
	if err != nil {
		return nil, err
	}
	notifications, err := newSDK(config.Notifications)
	if err != nil {
		return nil, err
	}
	graphQLContext := config.GraphQLContext
	if graphQLContext == nil {
		graphQLContext = func(ctx context.Context) context.Context { return ctx }
	}
	return &Client{
		gh: read, ghWrite: write, ghNotifications: notifications,
		httpClient: config.Read, httpWriteClient: config.Write, httpNotificationClient: config.Notifications,
		markdownImageHTTPClient: config.MarkdownImages, platformHost: host, graphQLEndpoint: graphQL,
		source: config.Authentication.Source, auth: config.Authentication, now: config.Clock,
		rateTracker: config.ReadRate, writeRateTracker: config.WriteRate, notificationRateTracker: config.NotificationRate,
		graphQLRateTracker: config.GraphQLRate, writeGraphQLRateTracker: config.WriteGraphQLRate,
		viewerCacheTTL: config.ViewerCacheTTL, readOnlyContext: config.ReadOnlyContext,
		graphQLContext: graphQLContext, invalidateETags: config.InvalidateETags,
		progressFactory: config.Progress, warning: config.Warning,
	}, nil
}

func (c *Client) authContext(ctx context.Context, owner string, mutation bool) context.Context {
	if c.auth.Context != nil {
		return c.auth.Context(ctx, owner, mutation)
	}
	return ctx
}
func (c *Client) progress(owner, repo, kind string) Progress {
	if c.progressFactory != nil {
		return c.progressFactory(owner, repo, kind)
	}
	return Progress{}
}
func (c *Client) warn(message string, args ...any) {
	if c.warning != nil {
		c.warning(message, args...)
	}
}

type unconditionalReadKey struct{}

// WithUnconditionalRead requests a complete body even if the caller's transport
// normally adds validators. Inventory and explicit detail refreshes use it.
func WithUnconditionalRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, unconditionalReadKey{}, true)
}
func UnconditionalRead(ctx context.Context) bool {
	value, _ := ctx.Value(unconditionalReadKey{}).(bool)
	return value
}
