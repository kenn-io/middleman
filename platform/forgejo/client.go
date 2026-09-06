package forgejo

import (
	"context"
	"net/http"
	"strings"
	"time"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitealike"
)

type ClientOption func(*clientOptions)

type clientOptions struct {
	baseURL           string
	foregroundTimeout time.Duration
	rateTracker       platform.RateObserver
	transport         http.RoundTripper
	serverVersion     string
}

type provider = gitealike.Provider

type Client struct {
	host      string
	baseURL   string
	transport *transport
	*provider
	api               *forgejosdk.Client
	foregroundTimeout time.Duration
}

func WithBaseURLForTesting(baseURL string) ClientOption {
	return func(opts *clientOptions) {
		opts.baseURL = strings.TrimRight(baseURL, "/")
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

// WithServerVersion supplies version metadata already observed by the caller.
func WithServerVersion(version string) ClientOption {
	return func(opts *clientOptions) { opts.serverVersion = version }
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

	clientOptions := []forgejosdk.ClientOption{
		forgejosdk.SetUserAgent("kenn-forge"),
	}
	clientOptions = append(clientOptions, forgejosdk.SetForgejoVersion(opts.serverVersion))
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
	clientOptions = append(clientOptions, forgejosdk.SetHTTPClient(apiHTTPClient))

	api, err := forgejosdk.NewClient(opts.baseURL, clientOptions...)
	if err != nil {
		return nil, err
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
		host:      host,
		baseURL:   opts.baseURL,
		api:       api,
		transport: transport,
		provider: gitealike.NewProvider(
			platform.KindForgejo,
			host,
			transport,
			gitealike.WithReadActions(),
			gitealike.WithMutations(),
		),
		foregroundTimeout: opts.foregroundTimeout,
	}, nil
}

func (c *Client) Platform() platform.Kind {
	return platform.KindForgejo
}

// ServerVersion reads version metadata under the caller's operation context.
// Construction does not query or guess a server version.
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
	caps.ReviewDraftMutation = true
	caps.ReadReviewThreads = true
	caps.Archive.InlineReviewComments = true
	caps.NativeMultilineRanges = false
	return caps
}

func (c *Client) AuthenticatedUser(
	ctx context.Context,
	ref platform.RepoRef,
) (string, error) {
	return c.provider.AuthenticatedUser(ctx, ref)
}

type transport struct {
	api                *forgejosdk.Client
	httpClient         *http.Client
	baseURL            string
	mergeability       *gitealike.MergeableCache
	mergeRejections    *gitealike.MergeRejectionCapture
	requestContextLock chan struct{}
}

func (t *transport) getRepositoryRaw(
	ctx context.Context, owner, repo string,
) (*forgejosdk.Repository, error) {
	var repository *forgejosdk.Repository
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
