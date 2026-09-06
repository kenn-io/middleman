package gitea

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	giteasdk "code.gitea.io/sdk/gitea"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitealike"
	gitremote "go.kenn.io/kit/git/remote"
)

const minimumReviewThreadVersion = ">= 1.24.6"

type ClientOption func(*clientOptions)

type clientOptions struct {
	baseURL           string
	foregroundTimeout time.Duration
	rateTracker       platform.RateObserver
	transport         http.RoundTripper
	serverVersion     string
	allowInsecureHTTP bool
}

type provider = gitealike.Provider

type Client struct {
	host      string
	baseURL   string
	transport *transport
	*provider
	api               *giteasdk.Client
	foregroundTimeout time.Duration
	readReviewThreads bool
	allowInsecureHTTP bool
}

// WithBaseURL configures the API transport independently from the provider
// host, which remains the stable identity and credential-routing key.
func WithBaseURL(baseURL string, allowInsecure bool) ClientOption {
	return func(opts *clientOptions) {
		if strings.TrimSpace(baseURL) != "" {
			opts.baseURL = baseURL
		}
		opts.allowInsecureHTTP = allowInsecure
	}
}

func WithServerVersion(serverVersion string) ClientOption {
	return func(opts *clientOptions) {
		opts.serverVersion = serverVersion
	}
}

func WithForegroundTimeoutForTesting(timeout time.Duration) ClientOption {
	return func(opts *clientOptions) {
		opts.foregroundTimeout = timeout
	}
}

func WithRateTracker(rateTracker platform.RateObserver) ClientOption {
	return func(opts *clientOptions) {
		opts.rateTracker = rateTracker
	}
}

// WithTransport supplies the caller's HTTP transport, including admission.
func WithTransport(transport http.RoundTripper) ClientOption {
	return func(opts *clientOptions) { opts.transport = transport }
}

