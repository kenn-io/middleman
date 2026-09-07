package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.kenn.io/forge/platform"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/tokenauth"
)

// IdentityKey identifies the GitHub principal whose rate limit and sync budget
// a request consumes on one host.
type IdentityKey struct {
	Host      string
	Principal string
}

func (k IdentityKey) String() string {
	return strings.ToLower(strings.TrimSpace(k.Host)) + "\x00" +
		strings.TrimSpace(k.Principal)
}

// hostPrincipal is the principal used when no credential router resolved a
// GitHub identity, so one host-wide chain accounts for all of its traffic.
const hostPrincipal = "host"

// HostIdentity returns the fallback identity for host, used when startup
// configured no per-owner credential routes.
func HostIdentity(host string) IdentityKey {
	return IdentityKey{Host: normalizedPlatformHost(host), Principal: hostPrincipal}
}

// GitHubIdentity is the stable principal plus safe display metadata resolved
// for one credential route.
type GitHubIdentity struct {
	Key   IdentityKey
	Login string
}

func (i GitHubIdentity) Label() string {
	switch {
	case strings.HasPrefix(i.Key.Principal, "user:") && i.Login != "":
		return "GitHub user " + i.Login
	case strings.HasPrefix(i.Key.Principal, "installation:"):
		return "GitHub App installation " + strings.TrimPrefix(
			i.Key.Principal, "installation:",
		)
	default:
		return i.Key.Principal
	}
}

// IdentityResolver resolves a user credential to GitHub's immutable numeric
// account identity and returns the exact token accepted by that lookup.
type IdentityResolver interface {
	ResolvePAT(context.Context, string, tokenauth.Source) (GitHubIdentity, string, error)
}

var ErrIdentityChanged = errors.New("GitHub credential identity changed; restart required")

type identityBoundSource struct {
	source   tokenauth.Source
	host     string
	expected IdentityKey
	resolver IdentityResolver

	mu            sync.Mutex
	acceptedToken string
}

// BindSourceIdentity prevents a lazily reloaded PAT from moving a live route to
// a different GitHub user while its trackers and budget remain bound to the
// startup identity. The startup-verified token is accepted immediately; App
// reads pass through and later mutation/user token values are re-resolved only
// when they change.
func BindSourceIdentity(
	source tokenauth.Source,
	host string,
	expected IdentityKey,
	acceptedToken string,
	resolver IdentityResolver,
) tokenauth.Source {
	if source == nil || expected.Principal == "" || resolver == nil {
		return source
	}
	return &identityBoundSource{
		source: source, host: host, expected: expected, resolver: resolver,
		acceptedToken: acceptedToken,
	}
}

