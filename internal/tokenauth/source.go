package tokenauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

var ErrMissingToken = errors.New("missing provider token")

type GitHubCLIRunner func(context.Context, string) (string, error)

// GitHubAppMinter exchanges a github_app candidate (app id, private
// key path, installation id, host) for an installation access token
// and its expiry. Installation tokens live one hour; the managed
// source caches them and re-mints ahead of expiry.
type GitHubAppMinter func(context.Context, Candidate) (string, time.Time, error)

type Options struct {
	GitHubCLI GitHubCLIRunner
	GitHubApp GitHubAppMinter
}

type Source interface {
	Token(context.Context) (string, error)
	// Invalidate evicts only cache state that produced rejectedToken.
	Invalidate(rejectedToken string)
	Descriptor() Descriptor
}

type ManagedSource struct {
	mu        sync.Mutex
	desc      Descriptor
	options   Options
	ghToken   string
	ghCached  bool
	appTokens *githubAppTokenStore
}

type githubAppTokenCache struct {
	token       string
	exp         time.Time
	err         error
	retryAt     time.Time
	mintDone    chan struct{}
	invalidated bool
}

// githubAppTokenStore is shared by every managed source in one SourceSet.
// Repository-exact authorization routes can therefore reuse the one-hour token
// minted for their common App installation without weakening route selection.
type githubAppTokenStore struct {
	mu     sync.Mutex
	tokens map[Candidate]*githubAppTokenCache
	now    func() time.Time
}

func newGitHubAppTokenStore() *githubAppTokenStore {
	return &githubAppTokenStore{
		tokens: make(map[Candidate]*githubAppTokenCache),
		now:    time.Now,
	}
}

const (
	githubAppMintRetryDefault = 5 * time.Second
	githubAppMintRetryMax     = time.Hour
)

type retryDeadlineError interface {
	RetryDeadline(time.Time) time.Time
}

func githubAppMintRetryDeadline(err, callerErr error, now time.Time) time.Time {
	if err == nil || (callerErr != nil && errors.Is(err, callerErr)) {
		return time.Time{}
	}
	var retryErr retryDeadlineError
	if !errors.As(err, &retryErr) {
		return now.Add(githubAppMintRetryDefault)
	}
	retryAt := retryErr.RetryDeadline(now)
	if !retryAt.After(now) {
		return now.Add(githubAppMintRetryDefault)
	}
	maxRetryAt := now.Add(githubAppMintRetryMax)
	if retryAt.After(maxRetryAt) {
		return maxRetryAt
	}
	return retryAt
}

func (s *githubAppTokenStore) resolve(
	ctx context.Context,
	candidate Candidate,
	minter GitHubAppMinter,
) (string, time.Time, error) {
	key := canonicalCandidate(candidate)
	for {
		now := s.now()
		s.mu.Lock()
		cached := s.tokens[key]
		if cached != nil && cached.err != nil && now.Before(cached.retryAt) {
			err := cached.err
			s.mu.Unlock()
			return "", time.Time{}, err
		}
		if cached != nil && cached.token != "" &&
			now.Add(githubAppTokenRefreshSkew).Before(cached.exp) {
			token, exp := cached.token, cached.exp
			s.mu.Unlock()
			return token, exp, nil
		}
		if cached != nil && cached.mintDone != nil {
			done := cached.mintDone
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", time.Time{}, ctx.Err()
			case <-done:
				s.mu.Lock()
				if cached.invalidated {
					s.mu.Unlock()
					continue
				}
				token, exp, err := cached.token, cached.exp, cached.err
				s.mu.Unlock()
				return token, exp, err
			}
		}

		cached = &githubAppTokenCache{mintDone: make(chan struct{})}
		s.tokens[key] = cached
		s.mu.Unlock()

		token, exp, err := minter(ctx, candidate)
		now = s.now()
		s.mu.Lock()
		cached.token = token
		cached.exp = exp
		cached.err = err
		cached.retryAt = githubAppMintRetryDeadline(err, ctx.Err(), now)
		if err != nil && cached.retryAt.IsZero() {
			// Cancellation and deadline failures come from the winning
			// caller's context, not from GitHub: mark the entry so
			// waiters loop and re-mint with their own live contexts
			// instead of inheriting the error.
			cached.invalidated = true
		}
		close(cached.mintDone)
		cached.mintDone = nil
		if s.tokens[key] == cached && cached.invalidated {
			delete(s.tokens, key)
		}
		s.mu.Unlock()
		return token, exp, err
	}
}