func NewClient(host string, source platform.CredentialSource, options ...ClientOption) (*Client, error) {
	opts := clientOptions{
		baseURL:           "https://" + strings.TrimRight(host, "/"),
		foregroundTimeout: 20 * time.Second,
	}
	for _, option := range options {
		option(&opts)
	}
	if opts.transport == nil {
		return nil, &platform.Error{Code: platform.ErrCodeInvalidArgument, Field: "transport"}
	}
	baseURL, err := validateBaseURL(opts.baseURL, opts.allowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	opts.baseURL = baseURL

	clientOptions := []giteasdk.ClientOption{
		giteasdk.SetUserAgent("kenn-forge"),
	}
	// An unknown version disables version-dependent advertised capabilities;
	// the SDK must not probe behind this constructor's caller.
	clientOptions = append(clientOptions, giteasdk.SetGiteaVersion(opts.serverVersion))
	httpTransport := opts.transport
	if opts.rateTracker != nil {
		httpTransport = &rateTrackingTransport{
			base:        httpTransport,
			rateTracker: opts.rateTracker,
		}
	}
	mergeability := gitealike.NewMergeableCache()
	httpTransport = &gitealike.MergeableCaptureTransport{
		Base:  httpTransport,
		Cache: mergeability,
	}
	mergeRejections := gitealike.NewMergeRejectionCapture()
	httpTransport = &gitealike.MergeRejectionCaptureTransport{
		Base:    httpTransport,
		Capture: mergeRejections,
	}
	httpTransport = &timelineLabelTransport{base: httpTransport}
	authRT := platform.AuthTransport{
		Source:              source,
		Base:                httpTransport,
		SetHeader:           platform.TokenAuthHeader,
		RetryOnUnauthorized: true,
		AllowedOrigin:       opts.baseURL,
	}
	apiHTTPClient := &http.Client{
		Timeout:   opts.foregroundTimeout,
		Transport: authRT,
	}
	clientOptions = append(clientOptions, giteasdk.SetHTTPClient(apiHTTPClient))

	api, err := giteasdk.NewClient(opts.baseURL, clientOptions...)
	if err != nil {
		return nil, err
	}
	readReviewThreads := opts.serverVersion != "" && api.CheckServerVersionConstraint(minimumReviewThreadVersion) == nil
	providerOptions := []gitealike.Option{gitealike.WithMutations()}
	if opts.serverVersion != "" {
		providerOptions = append(providerOptions, gitealike.WithReadActions())
	}
	transport := &transport{
		api:                api,
		httpClient:         apiHTTPClient,
		baseURL:            opts.baseURL,
		mergeability:       mergeability,
		mergeRejections:    mergeRejections,
		requestContextLock: make(chan struct{}, 1),
	}
	return &Client{
		host:              host,
		baseURL:           opts.baseURL,
		api:               api,
		transport:         transport,
		provider:          gitealike.NewProvider(platform.KindGitea, host, transport, providerOptions...),
		foregroundTimeout: opts.foregroundTimeout,
		readReviewThreads: readReviewThreads,
		allowInsecureHTTP: opts.allowInsecureHTTP,
	}, nil
}

func validateBaseURL(raw string, allowInsecure bool) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("gitea base URL must be an absolute http(s) URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("gitea base URL scheme must be http or https")
	}
	if u.User != nil {
		return "", fmt.Errorf("gitea base URL must not include user info")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("gitea base URL must not include a query string")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("gitea base URL must not include a fragment")
	}
	if u.Scheme == "http" && !allowInsecure {
		return "", fmt.Errorf("gitea base URL uses plain HTTP without an explicit insecure transport acknowledgement")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func (c *Client) GetRepository(
	ctx context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	repo, err := c.provider.GetRepository(ctx, ref)
	if err != nil {
		return platform.Repository{}, err
	}
	if err := c.validateRepositoryCloneURL(repo); err != nil {
		return platform.Repository{}, err
	}
	return repo, nil
}

func (c *Client) ListRepositories(
	ctx context.Context,
	owner string,
	opts platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	repos, err := c.provider.ListRepositories(ctx, owner, opts)
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if err := c.validateRepositoryCloneURL(repo); err != nil {
			return nil, err
		}
	}
	return repos, nil
}

func (c *Client) validateRepositoryCloneURL(repo platform.Repository) error {
	raw := strings.TrimSpace(repo.CloneURL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("gitea repository %q advertised a clone URL that is not absolute HTTP(S)", repo.Ref.DisplayName())
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("gitea repository %q advertised unsupported clone URL scheme %q", repo.Ref.DisplayName(), scheme)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("gitea repository %q advertised a clone URL with credentials, query, or fragment", repo.Ref.DisplayName())
	}
	if err := gitremote.ValidateRemoteIdentity(gitremote.Identity{
		Host: c.host, Owner: repo.Ref.Owner, Name: repo.Ref.Name,
	}, raw); err != nil {
		return fmt.Errorf("gitea repository %q clone transport is incompatible with configured platform identity: %w", repo.Ref.DisplayName(), err)
	}
	if scheme == "http" && !c.allowInsecureHTTP {
		return fmt.Errorf("gitea repository %q advertised a plain HTTP clone URL; set allow_insecure = true for platform %q only if sending credentials without TLS is intended", repo.Ref.DisplayName(), c.host)
	}
	return nil
}

func (c *Client) Platform() platform.Kind {
	return platform.KindGitea
}

// ServerVersion explicitly reads provider version metadata. Construction never
// probes the server. Pass this result to WithServerVersion before publishing
// the client to callers that use version-dependent capabilities.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var version string
	err := c.transport.withRequestContext(ctx, func() error {
		var err error
		version, _, err = c.api.ServerVersion()
		return err
	})
	return version, err
}

func (c *Client) Host() string {
	return c.host
}

func (c *Client) Capabilities() platform.Capabilities {
	caps := c.provider.Capabilities()
	if c.readReviewThreads {
		caps.ReadReviewThreads = true
		caps.Archive.InlineReviewComments = true
	}
	return caps
}

func (c *Client) AuthenticatedUser(
	ctx context.Context,
	ref platform.RepoRef,
) (string, error) {
	return c.provider.AuthenticatedUser(ctx, ref)
}

type transport struct {
	api                *giteasdk.Client
	httpClient         *http.Client
	baseURL            string
	mergeability       *gitealike.MergeableCache
	mergeRejections    *gitealike.MergeRejectionCapture
	requestContextLock chan struct{}
}

func (t *transport) getRepositoryRaw(
	ctx context.Context, owner, repo string,
) (*giteasdk.Repository, error) {
	var repository *giteasdk.Repository
	err := t.withRequestContext(ctx, func() error {
		var err error
		repository, _, err = t.api.GetRepo(owner, repo)
		return err
	})
	return repository, err
}

func (t *transport) withRequestContext(ctx context.Context, request func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case t.requestContextLock <- struct{}{}:
		defer func() { <-t.requestContextLock }()
	case <-ctx.Done():
		return ctx.Err()
	}

	t.api.SetContext(ctx)
	defer t.api.SetContext(context.Background())
	return request()
}

type rateTrackingTransport struct {
	base        http.RoundTripper
	rateTracker platform.RateObserver
}

func (t *rateTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if resp != nil && t.rateTracker != nil {
		t.rateTracker.RecordRequest()
		if rate, ok := gitealike.RateFromHeaders(resp.Header); ok {
			t.rateTracker.UpdateFromRate(rate)
		}
	}
	return resp, err
}