func (s *identityBoundSource) Token(ctx context.Context) (string, error) {
	token, err := s.source.Token(ctx)
	if err != nil {
		return token, err
	}
	if !tokenauth.IsMutationAuth(ctx) && s.source.Descriptor().HasActiveGitHubApp() {
		return token, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == s.acceptedToken {
		return token, nil
	}
	identity, _, err := s.resolver.ResolvePAT(
		ctx, s.host, staticTokenSource{token: token, desc: s.source.Descriptor()},
	)
	if err != nil {
		return "", err
	}
	if identity.Key != s.expected {
		return "", ErrIdentityChanged
	}
	s.acceptedToken = token
	return token, nil
}

func (s *identityBoundSource) Invalidate(rejectedToken string) {
	s.source.Invalidate(rejectedToken)
	s.mu.Lock()
	if s.acceptedToken == rejectedToken {
		s.acceptedToken = ""
	}
	s.mu.Unlock()
}

func (s *identityBoundSource) Descriptor() tokenauth.Descriptor {
	return s.source.Descriptor()
}

type staticTokenSource struct {
	token string
	desc  tokenauth.Descriptor
}

func (s staticTokenSource) Token(context.Context) (string, error) { return s.token, nil }
func (s staticTokenSource) Invalidate(string)                     {}
func (s staticTokenSource) Descriptor() tokenauth.Descriptor      { return s.desc }

type authenticatedUserLookup func(
	context.Context, string, tokenauth.Source,
) (*gh.User, error)

type identityRecordingSource struct {
	source tokenauth.Source

	mu    sync.Mutex
	token string
}

func (s *identityRecordingSource) Token(ctx context.Context) (string, error) {
	token, err := s.source.Token(ctx)
	if err != nil {
		return token, err
	}
	s.mu.Lock()
	s.token = token
	s.mu.Unlock()
	return token, nil
}

func (s *identityRecordingSource) Invalidate(rejectedToken string) {
	s.source.Invalidate(rejectedToken)
	s.mu.Lock()
	if s.token == rejectedToken {
		s.token = ""
	}
	s.mu.Unlock()
}

func (s *identityRecordingSource) Descriptor() tokenauth.Descriptor {
	return s.source.Descriptor()
}

func (s *identityRecordingSource) resolvedToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// HTTPIdentityResolver resolves PAT identity through GitHub's authenticated
// user API. Lookup is injectable so tests exercise identity semantics without
// exposing an arbitrary request URL seam.
type HTTPIdentityResolver struct {
	Lookup  authenticatedUserLookup
	Timeout time.Duration
}

const defaultIdentityLookupTimeout = 10 * time.Second

func (r HTTPIdentityResolver) ResolvePAT(
	ctx context.Context, host string, source tokenauth.Source,
) (GitHubIdentity, string, error) {
	if source == nil {
		return GitHubIdentity{}, "", fmt.Errorf("resolve GitHub identity for %s: nil token source", host)
	}
	lookup := lookupAuthenticatedUser
	if r.Lookup != nil {
		lookup = r.Lookup
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultIdentityLookupTimeout
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	recording := &identityRecordingSource{source: source}
	user, err := lookup(tokenauth.WithMutationAuth(lookupCtx), host, recording)
	if err != nil {
		return GitHubIdentity{}, "", fmt.Errorf(
			"resolve GitHub identity for %s via %s: %w",
			host, source.Descriptor().SafeString(), err,
		)
	}
	if user == nil || user.GetID() <= 0 {
		return GitHubIdentity{}, "", fmt.Errorf(
			"resolve GitHub identity for %s via %s: response lacks a positive numeric user id",
			host, source.Descriptor().SafeString(),
		)
	}
	verifiedToken := recording.resolvedToken()
	if verifiedToken == "" {
		return GitHubIdentity{}, "", fmt.Errorf(
			"resolve GitHub identity for %s via %s: authenticated lookup used no token",
			host, source.Descriptor().SafeString(),
		)
	}
	return GitHubIdentity{
		Key: IdentityKey{
			Host:      normalizedPlatformHost(host),
			Principal: fmt.Sprintf("user:%d", user.GetID()),
		},
		Login: strings.TrimSpace(user.GetLogin()),
	}, verifiedToken, nil
}

// InstallationIdentity returns the principal used by GitHub App installation
// access tokens.
func InstallationIdentity(host string, installationID int64) GitHubIdentity {
	return GitHubIdentity{Key: IdentityKey{
		Host:      normalizedPlatformHost(host),
		Principal: fmt.Sprintf("installation:%d", installationID),
	}}
}

func identityHTTPClientForHost(
	host string, source tokenauth.Source,
) *http.Client {
	origin := restAPIOriginForHost(host)
	return &http.Client{Transport: wrapPublicGitHubAPIGuard(platform.AuthTransport{
		Source:              source,
		Base:                http.DefaultTransport,
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
		AllowedOrigin:       origin,
	})}
}

func lookupAuthenticatedUser(
	ctx context.Context, host string, source tokenauth.Source,
) (*gh.User, error) {
	httpClient := identityHTTPClientForHost(host, source)
	opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(httpClient)}
	host = normalizedPlatformHost(host)
	if host != "github.com" {
		origin := "https://" + host
		opts = append(opts, gh.WithEnterpriseURLs(
			origin+"/api/v3/", origin+"/api/uploads/",
		))
	}
	client, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("create GitHub identity client: %w", err)
	}
	user, _, err := client.Users.Get(ctx, "")
	return user, err
}