func (s *githubAppTokenStore) evictCompleted(candidates []Candidate) {
	if s == nil {
		return
	}
	s.mu.Lock()
	now := s.now()
	for _, candidate := range candidates {
		if candidate.Kind != SourceKindGitHubApp {
			continue
		}
		key := canonicalCandidate(candidate)
		cached := s.tokens[key]
		if cached == nil {
			continue
		}
		// Other routes may share the same canonical candidate. Preserve
		// in-flight mints and active failure cooldowns because neither
		// contains a completed token from the replaced descriptor.
		if cached.mintDone != nil ||
			cached.err != nil && now.Before(cached.retryAt) {
			continue
		}
		cached.invalidated = true
		delete(s.tokens, key)
	}
	s.mu.Unlock()
}

func (s *githubAppTokenStore) invalidateToken(
	candidates []Candidate, rejectedToken string,
) {
	if s == nil || rejectedToken == "" {
		return
	}
	s.mu.Lock()
	for _, candidate := range candidates {
		if candidate.Kind != SourceKindGitHubApp {
			continue
		}
		key := canonicalCandidate(candidate)
		cached := s.tokens[key]
		if cached == nil || cached.token != rejectedToken {
			continue
		}
		cached.invalidated = true
		delete(s.tokens, key)
	}
	s.mu.Unlock()
}

func NewManagedSource(desc Descriptor, options Options) *ManagedSource {
	return newManagedSource(desc, options, newGitHubAppTokenStore())
}

func newManagedSource(
	desc Descriptor, options Options, appTokens *githubAppTokenStore,
) *ManagedSource {
	return &ManagedSource{
		desc: cloneDescriptor(desc), options: options, appTokens: appTokens,
	}
}

func (s *ManagedSource) Descriptor() Descriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneDescriptor(s.desc)
}

func (s *ManagedSource) Update(desc Descriptor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.desc.EqualSource(desc) {
		s.ghToken = ""
		s.ghCached = false
		s.appTokens.evictCompleted(s.desc.Candidates)
	}
	s.desc = cloneDescriptor(desc)
}

func (s *ManagedSource) Invalidate(rejectedToken string) {
	s.mu.Lock()
	if s.ghToken == rejectedToken {
		s.ghToken = ""
		s.ghCached = false
	}
	s.appTokens.invalidateToken(s.desc.Candidates, rejectedToken)
	s.mu.Unlock()
}

func (s *ManagedSource) Token(ctx context.Context) (string, error) {
	desc := s.Descriptor()
	if len(desc.Candidates) == 0 {
		return "", missingTokenError(desc)
	}
	for _, candidate := range desc.Candidates {
		token, used, err := s.tokenFromCandidate(ctx, candidate)
		if err != nil {
			return "", err
		}
		if used && token != "" {
			RegisterKnownSecret(token)
			return token, nil
		}
	}
	return "", missingTokenError(desc)
}

func (s *ManagedSource) tokenFromCandidate(
	ctx context.Context,
	candidate Candidate,
) (string, bool, error) {
	switch candidate.Kind {
	case SourceKindEnv:
		return strings.TrimSpace(os.Getenv(candidate.EnvName)), true, nil
	case SourceKindFile:
		data, err := os.ReadFile(candidate.FilePath)
		if err != nil {
			return "", false, fmt.Errorf("read token file %s: %w", candidate.FilePath, err)
		}
		return strings.TrimSpace(string(data)), true, nil
	case SourceKindGitHubCLI:
		return s.githubCLIToken(ctx, candidate.Host)
	case SourceKindGitHubApp:
		return s.githubAppToken(ctx, candidate)
	default:
		return "", false, nil
	}
}

// githubAppTokenRefreshSkew re-mints installation tokens this long
// before their recorded expiry so in-flight requests never race the
// one-hour token lifetime.
const githubAppTokenRefreshSkew = 5 * time.Minute

type mutationAuthCtxKey struct{}
type githubOwnerCtxKey struct{}

// WithMutationAuth marks ctx so token resolution skips github_app
// installation tokens and resolves the user's own credential chain
// (env PAT, token file, gh CLI) instead. Mutations sent with an app
// token are attributed to "<app>[bot]" on GitHub; kenn-forge keeps
// user-visible writes (merges, comments, state changes) on the user's
// credential so they stay attributed to the user. A host configured
// with only an app and no PAT chain fails mutation auth with a
// missing-token error rather than silently writing as the bot.
func WithMutationAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, mutationAuthCtxKey{}, true)
}

// IsMutationAuth reports whether ctx was marked by WithMutationAuth.
func IsMutationAuth(ctx context.Context) bool {
	marked, ok := ctx.Value(mutationAuthCtxKey{}).(bool)
	return ok && marked
}

// WithGitHubOwner scopes token resolution to a GitHub repository or account
// owner. GitHub App installation tokens are account-scoped, so a candidate for
// one installation account must not be used for another owner on the same host.
func WithGitHubOwner(ctx context.Context, owner string) context.Context {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return ctx
	}
	return context.WithValue(ctx, githubOwnerCtxKey{}, owner)
}

func githubOwnerFromContext(ctx context.Context) (string, bool) {
	owner, ok := ctx.Value(githubOwnerCtxKey{}).(string)
	return owner, ok && owner != ""
}

func (s *ManagedSource) githubAppToken(
	ctx context.Context,
	candidate Candidate,
) (string, bool, error) {
	// An app entry that is not installed yet cannot mint tokens; fall
	// through to the remaining candidates (PAT env vars, gh CLI) so a
	// half-configured app does not take the whole host offline.
	if candidate.InstallationID == 0 {
		return "", false, nil
	}
	// Mutations stay on the user's own credential chain so writes are
	// attributed to the user instead of the app bot.
	if IsMutationAuth(ctx) {
		return "", false, nil
	}
	if candidate.InstallationAccount != "" {
		owner, ok := githubOwnerFromContext(ctx)
		if !ok || !strings.EqualFold(owner, candidate.InstallationAccount) {
			return "", false, nil
		}
	}
	s.mu.Lock()
	minter := s.options.GitHubApp
	store := s.appTokens
	s.mu.Unlock()
	if minter == nil {
		return "", false, nil
	}
	token, _, err := store.resolve(ctx, candidate, minter)
	if err != nil {
		// Surface mint failures instead of silently degrading to the
		// PAT chain: the app exists precisely because the PAT budget
		// is exhausted, and a quiet fallback would hide broken keys.
		return "", false, fmt.Errorf(
			"mint github app installation token (%s): %w", candidate.SafeString(), err,
		)
	}
	if token == "" {
		return "", true, nil
	}
	return token, true, nil
}

func (s *ManagedSource) githubCLIToken(
	ctx context.Context,
	host string,
) (string, bool, error) {
	s.mu.Lock()
	if s.ghCached {
		token := s.ghToken
		s.mu.Unlock()
		return token, true, nil
	}
	runner := s.options.GitHubCLI
	s.mu.Unlock()
	if runner == nil {
		return "", true, nil
	}
	token, err := runner(ctx, host)
	if err != nil {
		return "", false, fmt.Errorf("github cli token for %s: %w", host, err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", true, nil
	}
	s.mu.Lock()
	s.ghToken = token
	s.ghCached = true
	s.mu.Unlock()
	return token, true, nil
}

func missingTokenError(desc Descriptor) error {
	return fmt.Errorf(
		"%w for %s host %s via %s",
		ErrMissingToken, desc.Key.Platform, desc.Key.Host, desc.SafeString(),
	)
}

type SourceSet struct {
	mu        sync.Mutex
	options   Options
	sources   map[Key]*ManagedSource
	appTokens *githubAppTokenStore
}

func NewSourceSet(options Options) *SourceSet {
	return &SourceSet{
		options: options, sources: make(map[Key]*ManagedSource),
		appTokens: newGitHubAppTokenStore(),
	}
}

func (s *SourceSet) Upsert(desc Descriptor) *ManagedSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.sources[desc.Key]; ok {
		existing.Update(desc)
		return existing
	}
	src := newManagedSource(desc, s.options, s.appTokens)
	s.sources[desc.Key] = src
	return src
}

func (s *SourceSet) Get(key Key) (*ManagedSource, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.sources[key]
	return src, ok
}

// ProbeBatch resolves descriptors with a SourceSet's options but a fresh
// installation-token cache scoped to one validation pass. Probes within the
// batch share mints, so multiple scoped routes covered by one GitHub App
// installation do not mint duplicates, while the first probe of each
// installation always re-mints: validation must observe a revoked
// installation or replaced private key instead of accepting a still-unexpired
// token from the live cache.
type ProbeBatch struct {
	options   Options
	appTokens *githubAppTokenStore
}

func (s *SourceSet) NewProbeBatch() *ProbeBatch {
	batch := &ProbeBatch{appTokens: newGitHubAppTokenStore()}
	if s != nil {
		s.mu.Lock()
		batch.options = s.options
		s.mu.Unlock()
	}
	return batch
}

// ProbeToken resolves desc without mutating live sources or the live
// installation-token cache.
func (b *ProbeBatch) ProbeToken(ctx context.Context, desc Descriptor) (string, error) {
	return newManagedSource(desc, b.options, b.appTokens).Token(ctx)
}

func (s *SourceSet) Keys() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]Key, 0, len(s.sources))
	for key := range s.sources {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if cmp := strings.Compare(a.Platform, b.Platform); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.Host, b.Host); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Scope, b.Scope)
	})
	return keys
}

func cloneDescriptor(desc Descriptor) Descriptor {
	desc.Candidates = append([]Candidate(nil), desc.Candidates...)
	return desc
}
